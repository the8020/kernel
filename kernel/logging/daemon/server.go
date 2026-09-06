// Package daemon owns logd's bounded ingestion and single file-owner loop.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/logging/records"
)

type controlCall struct {
	request  records.ControlRequest
	response chan records.ControlReply
}

type server struct {
	init              records.Initialization
	listener          *net.UnixListener
	control           net.Conn
	kernelRaw         [2]*os.File
	queue             *records.Queue
	policy            atomic.Pointer[records.Policy]
	revision          atomic.Uint64
	accepted, invalid atomic.Uint64
	upstream          [4]atomic.Uint64
	inputs            atomic.Int64
	mu                sync.Mutex
	producers         map[string]*producer
	connections       map[*net.UnixConn]bool
	connectionSlots   chan struct{}
	handshakes        chan struct{}
	commands          chan controlCall
	stopping          chan struct{}
	stopOnce          sync.Once
}

func (s *server) stop() { s.stopOnce.Do(func() { close(s.stopping); _ = s.listener.Close() }) }

// Run receives policy only through the inherited kernel control channel. Raw
// kernel descriptors remain readable after a kernel crash until their EOF.
func Run(ctx context.Context, control net.Conn, kernelRaw [2]*os.File) error {
	defer control.Close()
	_ = control.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, err := records.ReadFrame(control)
	if err != nil {
		return errors.New("read initial logging control frame")
	}
	var initial records.Initialization
	if json.Unmarshal(data, &initial) != nil || initial.Version != records.ProtocolVersion || !identity.Is(initial.NodeID, "nod") || !filepath.IsAbs(initial.Directory) || !filepath.IsAbs(initial.Socket) || initial.MaxProducers < 1 || initial.MaxProducers > records.MaxProducers {
		return errors.New("invalid initial logging configuration")
	}
	if err := initial.Policy.Validate(); err != nil {
		return err
	}
	_ = control.SetReadDeadline(time.Time{})
	if err := os.MkdirAll(initial.Directory, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(initial.Directory, writerLockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another log writer owns this node")
	}
	if err = os.MkdirAll(filepath.Dir(initial.Socket), 0755); err != nil {
		return err
	}
	if info, err := os.Lstat(initial.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("logging socket path is occupied by another file")
		}
		if err = os.Remove(initial.Socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: initial.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(initial.Socket, 0666); err != nil {
		return err
	}
	s := &server{init: initial, listener: listener, control: control, kernelRaw: kernelRaw, queue: records.NewQueue(records.PendingBytes, records.PriorityReserve, 4096), producers: make(map[string]*producer), connections: make(map[*net.UnixConn]bool), connectionSlots: make(chan struct{}, initial.MaxProducers+32), handshakes: make(chan struct{}, 32), commands: make(chan controlCall, 8), stopping: make(chan struct{})}
	s.policy.Store(&initial.Policy)
	s.revision.Store(1)
	defer s.closeInputs()
	defer s.stop()
	go s.accept()
	for i, f := range kernelRaw {
		if f != nil {
			s.inputs.Add(1)
			go func() { defer s.inputs.Add(-1); s.raw(f, "kernel", "", []string{"stdout", "stderr"}[i]) }()
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			s.stop()
		case <-s.stopping:
		}
	}()
	w := &fileOwner{server: s, status: records.Status{StartedAt: time.Now().UTC(), ActiveFiles: make(map[string]string)}}
	w.open()
	if err := writeControl(control, records.ControlReply{OK: true, Status: w.snapshot()}); err != nil {
		return err
	}
	go s.controlLoop()
	s.emit(records.Record{Time: time.Now().UTC(), Level: "INFO", Source: "logd", Component: "lifecycle", NodeID: initial.NodeID, Message: "log writer started", Attributes: map[string]string{"pid": strconv.Itoa(os.Getpid()), "protocol": strconv.Itoa(records.ProtocolVersion)}})
	return w.run()
}

func writeControl(conn net.Conn, reply records.ControlReply) error {
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return records.WriteControlFrame(conn, data)
}

func (s *server) controlLoop() {
	defer s.stop()
	for {
		data, err := records.ReadFrame(s.control)
		if err != nil {
			return
		}
		var request records.ControlRequest
		if json.Unmarshal(data, &request) != nil || !identity.Is(request.ID, "cor") {
			return
		}
		reply := records.ControlReply{ID: request.ID, OK: true}
		switch request.Operation {
		case "register":
			if request.Binding == nil {
				err = errors.New("missing producer binding")
			} else {
				var paths records.RawPaths
				paths, err = s.register(*request.Binding)
				reply.Paths = &paths
			}
		case "unregister":
			err = s.unregister(request.Producer)
		default:
			call := controlCall{request: request, response: make(chan records.ControlReply, 1)}
			select {
			case s.commands <- call:
			case <-s.stopping:
				return
			}
			select {
			case reply = <-call.response:
			case <-s.stopping:
				if request.Operation != "shutdown" {
					return
				}
				reply = records.ControlReply{ID: request.ID, OK: true}
			}
		}
		if err != nil {
			reply.OK = false
			reply.Error = records.Text(err.Error(), 512)
		}
		if writeControl(s.control, reply) != nil {
			return
		}
		if request.Operation == "shutdown" {
			return
		}
	}
}

type fileOwner struct {
	server        *server
	store         *store
	status        records.Status
	nextTry       time.Time
	preparation   string
	preparedUntil time.Time
	lastSummary   time.Time
	lastDropped   [4]uint64
	recovered     bool
}

func (w *fileOwner) failure(err error) {
	if err == nil {
		return
	}
	w.status.Available = false
	w.status.LastError = records.Text(err.Error(), 512)
	w.status.StorageErrors++
	w.nextTry = time.Now().Add(time.Second)
}
func (w *fileOwner) success() {
	if w.status.LastError != "" {
		w.recovered = true
	}
	w.status.Available = true
	w.status.LastError = ""
	w.nextTry = time.Time{}
}
func (w *fileOwner) open() {
	s, err := openStore(w.server.init.Directory, *w.server.policy.Load(), time.Now().UTC())
	if err != nil {
		w.failure(err)
		return
	}
	w.store = s
	w.success()
}

func (w *fileOwner) snapshot() *records.Status {
	s := w.server
	out := w.status
	out.Policy = *s.policy.Load()
	out.PolicyRevision = s.revision.Load()
	out.Queue = s.queue.Stats()
	out.Accepted = s.accepted.Load()
	out.Invalid = s.invalid.Load()
	for i := range out.UpstreamDropped {
		out.UpstreamDropped[i] = s.upstream[i].Load()
	}
	s.mu.Lock()
	out.Producers = len(s.producers)
	out.Connected = 0
	for _, p := range s.producers {
		if p.connection != nil {
			out.Connected++
		}
	}
	s.mu.Unlock()
	out.ActiveFiles = make(map[string]string, 4)
	if w.store != nil {
		out.TotalBytes = w.store.total
		out.Segments = len(w.store.segments)
		out.ReadPosition = w.store.readPosition()
		for name, seg := range w.store.active {
			out.ActiveFiles[name] = seg.path
		}
	}
	return &out
}

func (w *fileOwner) flush() {
	if w.store != nil && w.store.prepared != nil {
		return
	}
	b := w.server.queue.Take(records.BatchBytes)
	defer b.Release()
	if len(b.Frames) == 0 {
		return
	}
	if !w.server.policy.Load().Enabled {
		return
	}
	policy := w.server.policy.Load()
	frames := b.Frames[:0]
	for _, frame := range b.Frames {
		if policy.Allows(frame.Level) {
			frames = append(frames, frame)
		}
	}
	if len(frames) == 0 {
		return
	}
	if w.store == nil || time.Now().Before(w.nextTry) {
		for _, f := range frames {
			w.server.queue.Drop(f.Level, 1)
		}
		return
	}
	if w.status.LastError != "" {
		if err := w.store.sync(); err != nil {
			w.failure(err)
			for _, f := range frames {
				w.server.queue.Drop(f.Level, 1)
			}
			return
		}
	}
	groups := [][]records.Frame{frames}
	if w.store.policy.SplitBy == "source" {
		groups = make([][]records.Frame, 3)
		for _, f := range frames {
			index := 0
			if f.Source == "deno" {
				index = 1
			} else if f.Source == "logd" {
				index = 2
			}
			groups[index] = append(groups[index], f)
		}
	}
	failed := false
	for _, frames := range groups {
		if len(frames) == 0 {
			continue
		}
		if failed {
			for _, f := range frames {
				w.server.queue.Drop(f.Level, 1)
			}
			continue
		}
		lines := make([][]byte, len(frames))
		for i, f := range frames {
			lines[i] = f.Payload
		}
		n, err := w.store.appendBatch(frames[0].Source, lines, time.Now().UTC())
		w.status.Written += uint64(n)
		for _, f := range frames[:n] {
			w.status.WrittenBytes += uint64(len(f.Payload))
		}
		if err != nil {
			w.failure(err)
			failed = true
			for _, f := range frames[n:] {
				w.server.queue.Drop(f.Level, 1)
			}
		}
	}
	if !failed {
		w.success()
	}
}

func (w *fileOwner) summary(now time.Time) {
	if now.Sub(w.lastSummary) < 10*time.Second {
		return
	}
	stats := w.server.queue.Stats()
	var current [4]uint64
	changed := w.recovered
	attrs := make(map[string]string, 4)
	for i, name := range []string{"debug", "info", "warn", "error"} {
		current[i] = stats.Dropped[i] + w.server.upstream[i].Load()
		if current[i] != w.lastDropped[i] {
			changed = true
			attrs[name] = strconv.FormatUint(current[i]-w.lastDropped[i], 10)
		}
	}
	if !changed {
		return
	}
	message := "log records were dropped"
	if w.recovered {
		message = "log storage recovered"
	}
	w.server.emit(records.Record{Time: now, Level: "WARN", Source: "logd", Component: "writer", NodeID: w.server.init.NodeID, Message: message, Attributes: attrs})
	w.lastDropped = current
	w.lastSummary = now
	w.recovered = false
}

func (w *fileOwner) command(request records.ControlRequest) records.ControlReply {
	reply := records.ControlReply{ID: request.ID, OK: true}
	var err error
	now := time.Now().UTC()
	switch request.Operation {
	case "status":
		reply.Status = w.snapshot()
	case "query":
		if request.Query == nil {
			err = errors.New("missing log query")
		} else if w.store == nil {
			reply.Page = &records.Page{State: "unavailable", Reason: "Log storage is unavailable.", Records: []records.LocatedRecord{}}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			page, queryErr := w.store.query(ctx, *request.Query)
			cancel()
			reply.Page = &page
			err = queryErr
		}
	case "prepare":
		if request.Policy == nil {
			err = errors.New("missing log policy")
		} else if w.store == nil {
			err = errors.New("log storage is unavailable")
		} else {
			_, err = w.store.prepare(*request.Policy, now)
			if err == nil {
				w.preparation = request.ID
				w.preparedUntil = now.Add(5 * time.Second)
				reply.Preparation = request.ID
			}
		}
	case "commit", "discard":
		if w.store == nil || w.store.prepared == nil || request.Preparation == "" || request.Preparation != w.preparation {
			err = errors.New("log policy preparation is no longer available")
		} else if request.Operation == "discard" {
			err = w.store.prepared.discard()
			w.preparation = ""
		} else {
			policy := w.store.prepared.policy
			commitErr := w.store.prepared.commit(now)
			w.preparation = ""
			w.server.policy.Store(&policy)
			w.server.revision.Add(1)
			w.server.broadcast()
			if commitErr != nil {
				w.failure(commitErr)
			} else {
				w.success()
			}
		}
	case "shutdown":
		// The control loop acknowledges this request before starting the drain.
	default:
		err = errors.New("unknown logging control operation")
	}
	if err != nil {
		reply.OK = false
		reply.Error = records.Text(err.Error(), 512)
	}
	return reply
}

func (w *fileOwner) run() error {
	tick := time.NewTicker(records.FlushInterval)
	defer tick.Stop()
	syncTick := time.NewTicker(records.SyncInterval)
	defer syncTick.Stop()
	var deadline time.Time
	stopping := w.server.stopping
	forced := false
	for {
		select {
		case <-stopping:
			deadline = time.Now().Add(2 * time.Second)
			stopping = nil
		case call := <-w.server.commands:
			call.response <- w.command(call.request)
		case <-w.server.queue.Notify():
			if !deadline.IsZero() || w.server.queue.Stats().PendingBytes >= records.BatchBytes {
				w.flush()
			}
		case now := <-tick.C:
			if w.store != nil && w.store.prepared != nil && !now.Before(w.preparedUntil) {
				if err := w.store.prepared.discard(); err != nil {
					w.failure(err)
				}
				w.preparation = ""
			}
			w.flush()
			w.summary(now.UTC())
		case now := <-syncTick.C:
			if w.store == nil && !now.Before(w.nextTry) {
				w.open()
			}
			if w.store != nil && w.store.prepared == nil && !now.Before(w.nextTry) {
				if err := errors.Join(w.store.maintain(now.UTC(), 0), w.store.sync()); err != nil {
					w.failure(err)
				}
			}
		}
		if !deadline.IsZero() {
			if w.store != nil && w.store.prepared != nil {
				_ = w.store.prepared.discard()
				w.preparation = ""
			}
			now := time.Now()
			if !forced && !now.Before(deadline) {
				w.server.closeInputs()
				forced = true
			}
			if w.server.inputs.Load() == 0 && w.server.queue.Stats().PendingRecords == 0 || now.After(deadline.Add(250*time.Millisecond)) {
				if w.store != nil {
					return w.store.close()
				}
				return fmt.Errorf("log storage unavailable at shutdown: %s", w.status.LastError)
			}
		}
	}
}
