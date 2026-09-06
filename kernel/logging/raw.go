package logging

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"the8020/kernel/identity"
	"the8020/kernel/logging/records"
)

// rawGuard keeps a read endpoint alive across logd death. While healthy it does
// no reads; during an outage it discards bytes using one fixed 4 KiB scratch.
// Taking mu for each nonblocking read makes pausing an acknowledged handoff.
type rawGuard struct {
	file             *os.File
	fd               int
	mu               sync.Mutex
	active, closed   bool
	wake, stop, done chan struct{}
	dropped          *atomic.Uint64
}

func newRawGuard(file *os.File, dropped *atomic.Uint64) *rawGuard {
	g := &rawGuard{file: file, fd: int(file.Fd()), wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), dropped: dropped}
	_ = syscall.SetNonblock(g.fd, true)
	go g.run()
	return g
}
func (g *rawGuard) drain(enabled bool) {
	g.mu.Lock()
	if !g.closed {
		// exec inheritance may put the shared description in blocking mode.
		_ = syscall.SetNonblock(g.fd, true)
		g.active = enabled
	}
	g.mu.Unlock()
	select {
	case g.wake <- struct{}{}:
	default:
	}
}
func (g *rawGuard) run() {
	defer close(g.done)
	buffer := make([]byte, 4096)
	for {
		select {
		case <-g.stop:
			return
		case <-g.wake:
		}
		for {
			g.mu.Lock()
			if g.closed || !g.active {
				g.mu.Unlock()
				break
			}
			n, err := syscall.Read(g.fd, buffer)
			g.mu.Unlock()
			if n > 0 {
				g.dropped.Add(uint64(n))
				continue
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-g.stop:
				timer.Stop()
				return
			case <-g.wake:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}
func (g *rawGuard) close() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		close(g.stop)
		_ = g.file.Close()
	}
	g.mu.Unlock()
	<-g.done
}

type standardCapture struct {
	guards        [2]*rawGuard
	writes, saved [2]*os.File
	mu            sync.Mutex
	captured      bool
}

func captureStandard(enabled bool, dropped *atomic.Uint64) (_ *standardCapture, err error) {
	c := &standardCapture{}
	defer func() {
		if err != nil {
			_ = c.restore()
			for _, g := range c.guards {
				if g != nil {
					g.close()
				}
			}
			c.closeConsole()
		}
	}()
	for i := range c.guards {
		read, write, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		c.guards[i], c.writes[i] = newRawGuard(read, dropped), write
		if !enabled {
			_ = write.Close()
			c.writes[i] = nil
			continue
		}
		fd, dupErr := unix.FcntlInt(uintptr(i+1), unix.F_DUPFD_CLOEXEC, 3)
		if dupErr != nil {
			return nil, dupErr
		}
		c.saved[i] = os.NewFile(uintptr(fd), "kernel-console")
	}
	if enabled {
		c.captured = true
		for i, write := range c.writes {
			if err := unix.Dup3(int(write.Fd()), i+1, 0); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

func (c *standardCapture) console() (*os.File, *os.File) {
	if c.saved[0] != nil && c.saved[1] != nil {
		return c.saved[0], c.saved[1]
	}
	return os.Stdout, os.Stderr
}
func (c *standardCapture) restore() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if c.captured {
		for i, saved := range c.saved {
			if saved != nil {
				err = errors.Join(err, unix.Dup3(int(saved.Fd()), i+1, 0))
			}
		}
		c.captured = false
	}
	for i, write := range c.writes {
		if write != nil {
			err = errors.Join(err, write.Close())
			c.writes[i] = nil
		}
	}
	return err
}
func (c *standardCapture) closeConsole() {
	for _, f := range c.saved {
		if f != nil {
			_ = f.Close()
		}
	}
}

// Adopt only this protocol's FIFO names, once at logging startup and before
// database/runtime recovery. Empty tokens permit raw reading but never a socket
// connection; the lifecycle owner later supplies each existing supervisor token.
func (m *Manager) adoptRaw() error {
	remaining := 2*m.config.MaxProducers + 32
	dir := (records.Binding{}).IngressDirectory(m.config.Socket)
	file, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var firstError error
	for remaining > 0 {
		entries, err := file.ReadDir(min(128, remaining))
		remaining -= len(entries)
		for _, entry := range entries {
			id, found := strings.CutSuffix(entry.Name(), "-stdout")
			if !found {
				id, found = strings.CutSuffix(entry.Name(), "-stderr")
			}
			if !found || !identity.Is(id, "sbx") {
				continue
			}
			if old := m.registrations[id]; old != nil {
				continue
			}
			if len(m.registrations) >= m.config.MaxProducers {
				return errors.New("raw log recovery exceeds configured live producer capacity")
			}
			r, err := m.prepareRaw(records.Binding{ID: id}, false)
			if err != nil {
				if firstError == nil {
					firstError = err
				}
				continue
			}
			m.registrations[id] = r
		}
		if errors.Is(err, io.EOF) {
			return firstError
		}
		if err != nil {
			return err
		}
	}
	return errors.New("raw log recovery directory exceeds the bounded startup scan")
}

func (m *Manager) prepareRaw(binding records.Binding, drain bool) (_ *registration, err error) {
	dir := binding.IngressDirectory(m.config.Socket)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	r := &registration{binding: binding, paths: binding.IngressPaths(m.config.Socket)}
	var created [2]os.FileInfo
	defer func() {
		if err != nil {
			for i, path := range []string{r.paths.Stdout, r.paths.Stderr} {
				if created[i] != nil {
					err = errors.Join(err, unlinkSame(path, created[i]))
				}
			}
			r.release()
		}
	}()
	for i, path := range []string{r.paths.Stdout, r.paths.Stderr} {
		if createErr := syscall.Mkfifo(path, 0600); createErr == nil {
			created[i], err = os.Lstat(path)
			if err != nil {
				return nil, err
			}
		} else if !errors.Is(createErr, syscall.EEXIST) {
			return nil, createErr
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			_ = file.Close()
			return nil, errors.New("log ingress endpoint is not a FIFO")
		}
		r.guards[i] = newRawGuard(file, &m.rawDropped)
		r.guards[i].drain(drain)
	}
	return r, nil
}

func unlinkSame(path string, opened os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if os.SameFile(opened, current) {
		return os.Remove(path)
	}
	return nil
}

// A lower producer limit can leave inherited FIFOs outside the bounded startup
// adoption. Confirmed native deletion still retires that owner's two endpoints
// without allocating a registration or starting additional standby readers.
func (m *Manager) retireUnadoptedRaw(binding records.Binding) error {
	if !identity.Is(binding.ID, "sbx") {
		return errors.New("invalid sandbox log cleanup binding")
	}
	directory, err := os.OpenFile(binding.IngressDirectory(m.config.Socket), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	var result error
	for _, stream := range []string{"stdout", "stderr"} {
		name := binding.ID + "-" + stream
		fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		var opened, current unix.Stat_t
		err = unix.Fstat(fd, &opened)
		if err == nil && opened.Mode&unix.S_IFMT != unix.S_IFIFO {
			err = errors.New("log ingress endpoint is not a FIFO")
		}
		if err == nil {
			err = unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW)
			if err == nil && opened.Dev == current.Dev && opened.Ino == current.Ino {
				err = unix.Unlinkat(int(directory.Fd()), name, 0)
			}
		}
		_ = unix.Close(fd)
		if !errors.Is(err, unix.ENOENT) {
			result = errors.Join(result, err)
		}
	}
	return result
}

// remove is called only after native deletion and logd's available-tail drain.
// Retain failed endpoints for explicit cleanup retry rather than losing ownership.
func (r *registration) remove() error {
	var result error
	for i, g := range r.guards {
		if g == nil {
			continue
		}
		path := []string{r.paths.Stdout, r.paths.Stderr}[i]
		opened, err := g.file.Stat()
		if err == nil {
			err = unlinkSame(path, opened)
		}
		result = errors.Join(result, err)
		if err == nil {
			g.close()
			r.guards[i] = nil
		}
	}
	return result
}

// Process shutdown releases descriptors without unlinking still-registered
// FIFOs: only the sandbox lifecycle owner knows native cleanup is complete.
func (r *registration) release() {
	for i, guard := range r.guards {
		if guard != nil {
			guard.close()
			r.guards[i] = nil
		}
	}
}
