package daemon

import (
	"container/heap"
	"container/list"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"the8020/kernel/logging/records"
)

var errPolicyPrepared = errors.New("logging policy is awaiting commit")

var ownedName = regexp.MustCompile(`^segment-([0-9]{20})-(all|kernel|deno|logd)\.log$`)

type outputFile interface {
	io.Writer
	Sync() error
	Close() error
	Truncate(int64) error
	Seek(int64, int) (int64, error)
}

type segment struct {
	id                 uint64
	stream, path       string
	size               int64
	modified, end      time.Time
	file               outputFile
	dirty              bool
	ageIndex           int
	order, streamOrder *list.Element
}

type expiryHeap []*segment

func (h expiryHeap) Len() int { return len(h) }
func (h expiryHeap) Less(i, j int) bool {
	if h[i].modified.Equal(h[j].modified) {
		return h[i].id < h[j].id
	}
	return h[i].modified.Before(h[j].modified)
}
func (h expiryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].ageIndex = i; h[j].ageIndex = j }
func (h *expiryHeap) Push(v any)   { s := v.(*segment); s.ageIndex = len(*h); *h = append(*h, s) }
func (h *expiryHeap) Pop() any {
	old := *h
	s := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	s.ageIndex = -1
	return s
}

// store is confined to the logd file-owner loop. Its index retains only segment
// metadata; record content remains on disk. Queueing happens outside this owner.
type store struct {
	directory      string
	policy         records.Policy
	counter        uint64
	total          int64
	segments       map[uint64]*segment
	ordered        list.List
	streams        map[string]*list.List
	active         map[string]*segment
	expiry         expiryHeap
	openFile       func(string, int, os.FileMode) (outputFile, error)
	remove         func(string) error
	prepared       *preparedPolicy
	directoryDirty bool
	batch          [records.BatchBytes]byte
}

func openStore(directory string, policy records.Policy, now time.Time) (*store, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	s := &store{directory: directory, policy: policy, segments: make(map[uint64]*segment), streams: make(map[string]*list.List), active: make(map[string]*segment), remove: os.Remove, openFile: func(path string, flags int, mode os.FileMode) (outputFile, error) {
		return os.OpenFile(path, flags, mode)
	}}
	counterFile, err := os.OpenFile(filepath.Join(directory, "segments.counter"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(counterFile, 33))
		err = errors.Join(err, counterFile.Close())
	}
	if err == nil {
		if len(data) > 32 {
			return nil, errors.New("invalid log segment counter")
		}
		s.counter, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return nil, errors.New("invalid log segment counter")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var found []*segment
	for _, entry := range entries {
		parts := ownedName.FindStringSubmatch(entry.Name())
		if parts == nil || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		id, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid log segment identity")
		}
		if _, ok := s.segments[id]; ok {
			return nil, errors.New("duplicate log segment identity")
		}
		seg := &segment{id: id, stream: parts[2], path: filepath.Join(directory, entry.Name()), size: info.Size(), modified: info.ModTime(), ageIndex: -1}
		s.segments[id] = seg
		found = append(found, seg)
		s.counter = max(s.counter, id)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].id < found[j].id })
	s.segments = make(map[uint64]*segment, len(found))
	for _, seg := range found {
		s.register(seg)
		if seg.size == 0 {
			if err := s.erase(seg); err != nil {
				return nil, err
			}
		}
	}
	if policy.Enabled {
		for _, stream := range policyStreams(policy) {
			seg, err := s.create(stream, policy, now)
			if err != nil {
				s.close()
				return nil, err
			}
			s.register(seg)
			s.active[stream] = seg
		}
	}
	if err := s.maintain(now, 0); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func policyStreams(p records.Policy) []string {
	if p.SplitBy == "source" {
		return []string{"kernel", "deno", "logd"}
	}
	return []string{"all"}
}

func (s *store) register(seg *segment) {
	s.segments[seg.id] = seg
	s.total += seg.size
	seg.order = s.ordered.PushBack(seg)
	if s.streams[seg.stream] == nil {
		s.streams[seg.stream] = new(list.List)
	}
	seg.streamOrder = s.streams[seg.stream].PushBack(seg)
	if seg.size > 0 {
		heap.Push(&s.expiry, seg)
	}
}

func (s *store) reserveID() (uint64, error) {
	if s.counter == ^uint64(0) {
		return 0, errors.New("log segment counter exhausted")
	}
	next := s.counter + 1
	path := filepath.Join(s.directory, "segments.counter")
	temp := path + ".next"
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return 0, err
	}
	err = records.WriteAll(f, []byte(strconv.FormatUint(next, 10)+"\n"))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(temp, path)
	}
	if err != nil {
		_ = os.Remove(temp)
		return 0, err
	}
	// Reserve even if directory synchronization fails: this process may retry,
	// but must never reuse a counter value whose rename might already be durable.
	s.counter = next
	s.directoryDirty = true
	if err := s.syncDirectory(); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *store) create(stream string, policy records.Policy, now time.Time) (*segment, error) {
	id, err := s.reserveID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.directory, fmt.Sprintf("segment-%020d-%s.log", id, stream))
	f, err := s.openFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	s.directoryDirty = true
	return &segment{id: id, stream: stream, path: path, modified: now, end: records.PeriodBoundary(now, policy.SplitPeriod), file: f, ageIndex: -1}, nil
}

func (s *store) seal(seg *segment) error {
	if seg.file == nil {
		return nil
	}
	var err error
	if seg.dirty {
		err = seg.file.Sync()
	}
	err = errors.Join(err, seg.file.Close())
	seg.file = nil
	seg.dirty = false
	if s.active[seg.stream] == seg {
		delete(s.active, seg.stream)
	}
	if seg.size == 0 {
		if seg.ageIndex < 0 {
			heap.Push(&s.expiry, seg)
		}
		err = errors.Join(err, s.erase(seg))
	}
	return err
}

func (s *store) rotate(stream string, now time.Time) (*segment, error) {
	if s.prepared != nil {
		return nil, errPolicyPrepared
	}
	seg, err := s.create(stream, s.policy, now)
	if err != nil {
		return nil, err
	}
	old := s.active[stream]
	s.register(seg)
	s.active[stream] = seg
	if old != nil {
		if err = s.seal(old); err != nil {
			return seg, err
		}
	}
	return seg, nil
}

// appendBatch writes one source's complete encoded records with one ordinary
// write in the common case. Batches are split at record-preserving boundaries.
func (s *store) appendBatch(source string, lines [][]byte, now time.Time) (int, error) {
	if s.prepared != nil {
		return 0, errPolicyPrepared
	}
	if !s.policy.Enabled {
		return 0, nil
	}
	if !records.ValidSource(source) {
		return 0, errors.New("invalid log source")
	}
	stream := s.policy.Stream(source)
	completed := 0
	for len(lines) > 0 {
		seg := s.active[stream]
		if seg == nil || (seg.size > 0 && (!seg.end.IsZero() && !now.Before(seg.end) || seg.size+int64(len(lines[0])) > s.policy.MaxFileSize)) {
			var err error
			seg, err = s.rotate(stream, now)
			if err != nil {
				return completed, err
			}
		}
		batch := s.batch[:0]
		n := 0
		for n < len(lines) {
			line := lines[n]
			if len(line) == 0 || len(line) > records.MaxRecord || line[len(line)-1] != '\n' {
				return completed, errors.New("invalid encoded log record")
			}
			if len(batch)+len(line) > records.BatchBytes || seg.size+int64(len(batch)+len(line)) > s.policy.MaxFileSize {
				break
			}
			batch = append(batch, line...)
			n++
		}
		if n == 0 {
			return completed, errors.New("log record cannot fit configured segment")
		}
		if err := s.maintain(now, int64(len(batch))); err != nil {
			return completed, err
		}
		seg = s.active[stream]
		if seg == nil {
			var err error
			seg, err = s.rotate(stream, now)
			if err != nil {
				return completed, err
			}
		}
		before := seg.size
		written, err := writeBatch(seg.file, batch)
		seg.size += int64(written)
		s.total += int64(written)
		s.changed(seg, now)
		if err != nil {
			rollback := seg.file.Truncate(before)
			if rollback == nil {
				s.total -= seg.size - before
				seg.size = before
				s.changed(seg, now)
				_, rollback = seg.file.Seek(before, io.SeekStart)
			}
			if rollback != nil {
				rollback = errors.Join(rollback, s.seal(seg))
			}
			return completed, errors.Join(err, rollback)
		}
		completed += n
		lines = lines[n:]
	}
	return completed, nil
}

func (s *store) changed(seg *segment, now time.Time) {
	seg.modified = now
	seg.dirty = true
	if seg.size == 0 {
		if seg.ageIndex >= 0 {
			heap.Remove(&s.expiry, seg.ageIndex)
		}
	} else if seg.ageIndex < 0 {
		heap.Push(&s.expiry, seg)
	} else {
		heap.Fix(&s.expiry, seg.ageIndex)
	}
}

func writeBatch(w io.Writer, p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, err := w.Write(p[written:])
		if n < 0 || n > len(p)-written {
			return written, errors.New("invalid log write count")
		}
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (s *store) erase(seg *segment) error {
	if seg.file != nil {
		return errors.New("cannot delete active log segment")
	}
	if err := s.remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if seg.ageIndex >= 0 {
		heap.Remove(&s.expiry, seg.ageIndex)
	}
	s.ordered.Remove(seg.order)
	s.streams[seg.stream].Remove(seg.streamOrder)
	delete(s.segments, seg.id)
	s.total -= seg.size
	s.directoryDirty = true
	return nil
}

func (s *store) maintain(now time.Time, reserve int64) error {
	if s.prepared != nil {
		return errPolicyPrepared
	}
	for len(s.expiry) > 0 {
		seg := s.expiry[0]
		if now.Before(seg.modified.Add(s.policy.MaxAge)) {
			break
		}
		if seg.file != nil {
			if _, err := s.rotate(seg.stream, now); err != nil {
				return err
			}
		}
		if err := s.erase(seg); err != nil {
			return err
		}
	}
	for s.total+reserve > s.policy.MaxTotalSize {
		var closed, oldActive *segment
		for e := s.ordered.Front(); e != nil; e = e.Next() {
			seg := e.Value.(*segment)
			if seg.size == 0 {
				continue
			}
			if seg.file == nil {
				closed = seg
				break
			}
			if oldActive == nil {
				oldActive = seg
			}
		}
		if closed == nil {
			if oldActive == nil {
				return errors.New("log retention cannot free required space")
			}
			if _, err := s.rotate(oldActive.stream, now); err != nil {
				return err
			}
			closed = oldActive
		}
		if err := s.erase(closed); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) syncDirectory() error {
	if !s.directoryDirty {
		return nil
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err == nil {
		s.directoryDirty = false
	}
	return err
}

func (s *store) sync() error {
	var errs []error
	for _, seg := range s.active {
		if seg.dirty {
			if err := seg.file.Sync(); err != nil {
				errs = append(errs, err)
			} else {
				seg.dirty = false
			}
		}
	}
	return errors.Join(append(errs, s.syncDirectory())...)
}

func (s *store) close() error {
	var errs []error
	if s.prepared != nil {
		errs = append(errs, s.prepared.discard())
	}
	for _, seg := range s.active {
		errs = append(errs, s.seal(seg))
	}
	return errors.Join(append(errs, s.syncDirectory())...)
}

type preparedPolicy struct {
	store  *store
	policy records.Policy
	files  []*segment
	done   bool
}

func (s *store) prepare(policy records.Policy, now time.Time) (*preparedPolicy, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if s.prepared != nil {
		return nil, errors.New("logging policy already prepared")
	}
	p := &preparedPolicy{store: s, policy: policy}
	if policy.Enabled {
		for _, stream := range policyStreams(policy) {
			seg, err := s.create(stream, policy, now)
			if err != nil {
				p.discard()
				return nil, err
			}
			p.files = append(p.files, seg)
		}
	}
	s.prepared = p
	return p, nil
}

func (p *preparedPolicy) discard() error {
	if p.done {
		return nil
	}
	p.done = true
	var errs []error
	for _, seg := range p.files {
		errs = append(errs, seg.file.Close(), p.store.remove(seg.path))
	}
	if p.store.prepared == p {
		p.store.prepared = nil
	}
	return errors.Join(errs...)
}

func (p *preparedPolicy) commit(now time.Time) error {
	if p.done {
		return errors.New("logging policy already settled")
	}
	p.done = true
	s := p.store
	var errs []error
	for _, seg := range s.active {
		errs = append(errs, s.seal(seg))
	}
	s.policy = p.policy
	for _, seg := range p.files {
		s.register(seg)
		s.active[seg.stream] = seg
	}
	s.prepared = nil
	errs = append(errs, s.maintain(now, 0))
	return errors.Join(errs...)
}
