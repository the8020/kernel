// Package logging owns slog output, rotation, retention, and runtime policy replacement.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"the8020/kernel/settings"
)

// ErrInitialization identifies failure to prepare a logging policy.
var ErrInitialization = errors.New("logging initialization failed")

// Policy is the complete active logging configuration.
type Policy struct {
	Enabled      bool
	SplitPeriod  string
	MaxFileSize  int64
	MaxTotalSize int64
}

type state struct {
	policy       Policy
	file         *os.File
	path         string
	size         int64
	periodEnd    time.Time
	sequence     uint64
	preparedPath string
}

// Manager is a concurrent rotating writer and runtime settings applier.
type Manager struct {
	mu        sync.Mutex
	directory string
	now       func() time.Time
	state     *state
	logger    *slog.Logger
}

// New validates and starts the initial logging policy.
func New(directory string, policy Policy) (*Manager, error) {
	manager := &Manager{directory: directory, now: func() time.Time { return time.Now().UTC() }}
	manager.logger = slog.New(slog.NewJSONHandler(manager, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prepared, err := manager.prepare(policy)
	if err != nil {
		return nil, err
	}
	manager.state = prepared
	return manager, nil
}

// Logger returns the kernel's single structured logger.
func (m *Manager) Logger() *slog.Logger { return m.logger }

// ActiveFile returns the current log path, or an empty string when disabled.
func (m *Manager) ActiveFile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return ""
	}
	return m.state.path
}

// Enabled reports the active policy's enabled state.
func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state != nil && m.state.policy.Enabled
}

// Write implements io.Writer for slog and preserves complete records across rotation.
func (m *Manager) Write(record []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || !m.state.policy.Enabled {
		return len(record), nil
	}
	if int64(len(record)) > m.state.policy.MaxFileSize {
		return 0, fmt.Errorf("log record exceeds maximum file size")
	}
	now := m.now().UTC()
	if (!m.state.periodEnd.IsZero() && !now.Before(m.state.periodEnd)) || (m.state.size > 0 && m.state.size+int64(len(record)) > m.state.policy.MaxFileSize) {
		if err := m.rotateLocked(now); err != nil {
			return 0, err
		}
	}
	written, err := m.state.file.Write(record)
	m.state.size += int64(written)
	if err != nil {
		return written, err
	}
	if err := m.cleanupLocked(); err != nil {
		return written, err
	}
	return written, nil
}

func (m *Manager) prepare(policy Policy) (*state, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.directory, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInitialization, err)
	}
	candidate := &state{policy: policy}
	if !policy.Enabled {
		return candidate, nil
	}
	if err := m.open(candidate, m.now().UTC()); err != nil {
		return nil, err
	}
	candidate.preparedPath = candidate.path
	return candidate, nil
}

func validatePolicy(policy Policy) error {
	allowed := map[string]bool{"none": true, "minute": true, "hour": true, "day": true, "week": true, "month": true, "year": true}
	if !allowed[policy.SplitPeriod] {
		return fmt.Errorf("%w: invalid split period %q", ErrInitialization, policy.SplitPeriod)
	}
	if policy.MaxFileSize <= 0 {
		return fmt.Errorf("%w: max file size must be positive", ErrInitialization)
	}
	if policy.MaxTotalSize < policy.MaxFileSize {
		return fmt.Errorf("%w: max total size must be at least max file size", ErrInitialization)
	}
	return nil
}

func (m *Manager) open(target *state, now time.Time) error {
	for {
		target.sequence++
		name := fmt.Sprintf("kernel-%s-%03d.log", now.Format("20060102T150405.000000000Z"), target.sequence)
		path := filepath.Join(m.directory, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: open log file: %v", ErrInitialization, err)
		}
		target.file, target.path, target.size, target.periodEnd = file, path, 0, periodBoundary(now, target.policy.SplitPeriod)
		return nil
	}
}

func (m *Manager) rotateLocked(now time.Time) error {
	if m.state.file != nil {
		if err := m.state.file.Close(); err != nil {
			return err
		}
	}
	m.state.file, m.state.path, m.state.size = nil, "", 0
	return m.open(m.state, now)
}

func periodBoundary(now time.Time, period string) time.Time {
	now = now.UTC()
	switch period {
	case "none":
		return time.Time{}
	case "minute":
		return now.Truncate(time.Minute).Add(time.Minute)
	case "hour":
		return now.Truncate(time.Hour).Add(time.Hour)
	case "day":
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	case "week":
		days := (8 - int(now.Weekday())) % 7
		if days == 0 {
			days = 7
		}
		return time.Date(now.Year(), now.Month(), now.Day()+days, 0, 0, 0, 0, time.UTC)
	case "month":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	case "year":
		return time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

type ownedFile struct {
	path     string
	size     int64
	modified time.Time
}

func (m *Manager) ownedFilesLocked() ([]ownedFile, int64, error) {
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		return nil, 0, err
	}
	var files []ownedFile
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "kernel-") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, 0, err
		}
		item := ownedFile{path: filepath.Join(m.directory, entry.Name()), size: info.Size(), modified: info.ModTime()}
		files = append(files, item)
		total += item.size
	}
	return files, total, nil
}
func (m *Manager) cleanupLocked() error {
	files, total, err := m.ownedFilesLocked()
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modified.Equal(files[j].modified) {
			return files[i].path < files[j].path
		}
		return files[i].modified.Before(files[j].modified)
	})
	for _, file := range files {
		if total <= m.state.policy.MaxTotalSize {
			break
		}
		if file.path == m.state.path {
			continue
		}
		if err := os.Remove(file.path); err != nil {
			return err
		}
		total -= file.size
	}
	return nil
}

// Prepare initializes a complete replacement writer before settings persistence.
func (m *Manager) Prepare(_ context.Context, values settings.Values) (settings.Prepared, error) {
	policy, err := policyFromValues(values)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	candidate, err := m.prepare(policy)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &prepared{manager: m, candidate: candidate}, nil
}

func policyFromValues(values settings.Values) (Policy, error) {
	enabled, ok := values["logging.enabled"].(bool)
	if !ok {
		return Policy{}, fmt.Errorf("%w: logging.enabled is invalid", ErrInitialization)
	}
	period, ok := values["logging.split_period"].(string)
	if !ok {
		return Policy{}, fmt.Errorf("%w: logging.split_period is invalid", ErrInitialization)
	}
	file, ok := values["logging.max_file_size"].(settings.ByteSize)
	if !ok {
		return Policy{}, fmt.Errorf("%w: logging.max_file_size is invalid", ErrInitialization)
	}
	total, ok := values["logging.max_total_size"].(settings.ByteSize)
	if !ok {
		return Policy{}, fmt.Errorf("%w: logging.max_total_size is invalid", ErrInitialization)
	}
	return Policy{Enabled: enabled, SplitPeriod: period, MaxFileSize: int64(file), MaxTotalSize: int64(total)}, nil
}

// PolicyFromValues converts the configured setting snapshot for initial composition.
func PolicyFromValues(values settings.Values) (Policy, error) { return policyFromValues(values) }

type prepared struct {
	manager   *Manager
	candidate *state
	once      sync.Once
}

func (p *prepared) Commit() {
	p.once.Do(func() {
		p.manager.mu.Lock()
		old := p.manager.state
		p.manager.state = p.candidate
		p.candidate.preparedPath = ""
		_ = p.manager.cleanupLocked()
		p.manager.mu.Unlock()
		if old != nil && old.file != nil {
			_ = old.file.Close()
		}
	})
}
func (p *prepared) Discard() {
	p.once.Do(func() {
		if p.candidate.file != nil {
			_ = p.candidate.file.Close()
		}
		if p.candidate.preparedPath != "" {
			_ = os.Remove(p.candidate.preparedPath)
		}
	})
}

// Close flushes and closes the active file.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.file == nil {
		return nil
	}
	err := m.state.file.Close()
	m.state.file = nil
	m.state.path = ""
	return err
}

var _ io.Writer = (*Manager)(nil)
