package logging

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"the8020/kernel/logging/records"
)

const producerBytes = 128 * 1024

// producerClient owns one persistent kernel producer connection. Socket I/O is
// confined to its goroutine and never runs in a logging caller.
type producerClient struct {
	socket        string
	binding       records.Binding
	policy        *atomic.Pointer[records.Policy]
	policyMu      sync.Mutex
	remote        atomic.Pointer[records.ProducerPolicy]
	policyChanged chan struct{}
	queue         *records.Queue
	stop          chan struct{}
	done          chan struct{}
	stopOnce      sync.Once
	mu            sync.Mutex
	conn          net.Conn
	closed        atomic.Bool
	invalid       atomic.Uint64
}

func newProducer(socket string, binding records.Binding, policy *atomic.Pointer[records.Policy]) *producerClient {
	p := &producerClient{socket: socket, binding: binding, policy: policy, policyChanged: make(chan struct{}, 1), queue: records.NewQueue(producerBytes, records.MaxFrame+512, 512), stop: make(chan struct{}), done: make(chan struct{})}
	go p.run()
	return p
}

func (p *producerClient) allows(level string) bool {
	return !p.closed.Load() && p.policy.Load().Allows(level)
}
func (p *producerClient) emit(record records.Record) bool {
	if !p.allows(record.Level) {
		return true
	}
	payload, err := records.Marshal(record)
	if err != nil {
		p.invalid.Add(1)
		return false
	}
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	if !p.allows(record.Level) {
		return true
	}
	return p.queue.Add(records.Frame{Payload: payload, Source: record.Source, Level: record.Level})
}

func (p *producerClient) setPolicy(policy *records.Policy) {
	p.policyMu.Lock()
	p.policy.Store(policy)
	p.policyMu.Unlock()
}

func (p *producerClient) connect() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", p.socket)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
	hello, _ := json.Marshal(records.Hello{Version: records.ProtocolVersion, ID: p.binding.ID, Token: p.binding.Token})
	if err = records.WriteFrame(conn, hello); err == nil {
		var data []byte
		data, err = records.ReadFrame(conn)
		if err == nil {
			var policy records.ProducerPolicy
			if json.Unmarshal(data, &policy) != nil || !validProducerPolicy(policy) {
				err = errors.New("invalid log producer policy")
			} else {
				p.updatePolicy(policy)
			}
		}
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
	return conn, nil
}

func (p *producerClient) run() {
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			p.dropPending()
			return
		default:
		}
		conn, err := p.connect()
		if err != nil {
			// Keep only the existing byte-bounded queue while startup/reconnect is
			// pending. This preserves ordinary boot records without growing history.
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-p.stop:
				timer.Stop()
				p.dropPending()
				return
			case <-timer.C:
			}
			continue
		}
		p.connected(conn)
		p.remote.Store(nil)
		_ = conn.Close()
		p.mu.Lock()
		if p.conn == conn {
			p.conn = nil
		}
		p.mu.Unlock()
	}
}

func (p *producerClient) connected(conn net.Conn) {
	readDone := make(chan struct{})
	defer func() { _ = conn.Close(); <-readDone }()
	go func() {
		defer close(readDone)
		defer conn.Close()
		for {
			data, err := records.ReadFrame(conn)
			if err != nil {
				return
			}
			var update records.ProducerPolicy
			if json.Unmarshal(data, &update) != nil || !validProducerPolicy(update) {
				return
			}
			p.updatePolicy(update)
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var reported [4]uint64
	buffer := make([]byte, 0, records.BatchBytes)
	for {
		select {
		case <-readDone:
			return
		case <-p.stop:
			deadline := time.Now().Add(250 * time.Millisecond)
			for p.queue.Stats().PendingRecords > 0 && time.Now().Before(deadline) {
				if !p.policyReady() {
					timer := time.NewTimer(time.Until(deadline))
					select {
					case <-p.policyChanged:
						timer.Stop()
					case <-timer.C:
					}
					continue
				}
				if !p.writeBatch(conn, deadline, buffer) {
					break
				}
			}
			p.dropPending()
			p.report(conn, &reported)
			return
		case <-ticker.C:
			if !p.report(conn, &reported) {
				return
			}
		case <-p.queue.Notify():
			if !p.writeBatch(conn, time.Now().Add(250*time.Millisecond), buffer) {
				return
			}
		case <-p.policyChanged:
			if !p.writeBatch(conn, time.Now().Add(250*time.Millisecond), buffer) {
				return
			}
		}
	}
}

func (p *producerClient) writeBatch(conn net.Conn, deadline time.Time, buffer []byte) bool {
	// Capture may use newly committed settings before logd receives commit. In
	// particular an enabled/wider record must not hit the previous filter and
	// disappear. The writer's coalesced policy push is its acknowledgment.
	p.policyMu.Lock()
	policy := p.policy.Load()
	if !p.matchesPolicy(policy) {
		p.policyMu.Unlock()
		return true
	}
	batch := p.queue.Take(records.BatchBytes)
	p.policyMu.Unlock()
	defer batch.Release()
	if len(batch.Frames) == 0 {
		return true
	}
	buffer = buffer[:0]
	for _, frame := range batch.Frames {
		if !policy.Allows(frame.Level) {
			continue
		}
		buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(frame.Payload)))
		buffer = append(buffer, frame.Payload...)
	}
	_ = conn.SetWriteDeadline(deadline)
	if err := records.WriteAll(conn, buffer); err != nil {
		for _, f := range batch.Frames {
			if policy.Allows(f.Level) {
				// Without record acknowledgments delivery at this failed boundary
				// is uncertain; count the batch conservatively and never replay it.
				p.queue.Drop(f.Level, 1)
			}
		}
		return false
	}
	return true
}

func validProducerPolicy(p records.ProducerPolicy) bool {
	return p.Revision > 0 && (p.Level == "debug" || p.Level == "info" || p.Level == "warn" || p.Level == "error")
}
func (p *producerClient) updatePolicy(policy records.ProducerPolicy) {
	p.remote.Store(&policy)
	select {
	case p.policyChanged <- struct{}{}:
	default:
	}
}
func (p *producerClient) policyReady() bool {
	return p.matchesPolicy(p.policy.Load())
}
func (p *producerClient) matchesPolicy(desired *records.Policy) bool {
	remote := p.remote.Load()
	return remote != nil && remote.Enabled == desired.Enabled && remote.Level == desired.Level
}

func (p *producerClient) report(conn net.Conn, last *[4]uint64) bool {
	counts := p.queue.Stats().Dropped
	if counts == *last {
		return true
	}
	data, _ := json.Marshal(records.ProducerPacket{Dropped: &counts})
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if err := records.WriteFrame(conn, data); err != nil {
		return false
	}
	*last = counts
	return true
}

func (p *producerClient) dropPending() {
	for {
		batch := p.queue.Take(records.BatchBytes)
		if len(batch.Frames) == 0 {
			batch.Release()
			return
		}
		for _, f := range batch.Frames {
			if p.policy.Load().Allows(f.Level) {
				p.queue.Drop(f.Level, 1)
			}
		}
		batch.Release()
	}
}

func (p *producerClient) close() {
	p.stopOnce.Do(func() {
		p.policyMu.Lock()
		p.closed.Store(true)
		close(p.stop)
		p.policyMu.Unlock()
	})
	select {
	case <-p.done:
		return
	case <-time.After(500 * time.Millisecond):
	}
	p.mu.Lock()
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(300 * time.Millisecond):
	}
}
