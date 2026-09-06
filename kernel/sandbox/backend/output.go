package backend

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"unicode/utf8"
)

func OpenRawOutput(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute raw output FIFO is required")
	}
	// Nonblocking open fails promptly if the ingress reader is absent. Native
	// stdout uses ordinary blocking descriptors once the owned FIFO is verified.
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open raw output FIFO: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		_ = file.Close()
		return nil, errors.New("raw output must be a FIFO")
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// OutputBuffer retains the beginning and end in fixed storage. Truncated
// control stdout is rejected instead of attempting to parse an incomplete JSON.
type OutputBuffer struct {
	head, tail []byte
	total      uint64
	limit      int
}

func NewOutputBuffer(limit int) *OutputBuffer {
	return &OutputBuffer{head: make([]byte, 0, limit/2), tail: make([]byte, 0, limit-limit/2), limit: limit}
}

func (b *OutputBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if uint64(n) > ^uint64(0)-b.total {
		b.total = ^uint64(0)
	} else {
		b.total += uint64(n)
	}
	if remaining := cap(b.head) - len(b.head); remaining > 0 {
		count := min(len(data), remaining)
		b.head = append(b.head, data[:count]...)
		data = data[count:]
	}
	if len(data) >= cap(b.tail) {
		b.tail = append(b.tail[:0], data[len(data)-cap(b.tail):]...)
	} else {
		if excess := len(b.tail) + len(data) - cap(b.tail); excess > 0 {
			copy(b.tail, b.tail[excess:])
			b.tail = b.tail[:len(b.tail)-excess]
		}
		b.tail = append(b.tail, data...)
	}
	return n, nil
}

func (b *OutputBuffer) Truncated() bool { return b.total > uint64(b.limit) }
func (b *OutputBuffer) Bytes() []byte {
	result := make([]byte, 0, b.limit+64)
	head, tail := b.head, b.tail
	if b.Truncated() {
		start := len(head) - 1
		for start > 0 && head[start]&0xc0 == 0x80 {
			start--
		}
		if start >= 0 && !utf8.FullRune(head[start:]) {
			head = head[:start]
		}
		for len(tail) > 0 && tail[0]&0xc0 == 0x80 {
			tail = tail[1:]
		}
	}
	result = append(result, head...)
	if b.Truncated() {
		result = fmt.Appendf(result, " ...[%d bytes omitted]... ", b.total-uint64(len(head)+len(tail)))
	}
	return append(result, tail...)
}

func (b *OutputBuffer) Total() uint64 { return b.total }
