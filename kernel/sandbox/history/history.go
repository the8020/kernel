// Package history stores terminal sandbox metadata outside live state.
package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/sandbox/model"
)

const (
	DefaultRetention = 7 * 24 * time.Hour
	DefaultPageSize  = 100
	MaximumPageSize  = 1000
	bucketLayout     = "20060102T15"
	historyLayout    = "20060102T150405.000000000Z"
)

type Store struct {
	mu             sync.Mutex
	idsMu          sync.RWMutex
	root           string
	now            func() time.Time
	lastArchivedAt time.Time
	retainedIDs    map[string]bool
}

type Config struct {
	Root string
	Now  func() time.Time
}

type Record struct {
	SchemaVersion int                 `json:"schema_version"`
	HistoryID     string              `json:"history_id"`
	ArchivedAt    time.Time           `json:"archived_at"`
	ExpiresAt     time.Time           `json:"expires_at"`
	Reason        string              `json:"reason"`
	Spec          model.SandboxSpec   `json:"spec"`
	Status        model.SandboxStatus `json:"status"`
}

type Summary struct {
	HistoryID string `json:"history_id"`
	SandboxID string `json:"sandbox_id"`

	WorkloadType  model.WorkloadType `json:"workload_type"`
	State         model.SandboxState `json:"state"`
	Reason        string             `json:"reason,omitempty"`
	FailureReason string             `json:"failure_reason,omitempty"`
	ArchivedAt    time.Time          `json:"archived_at"`
	ExpiresAt     time.Time          `json:"expires_at"`
}

type Inspection struct {
	Record Record `json:"record"`
}

type Page struct {
	Sandboxes  []Summary `json:"sandboxes"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

func New(config Config) (*Store, error) {
	if config.Root == "" {
		return nil, errors.New("sandbox history root is required")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	for _, directory := range []string{config.Root, filepath.Join(config.Root, "buckets"), filepath.Join(config.Root, "ids")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create sandbox history: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("restrict sandbox history: %w", err)
		}
	}
	store := &Store{root: config.Root, now: config.Now, retainedIDs: map[string]bool{}}
	shards, err := os.ReadDir(filepath.Join(config.Root, "ids"))
	if err != nil {
		return nil, err
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(config.Root, "ids", shard.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				store.retainedIDs[entry.Name()] = true
			}
		}
	}
	return store, nil
}

// ContainsSandboxID uses the marker index loaded at startup and maintained by
// archive/cleanup. Admission paths never touch the filesystem.
func (s *Store) ContainsSandboxID(sandboxID string) (bool, error) {
	if err := validComponent(sandboxID); err != nil {
		return false, err
	}
	s.idsMu.RLock()
	retained := s.retainedIDs[sandboxID]
	s.idsMu.RUnlock()
	return retained, nil
}

// Archive writes immutable terminal metadata with its existing log reference,
// appends one bucket index entry, and finally publishes the direct ID marker.
func (s *Store) Archive(spec model.SandboxSpec, status model.SandboxStatus, reason string, retention time.Duration) (Record, error) {
	if err := validComponent(spec.SandboxID); err != nil {
		return Record{}, fmt.Errorf("sandbox ID: %w", err)
	}
	if retention <= 0 {
		return Record{}, errors.New("positive sandbox history retention is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if marker, err := os.ReadFile(s.markerPath(spec.SandboxID)); err == nil {
		s.setRetained(spec.SandboxID, true)
		existingID := strings.TrimSpace(string(marker))
		directory, pathErr := s.recordDirectory(existingID)
		if pathErr != nil {
			return Record{}, pathErr
		}
		var existing Record
		if err := readJSON(filepath.Join(directory, "metadata.json"), &existing); err != nil {
			return Record{}, err
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}

	archivedAt := s.now().UTC()
	if !archivedAt.After(s.lastArchivedAt) {
		archivedAt = s.lastArchivedAt.Add(time.Nanosecond)
	}
	s.lastArchivedAt = archivedAt
	historyID := archivedAt.Format(historyLayout) + "-" + spec.SandboxID
	record := Record{SchemaVersion: 2, HistoryID: historyID, ArchivedAt: archivedAt, ExpiresAt: archivedAt.Add(retention), Reason: reason, Spec: spec, Status: status}
	directory, err := s.recordDirectory(historyID)
	if err != nil {
		return Record{}, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Record{}, err
	}
	if err := writeJSON(filepath.Join(directory, "metadata.json"), record); err != nil {
		return Record{}, err
	}
	summary := Summary{HistoryID: historyID, SandboxID: spec.SandboxID, WorkloadType: spec.WorkloadType, State: status.ObservedState, Reason: reason, FailureReason: status.FailureReason, ArchivedAt: archivedAt, ExpiresAt: record.ExpiresAt}
	if err := appendJSONLine(s.indexPath(archivedAt), summary); err != nil {
		return Record{}, err
	}
	if err := writeFileAtomic(s.markerPath(spec.SandboxID), []byte(historyID+"\n")); err != nil {
		return Record{}, err
	}
	s.setRetained(spec.SandboxID, true)
	return record, nil
}

// List reads bucket indexes only when history is explicitly requested. It
// reads each index from the tail and stops as soon as the bounded page is full.
func (s *Store) List(limit int, before string) (Page, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaximumPageSize {
		return Page{}, fmt.Errorf("sandbox history limit cannot exceed %d", MaximumPageSize)
	}
	beforeBucket := ""
	if before != "" {
		archivedAt, err := historyTime(before)
		if err != nil {
			return Page{}, err
		}
		beforeBucket = archivedAt.Format(bucketLayout)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buckets, err := s.bucketNames()
	if err != nil {
		return Page{}, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(buckets)))
	page := Page{Sandboxes: make([]Summary, 0, limit)}
	for bucketIndex, bucket := range buckets {
		if beforeBucket != "" && bucket > beforeBucket {
			continue
		}
		if len(page.Sandboxes) == limit {
			break
		}
		remaining := limit - len(page.Sandboxes)
		items, more, err := readSummariesReverse(filepath.Join(s.root, "buckets", bucket, "index.jsonl"), remaining, before)
		if err != nil {
			return Page{}, err
		}
		page.Sandboxes = append(page.Sandboxes, items...)
		if len(page.Sandboxes) == limit && (more || bucketIndex < len(buckets)-1) {
			page.NextCursor = page.Sandboxes[len(page.Sandboxes)-1].HistoryID
		}
	}
	return page, nil
}

func (s *Store) Inspect(historyID string) (Inspection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.recordDirectory(historyID)
	if err != nil {
		return Inspection{}, err
	}
	var record Record
	if err := readJSON(filepath.Join(directory, "metadata.json"), &record); err != nil {
		return Inspection{}, err
	}
	return Inspection{Record: record}, nil
}

// Cleanup removes whole expired hour buckets. Marker deletion streams only the
// expired bucket's index, so cleanup cost is independent of the live catalog.
func (s *Store) Cleanup(retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, errors.New("positive sandbox history retention is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buckets, err := s.bucketNames()
	if err != nil {
		return 0, err
	}
	cutoff := s.now().UTC().Add(-retention).Truncate(time.Hour)
	removed := 0
	var joined error
	for _, bucket := range buckets {
		started, parseErr := time.ParseInLocation(bucketLayout, bucket, time.UTC)
		if parseErr != nil || !started.Before(cutoff) {
			continue
		}
		indexPath := filepath.Join(s.root, "buckets", bucket, "index.jsonl")
		file, openErr := os.Open(indexPath)
		if openErr == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				var summary Summary
				if json.Unmarshal(scanner.Bytes(), &summary) == nil {
					if removeErr := os.Remove(s.markerPath(summary.SandboxID)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
						joined = errors.Join(joined, removeErr)
					} else {
						s.setRetained(summary.SandboxID, false)
					}
					removed++
				}
			}
			joined = errors.Join(joined, scanner.Err(), file.Close())
		} else if !errors.Is(openErr, os.ErrNotExist) {
			joined = errors.Join(joined, openErr)
		}
		joined = errors.Join(joined, os.RemoveAll(filepath.Join(s.root, "buckets", bucket)))
	}
	return removed, joined
}

func (s *Store) setRetained(sandboxID string, retained bool) {
	s.idsMu.Lock()
	if retained {
		s.retainedIDs[sandboxID] = true
	} else {
		delete(s.retainedIDs, sandboxID)
	}
	s.idsMu.Unlock()
}

func (s *Store) bucketNames() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "buckets"))
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			result = append(result, entry.Name())
		}
	}
	return result, nil
}

func (s *Store) recordDirectory(historyID string) (string, error) {
	if err := validComponent(historyID); err != nil {
		return "", err
	}
	archivedAt, err := historyTime(historyID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, "buckets", archivedAt.UTC().Format(bucketLayout), shard(historyID), historyID), nil
}

func historyTime(historyID string) (time.Time, error) {
	if len(historyID) < len(historyLayout) {
		return time.Time{}, errors.New("invalid sandbox history ID")
	}
	archivedAt, err := time.Parse(historyLayout, historyID[:len(historyLayout)])
	if err != nil {
		return time.Time{}, errors.New("invalid sandbox history ID")
	}
	return archivedAt.UTC(), nil
}

func (s *Store) indexPath(archivedAt time.Time) string {
	return filepath.Join(s.root, "buckets", archivedAt.UTC().Format(bucketLayout), "index.jsonl")
}

func (s *Store) markerPath(sandboxID string) string {
	return filepath.Join(s.root, "ids", shard(sandboxID), sandboxID)
}

func shard(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:1])
}

func validComponent(value string) error {
	if value == "" || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) {
		return errors.New("invalid path component")
	}
	return nil
}

func appendJSONLine(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func readJSON(path string, output any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}

// readSummariesReverse performs bounded reverse line reads rather than loading
// an entire bucket index into memory.
func readSummariesReverse(path string, limit int, before string) ([]Summary, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	const blockSize int64 = 64 * 1024
	position := info.Size()
	pending := []byte{}
	result := make([]Summary, 0, limit)
	more := false
	for position > 0 && len(result) <= limit {
		readSize := blockSize
		if position < readSize {
			readSize = position
		}
		position -= readSize
		block := make([]byte, readSize)
		if _, err := file.ReadAt(block, position); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		data := append(block, pending...)
		lines := bytes.Split(data, []byte{'\n'})
		pending = append([]byte(nil), lines[0]...)
		for index := len(lines) - 1; index >= 1; index-- {
			line := bytes.TrimSpace(lines[index])
			if len(line) == 0 {
				continue
			}
			var summary Summary
			if err := json.Unmarshal(line, &summary); err != nil {
				return nil, false, err
			}
			if before != "" && summary.HistoryID >= before {
				continue
			}
			if len(result) == limit {
				more = true
				return result, more, nil
			}
			result = append(result, summary)
		}
	}
	if len(bytes.TrimSpace(pending)) > 0 && len(result) <= limit {
		var summary Summary
		if err := json.Unmarshal(bytes.TrimSpace(pending), &summary); err != nil {
			return nil, false, err
		}
		if before == "" || summary.HistoryID < before {
			if len(result) == limit {
				more = true
			} else {
				result = append(result, summary)
			}
		}
	}
	return result, more, nil
}
