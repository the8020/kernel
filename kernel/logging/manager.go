// Package logging owns the kernel's logd process, producers, and settings
// integration. Only the dedicated executable opens persisted log segments.
package logging

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/logging/daemon"
	"the8020/kernel/logging/records"
	"the8020/kernel/settings"
)

var ErrInitialization = errors.New("logging initialization failed")
var ErrUnavailable = errors.New("log writer is unavailable")

// Config supplies process composition, not a second user-settings surface.
// Executable defaults to logd alongside the invoking kernel executable.
type Config struct {
	Directory, Socket, NodeID, Executable string
	Policy                                records.Policy
	MaxProducers                          int
	CaptureStandard                       bool
}

// Status is a cached snapshot; reading it performs no process or storage I/O.
// Daemon counters belong to StartedAt; parent counters survive daemon restart.
type Status struct {
	records.Status
	Running          bool               `json:"running"`
	PID              int                `json:"pid,omitempty"`
	Restarts         uint64             `json:"restarts"`
	ProcessError     string             `json:"process_error,omitempty"`
	KernelQueue      records.QueueStats `json:"kernel_queue"`
	KernelInvalid    uint64             `json:"kernel_invalid"`
	RawDroppedBytes  uint64             `json:"raw_dropped_bytes"`
	RawRecoveryError string             `json:"raw_recovery_error,omitempty"`
	PolicyPending    bool               `json:"policy_pending"`
}

type managerCall struct {
	ctx     context.Context
	request records.ControlRequest
	reply   chan managerReply
}
type managerReply struct {
	wire       records.ControlReply
	generation uint64
	err        error
}
type registration struct {
	binding records.Binding
	paths   records.RawPaths
	guards  [2]*rawGuard
	retired bool
}
type childProcess struct {
	command    *exec.Cmd
	control    net.Conn
	wait       chan error
	generation uint64
	policy     records.Policy
}

// Manager retains bounded transport buffers and live producer registrations.
// One goroutine owns all process control and never runs on a logging caller.
type Manager struct {
	config     Config
	policy     atomic.Pointer[records.Policy]
	producer   *producerClient
	logger     *slog.Logger
	raw        *standardCapture
	rawDropped atomic.Uint64
	commands   chan managerCall
	wake       chan struct{}
	stop, done chan struct{}
	closeOnce  sync.Once
	mu         sync.Mutex
	status     Status
	pending    *prepared
	closeErr   error
	// run goroutine only:
	child            *childProcess
	generation       uint64
	registrations    map[string]*registration
	lastFailureEvent time.Time
}

func New(config Config) (*Manager, error) {
	if err := config.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInitialization, err)
	}
	if !identity.Is(config.NodeID, "nod") || !filepath.IsAbs(config.Directory) || !filepath.IsAbs(config.Socket) || config.MaxProducers < 1 || config.MaxProducers > records.MaxProducers {
		return nil, fmt.Errorf("%w: invalid log process configuration", ErrInitialization)
	}
	if config.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		config.Executable = filepath.Join(filepath.Dir(executable), "logd")
	}
	token, err := identity.NewToken()
	if err != nil {
		return nil, err
	}
	m := &Manager{config: config, commands: make(chan managerCall, 8), wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), registrations: make(map[string]*registration)}
	m.policy.Store(&config.Policy)
	m.status.Policy = config.Policy
	m.raw, err = captureStandard(config.CaptureStandard, &m.rawDropped)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInitialization, err)
	}
	binding := records.Binding{ID: config.NodeID, Token: token}
	m.registrations[binding.ID] = &registration{binding: binding}
	m.producer = newProducer(config.Socket, binding, &m.policy)
	m.logger = slog.New(&handler{node: config.NodeID, emitter: m.producer})
	go m.run()
	return m, nil
}

func (m *Manager) Logger() *slog.Logger { return m.logger }

func (m *Manager) NodeID() string { return m.config.NodeID }
func (m *Manager) Enabled() bool  { return m.policy.Load().Enabled }

// ReadPosition takes the latest cached writer boundary without I/O. Invocation
// and sandbox owners take it before emitting their first lifecycle/log record.
// An unavailable initial writer may have no reference yet; ID/time queries
// remain available. Existing positions expire with their underlying segments.
func (m *Manager) ReadPosition() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status.ReadPosition
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	s := m.status
	s.ActiveFiles = maps.Clone(s.ActiveFiles)
	m.mu.Unlock()
	s.PolicyPending = !s.Running || s.Policy != *m.policy.Load()
	s.KernelQueue = m.producer.queue.Stats()
	s.KernelInvalid = m.producer.invalid.Load()
	s.RawDroppedBytes = m.rawDropped.Load()
	return s
}
func (m *Manager) ActiveFile() string {
	if !m.Enabled() {
		return ""
	}
	s := m.Status()
	if path := s.ActiveFiles["all"]; path != "" {
		return path
	}
	return s.ActiveFiles["kernel"]
}

// Console preserves intentional process progress outside captured stdout/stderr.
func (m *Manager) Console() (stdout, stderr *os.File) { return m.raw.console() }

func (m *Manager) call(ctx context.Context, request records.ControlRequest) (managerReply, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	call := managerCall{ctx: ctx, request: request, reply: make(chan managerReply, 1)}
	select {
	case <-m.stop:
		return managerReply{}, ErrUnavailable
	case <-ctx.Done():
		return managerReply{}, ctx.Err()
	case m.commands <- call:
	default:
		return managerReply{}, errors.New("logging control queue is full")
	}
	select {
	case result := <-call.reply:
		return result, result.err
	case <-ctx.Done():
		return managerReply{}, ctx.Err()
	case <-m.done:
		return managerReply{}, ErrUnavailable
	}
}

// RegisterSandbox binds its existing private token and raw endpoints. A writer
// outage still returns usable drained FIFOs; the next process replays live binds.
func (m *Manager) RegisterSandbox(ctx context.Context, id, token string) (records.RawPaths, error) {
	decoded, err := hex.DecodeString(token)
	if !identity.Is(id, "sbx") || err != nil || len(decoded) != 32 || len(token) != 64 {
		return records.RawPaths{}, errors.New("invalid sandbox log binding")
	}
	result, err := m.call(ctx, records.ControlRequest{Operation: "register", Binding: &records.Binding{ID: id, Token: token}})
	if err != nil {
		return records.RawPaths{}, err
	}
	return *result.wire.Paths, nil
}

// UnregisterSandbox follows native process stop, never mere logd death.
func (m *Manager) UnregisterSandbox(ctx context.Context, id string) error {
	if !identity.Is(id, "sbx") {
		return errors.New("invalid sandbox log identity")
	}
	_, err := m.call(ctx, records.ControlRequest{Operation: "unregister", Producer: id, Binding: &records.Binding{ID: id}})
	return err
}

func (m *Manager) Query(ctx context.Context, query records.Query) (records.Page, error) {
	query, err := query.Normalize()
	if err != nil {
		return records.Page{}, err
	}
	result, err := m.call(ctx, records.ControlRequest{Operation: "query", Query: &query})
	if errors.Is(err, ErrUnavailable) {
		return records.Page{State: "unavailable", Reason: "The log writer is unavailable.", Records: []records.LocatedRecord{}}, nil
	}
	if err != nil {
		return records.Page{}, err
	}
	if result.wire.Page == nil {
		return records.Page{}, errors.New("missing log query response")
	}
	return *result.wire.Page, nil
}

func PolicyFromValues(values settings.Values) (records.Policy, error) {
	p := records.Policy{}
	var ok bool
	if p.Enabled, ok = values["logging.enabled"].(bool); !ok {
		return p, fmt.Errorf("%w: invalid logging.enabled", ErrInitialization)
	}
	for key, target := range map[string]*string{"logging.level": &p.Level, "logging.split_by": &p.SplitBy, "logging.split_period": &p.SplitPeriod} {
		if *target, ok = values[key].(string); !ok {
			return p, fmt.Errorf("%w: invalid %s", ErrInitialization, key)
		}
	}
	for key, target := range map[string]*int64{"logging.max_file_size": &p.MaxFileSize, "logging.max_total_size": &p.MaxTotalSize} {
		v, valid := values[key].(settings.ByteSize)
		if !valid {
			return p, fmt.Errorf("%w: invalid %s", ErrInitialization, key)
		}
		*target = int64(v)
	}
	age, ok := values["logging.max_age"].(settings.Duration)
	if !ok {
		return p, fmt.Errorf("%w: invalid logging.max_age", ErrInitialization)
	}
	p.MaxAge = time.Duration(age)
	if err := p.Validate(); err != nil {
		return p, fmt.Errorf("%w: %v", ErrInitialization, err)
	}
	return p, nil
}

func (m *Manager) Prepare(ctx context.Context, values settings.Values) (settings.Prepared, error) {
	policy, err := PolicyFromValues(values)
	if err != nil {
		return nil, err
	}
	result, err := m.call(ctx, records.ControlRequest{Operation: "prepare", Policy: &policy})
	if err != nil {
		return nil, err
	}
	return &prepared{manager: m, policy: policy, generation: result.generation, token: result.wire.Preparation}, nil
}

type prepared struct {
	manager    *Manager
	policy     records.Policy
	generation uint64
	token      string
	once       sync.Once
}

func (p *prepared) Commit() {
	p.once.Do(func() {
		m := p.manager
		m.mu.Lock()
		m.producer.setPolicy(&p.policy)
		m.pending = p
		m.mu.Unlock()
		select {
		case m.wake <- struct{}{}:
		default:
		}
	})
}
func (p *prepared) Discard() {
	p.once.Do(func() {
		// A saturated/closed control path cannot retain resources indefinitely:
		// uncommitted daemon preparations expire after five seconds.
		call := managerCall{ctx: context.Background(), request: records.ControlRequest{Operation: "discard", Preparation: p.token}, reply: make(chan managerReply, 1)}
		select {
		case p.manager.commands <- call:
		default:
		}
	})
}

func (m *Manager) failure(err error) {
	m.mu.Lock()
	m.status.Running, m.status.Available, m.status.PID = false, false, 0
	m.status.ProcessError = records.Text(err.Error(), 512)
	m.mu.Unlock()
	if time.Since(m.lastFailureEvent) >= 10*time.Second {
		m.lastFailureEvent = time.Now()
		m.logger.Error("log writer unavailable", "component", "logging", "error", err)
	}
}

func (m *Manager) snapshot(s *records.Status) {
	if s == nil {
		return
	}
	m.mu.Lock()
	recovered := m.status.ProcessError != ""
	m.status.Status = *s
	m.status.Running, m.status.PID = true, m.child.command.Process.Pid
	m.status.ProcessError = ""
	m.status.Restarts = m.generation - 1
	m.mu.Unlock()
	if recovered {
		m.logger.Warn("log writer recovered", "component", "logging", "restarts", m.generation-1)
	}
}

func (m *Manager) rpc(request records.ControlRequest) (records.ControlReply, error) {
	if m.child == nil {
		return records.ControlReply{}, ErrUnavailable
	}
	id, err := identity.New("cor")
	if err != nil {
		return records.ControlReply{}, err
	}
	request.ID = id
	data, err := json.Marshal(request)
	if err != nil {
		return records.ControlReply{}, err
	}
	// Once sent, always consume its reply even when the caller cancels. There
	// is one response stream, one owner, and no unbounded pending RPC map.
	timeout := 2500 * time.Millisecond
	if request.Operation == "shutdown" {
		timeout = 250 * time.Millisecond
	}
	_ = m.child.control.SetDeadline(time.Now().Add(timeout))
	if err = records.WriteFrame(m.child.control, data); err != nil {
		return records.ControlReply{}, err
	}
	data, err = records.ReadControlFrame(m.child.control)
	if err != nil {
		return records.ControlReply{}, err
	}
	var reply records.ControlReply
	if json.Unmarshal(data, &reply) != nil || reply.ID != id {
		return records.ControlReply{}, errors.New("invalid log control response")
	}
	if !reply.OK {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

func (m *Manager) start() error {
	if active, err := daemon.WriterActive(m.config.Directory); err != nil {
		return err
	} else if active {
		return errors.New("previous log writer is still draining")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	parent, inherited := os.NewFile(uintptr(fds[0]), "logd-parent"), os.NewFile(uintptr(fds[1]), "logd-child")
	defer inherited.Close()
	control, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		return err
	}
	m.drainRaw(false)
	cmd := exec.Command(m.config.Executable)
	cmd.ExtraFiles = []*os.File{inherited, m.raw.guards[0].file, m.raw.guards[1].file}
	cmd.Stdout, cmd.Stderr = m.raw.console()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = control.Close()
		m.drainRaw(true)
		return err
	}
	m.generation++
	m.child = &childProcess{command: cmd, control: control, wait: make(chan error, 1), generation: m.generation, policy: *m.policy.Load()}
	child := m.child
	go func() { child.wait <- cmd.Wait() }()
	initial := records.Initialization{Version: records.ProtocolVersion, Directory: m.config.Directory, Socket: m.config.Socket, NodeID: m.config.NodeID, Policy: child.policy, MaxProducers: m.config.MaxProducers}
	data, _ := json.Marshal(initial)
	_ = control.SetDeadline(time.Now().Add(2500 * time.Millisecond))
	if err = records.WriteFrame(control, data); err != nil {
		return err
	}
	data, err = records.ReadControlFrame(control)
	if err != nil {
		return err
	}
	var ready records.ControlReply
	if json.Unmarshal(data, &ready) != nil || !ready.OK || ready.Status == nil {
		return errors.New("invalid log writer initialization response")
	}
	for _, r := range m.registrations {
		if r.retired {
			continue
		}
		select {
		case <-m.stop:
			return ErrUnavailable
		default:
		}
		if _, err := m.rpc(records.ControlRequest{Operation: "register", Binding: &r.binding}); err != nil {
			return err
		}
	}
	m.snapshot(ready.Status)
	return nil
}

func (m *Manager) drainRaw(enabled bool) {
	for _, guard := range m.raw.guards {
		guard.drain(enabled)
	}
	// A previous kernel's writer may still be draining these same FIFO inodes.
	// New kernel stdio is independent, but sandbox standby readers must wait.
	if enabled {
		active, _ := daemon.WriterActive(m.config.Directory)
		enabled = !active
	}
	for _, r := range m.registrations {
		for _, guard := range r.guards {
			if guard != nil {
				guard.drain(enabled)
			}
		}
	}
}

func (m *Manager) stopChild(graceful bool) error {
	child := m.child
	if child == nil {
		return nil
	}
	if graceful {
		_, _ = m.rpc(records.ControlRequest{Operation: "shutdown"})
	}
	_ = child.control.Close()
	var err error
	select {
	case err = <-child.wait:
	case <-time.After(2750 * time.Millisecond):
		_ = child.command.Process.Kill()
		select {
		case err = <-child.wait:
		case <-time.After(250 * time.Millisecond):
			err = errors.New("log writer did not exit after kill")
		}
	}
	m.child = nil
	m.drainRaw(true)
	return err
}

func (m *Manager) applyPending() {
	m.mu.Lock()
	p := m.pending
	m.pending = nil
	m.mu.Unlock()
	if p == nil || m.child == nil {
		return
	}
	if m.child.generation == p.generation {
		if _, err := m.rpc(records.ControlRequest{Operation: "commit", Preparation: p.token}); err == nil {
			m.child.policy = p.policy
			return
		} else {
			m.failure(err)
		}
	} else if m.child.policy == *m.policy.Load() {
		return
	}
	// Persistence already committed. A lost/expired preparation is recovered by
	// recreating logd with that complete desired policy, never rolling it back.
	_ = m.stopChild(false)
}

func (m *Manager) process(call managerCall) managerReply {
	if err := call.ctx.Err(); err != nil {
		return managerReply{err: err}
	}
	m.applyPending()
	request := call.request
	if request.Operation == "register" {
		return m.register(call.ctx, *request.Binding)
	}
	if request.Operation == "unregister" {
		if request.Binding == nil || request.Binding.ID != request.Producer {
			return managerReply{err: errors.New("sandbox log cleanup binding is required")}
		}
		r := m.registrations[request.Producer]
		if r == nil {
			return managerReply{err: m.retireUnadoptedRaw(*request.Binding)}
		}
		return managerReply{err: m.unregister(request.Producer)}
	}
	reply, err := m.rpc(request)
	if err != nil && m.child != nil && reply.ID == "" {
		m.failure(err)
		_ = m.stopChild(false)
	}
	if call.ctx.Err() != nil && request.Operation == "prepare" && err == nil {
		_, _ = m.rpc(records.ControlRequest{Operation: "discard", Preparation: reply.Preparation})
		return managerReply{err: call.ctx.Err()}
	}
	return managerReply{wire: reply, generation: m.generation, err: err}
}

func (m *Manager) register(ctx context.Context, binding records.Binding) managerReply {
	r := m.registrations[binding.ID]
	created := r == nil
	if r != nil {
		if r.retired || (r.binding.Token != "" && r.binding != binding) {
			return managerReply{err: errors.New("sandbox log identity is already registered")}
		}
		if r.binding == binding {
			return managerReply{wire: records.ControlReply{Paths: &r.paths}}
		}
		r.binding = binding
	} else {
		if len(m.registrations) >= m.config.MaxProducers {
			return managerReply{err: errors.New("log producer capacity exhausted")}
		}
		var err error
		r, err = m.prepareRaw(binding, true)
		if err != nil {
			return managerReply{err: err}
		}
		m.registrations[binding.ID] = r
	}
	if m.child != nil {
		for _, g := range r.guards {
			g.drain(false)
		}
		if _, err := m.rpc(records.ControlRequest{Operation: "register", Binding: &binding}); err != nil {
			m.failure(err)
			_ = m.stopChild(false)
		}
	}
	if err := ctx.Err(); err != nil {
		if created {
			_ = m.unregister(binding.ID)
		}
		return managerReply{err: err}
	}
	return managerReply{wire: records.ControlReply{Paths: &r.paths}}
}

func (m *Manager) unregister(id string) error {
	r := m.registrations[id]
	if r == nil {
		return nil
	}
	if m.child != nil && !r.retired {
		if _, err := m.rpc(records.ControlRequest{Operation: "unregister", Producer: id}); err != nil {
			m.failure(err)
			_ = m.stopChild(false)
		}
	}
	r.retired = true
	if err := r.remove(); err != nil {
		return err
	}
	delete(m.registrations, id)
	return nil
}

func (m *Manager) run() {
	defer close(m.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	start := func() {
		if err := m.start(); err != nil {
			m.failure(err)
			_ = m.stopChild(false)
			m.drainRaw(true)
		}
	}
	if err := m.adoptRaw(); err != nil {
		m.mu.Lock()
		m.status.RawRecoveryError = records.Text(err.Error(), 512)
		m.mu.Unlock()
		m.logger.Error("raw log recovery incomplete", "component", "logging", "error", err)
	}
	start()
	for {
		var exited <-chan error
		if m.child != nil {
			exited = m.child.wait
		}
		select {
		case <-m.stop:
			m.applyPending()
			err := m.stopChild(true)
			for _, r := range m.registrations {
				r.release()
			}
			for _, g := range m.raw.guards {
				g.close()
			}
			m.raw.closeConsole()
			m.mu.Lock()
			m.closeErr = errors.Join(m.closeErr, err)
			m.status.Running = false
			m.status.Available = false
			m.status.PID = 0
			m.mu.Unlock()
			return
		case err := <-exited:
			if err == nil {
				err = errors.New("log writer exited")
			}
			_ = m.child.control.Close()
			m.child = nil
			m.failure(err)
			m.drainRaw(true)
		case <-m.wake:
			m.applyPending()
		case call := <-m.commands:
			call.reply <- m.process(call)
		case <-tick.C:
			m.applyPending()
			if m.child == nil {
				start()
				continue
			}
			result, err := m.rpc(records.ControlRequest{Operation: "status"})
			if err != nil {
				m.failure(err)
				_ = m.stopChild(false)
			} else {
				m.snapshot(result.Status)
			}
		}
	}
}

// Close stops producer intake, releases native write descriptors for EOF, and
// lets the separate writer drain. Only this manager's child is killed after its
// bounded shutdown deadline.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.producer.close()
		err := m.raw.restore()
		m.mu.Lock()
		m.closeErr = err
		m.mu.Unlock()
		close(m.stop)
	})
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeErr
}
