package daemon

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/logging/records"
)

type producer struct {
	binding    records.Binding
	paths      records.RawPaths
	raw        []*os.File
	connection *producerConnection
	dropped    [4]uint64
	closing    bool
	readers    sync.WaitGroup
}

type producerConnection struct {
	conn    *net.UnixConn
	updates chan records.ProducerPolicy
	done    chan struct{}
	once    sync.Once
}

func (c *producerConnection) close() { c.once.Do(func() { close(c.done); _ = c.conn.Close() }) }
func (c *producerConnection) update(p records.ProducerPolicy) {
	select {
	case c.updates <- p:
		return
	default:
	}
	select {
	case <-c.updates:
	default:
	}
	select {
	case c.updates <- p:
	default:
	}
}

func (s *server) register(binding records.Binding) (records.RawPaths, error) {
	if binding.ID != s.init.NodeID && !identity.Is(binding.ID, "sbx") {
		return records.RawPaths{}, errors.New("invalid log producer identity")
	}
	// Only inherited kernel control can adopt raw FIFOs before runtime recovery
	// knows the original credential. Such a binding cannot authenticate a socket.
	if binding.Token != "" || binding.ID == s.init.NodeID {
		token, err := hex.DecodeString(binding.Token)
		if len(binding.Token) != 64 || err != nil || len(token) != 32 {
			return records.RawPaths{}, errors.New("invalid log producer credential")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.producers[binding.ID]; old != nil {
		if (old.binding.Token != "" && old.binding != binding) || old.closing {
			return records.RawPaths{}, errors.New("log producer identity is already registered")
		}
		old.binding.Token = binding.Token
		return old.paths, nil
	}
	if len(s.producers) >= s.init.MaxProducers {
		return records.RawPaths{}, errors.New("log producer capacity exhausted")
	}
	p := &producer{binding: binding}
	if identity.Is(binding.ID, "sbx") {
		p.paths = binding.IngressPaths(s.init.Socket)
		for _, path := range []string{p.paths.Stdout, p.paths.Stderr} {
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				for _, f := range p.raw {
					_ = f.Close()
				}
				return records.RawPaths{}, errors.New("log ingress path is not a FIFO")
			}
			f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
			if err != nil {
				for _, open := range p.raw {
					_ = open.Close()
				}
				return records.RawPaths{}, err
			}
			p.raw = append(p.raw, f)
		}
	}
	s.producers[binding.ID] = p
	for i, f := range p.raw {
		stream := []string{"stdout", "stderr"}[i]
		p.readers.Add(1)
		s.inputs.Add(1)
		go func() { defer p.readers.Done(); defer s.inputs.Add(-1); s.raw(f, "deno", binding.ID, stream) }()
	}
	return p.paths, nil
}

func (s *server) unregister(id string) error {
	s.mu.Lock()
	p := s.producers[id]
	if p == nil {
		s.mu.Unlock()
		return nil
	}
	p.closing = true
	deadline := time.Now().Add(25 * time.Millisecond)
	if p.connection != nil {
		_ = p.connection.conn.SetDeadline(deadline)
	}
	for _, f := range p.raw {
		_ = f.SetReadDeadline(deadline)
	}
	s.mu.Unlock()
	// The native process must already have stopped. Let the available tail pass
	// before closing/reusing its FIFO inode, including its final unfinished line.
	p.readers.Wait()
	s.mu.Lock()
	if p.connection != nil {
		p.connection.close()
	}
	delete(s.producers, id)
	s.mu.Unlock()
	// The kernel owns FIFO lifetime and its standby descriptor. Acknowledging
	// unregister lets that owner safely unlink the same inode after this drain.
	return nil
}

func (s *server) accept() {
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case s.handshakes <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		select {
		case s.connectionSlots <- struct{}{}:
		default:
			<-s.handshakes
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		s.connections[conn] = true
		s.mu.Unlock()
		s.inputs.Add(1)
		go func() {
			var handshakeOnce sync.Once
			releaseHandshake := func() { handshakeOnce.Do(func() { <-s.handshakes }) }
			defer releaseHandshake()
			defer func() {
				_ = conn.Close()
				s.mu.Lock()
				delete(s.connections, conn)
				s.mu.Unlock()
				<-s.connectionSlots
				s.inputs.Add(-1)
			}()
			s.consume(conn, releaseHandshake)
		}()
	}
}

func (s *server) consume(conn *net.UnixConn, releaseHandshake func()) {
	_ = conn.SetReadBuffer(records.MaxFrame)
	_ = conn.SetWriteBuffer(records.MaxFrame)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	data, err := records.ReadFrame(conn)
	if err != nil {
		return
	}
	var hello records.Hello
	if len(data) > 1024 || json.Unmarshal(data, &hello) != nil || hello.Version != records.ProtocolVersion {
		return
	}
	s.mu.Lock()
	p := s.producers[hello.ID]
	if p == nil || p.closing || len(p.binding.Token) != 64 || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(p.binding.Token)) != 1 {
		s.mu.Unlock()
		return
	}
	if p.connection != nil {
		p.connection.close()
	}
	c := &producerConnection{conn: conn, updates: make(chan records.ProducerPolicy, 1), done: make(chan struct{})}
	p.connection = c
	s.mu.Unlock()
	releaseHandshake()
	defer c.close()
	defer func() {
		s.mu.Lock()
		if p.connection == c {
			p.connection = nil
		}
		s.mu.Unlock()
	}()
	_ = conn.SetDeadline(time.Time{})
	c.update(s.producerPolicy())
	go func() {
		for {
			select {
			case <-c.done:
				return
			case update := <-c.updates:
				data, _ := json.Marshal(update)
				_ = conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
				if records.WriteFrame(conn, data) != nil {
					c.close()
					return
				}
			}
		}
	}()
	for {
		data, err = records.ReadFrame(conn)
		if err != nil {
			return
		}
		var packet records.ProducerPacket
		if json.Unmarshal(data, &packet) != nil || packet.Record == nil && packet.Dropped == nil {
			s.invalid.Add(1)
			return
		}
		if packet.Dropped != nil {
			s.mu.Lock()
			for i, current := range *packet.Dropped {
				previous := p.dropped[i]
				delta := current
				if current >= previous {
					delta = current - previous
				}
				s.upstream[i].Add(delta)
				p.dropped[i] = current
			}
			s.mu.Unlock()
		}
		if packet.Record == nil {
			continue
		}
		r := *packet.Record
		source := "kernel"
		sandbox := ""
		if identity.Is(p.binding.ID, "sbx") {
			source = "deno"
			sandbox = p.binding.ID
		}
		if r.NodeID != "" && r.NodeID != s.init.NodeID || r.Source != "" && r.Source != source || sandbox != "" && r.SandboxID != "" && r.SandboxID != sandbox {
			s.invalid.Add(1)
			continue
		}
		r.NodeID = s.init.NodeID
		r.Source = source
		if sandbox != "" {
			r.SandboxID = sandbox
		}
		s.emit(r)
	}
}

func (s *server) raw(reader io.ReadCloser, source, sandbox, stream string) {
	defer reader.Close()
	level := "INFO"
	if stream == "stderr" {
		level = "ERROR"
	}
	lines := records.NewLines(func(message string, partial bool) {
		r := records.Record{Time: time.Now().UTC(), Level: level, Source: source, Component: "raw", NodeID: s.init.NodeID, SandboxID: sandbox, Stream: stream, Message: message}
		if partial {
			r.Attributes = map[string]string{"unterminated": "true"}
		}
		s.emit(r)
	})
	defer lines.EOF()
	var buf [16 * 1024]byte
	for {
		n, err := reader.Read(buf[:])
		if n > 0 {
			lines.Feed(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *server) emit(r records.Record) {
	if !s.policy.Load().Allows(r.Level) {
		return
	}
	line, err := records.Encode(r)
	if err != nil {
		s.invalid.Add(1)
		return
	}
	s.accepted.Add(1)
	s.queue.Add(records.Frame{Payload: line, Source: r.Source, Level: r.Level})
}

func (s *server) producerPolicy() records.ProducerPolicy {
	p := s.policy.Load()
	return records.ProducerPolicy{Enabled: p.Enabled, Level: p.Level, Revision: s.revision.Load()}
}
func (s *server) broadcast() {
	p := s.producerPolicy()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, producer := range s.producers {
		if producer.connection != nil && !producer.closing {
			producer.connection.update(p)
		}
	}
}

func (s *server) closeInputs() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.connections {
		_ = conn.Close()
	}
	for _, p := range s.producers {
		for _, f := range p.raw {
			_ = f.Close()
		}
	}
	for _, f := range s.kernelRaw {
		if f != nil {
			_ = f.Close()
		}
	}
}
