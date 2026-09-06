package records

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

const rawHalf = 6 * 1024

// Lines frames arbitrary byte fragments without retaining an unbounded unfinished
// line. Its owner supplies fixed raw-stream metadata and a nonblocking emit path.
// It is confined to one reader. EOF emits any final unterminated line.
type Lines struct {
	head                 [rawHalf]byte
	tail                 [rawHalf]byte
	headN, tailN, tailAt int
	omitted              bool
	emit                 func(message string, unterminated bool)
}

func NewLines(emit func(string, bool)) *Lines { return &Lines{emit: emit} }

func (l *Lines) Feed(p []byte) {
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			l.append(p)
			return
		}
		l.append(p[:end])
		l.flush(false)
		p = p[end+1:]
	}
}

func (l *Lines) append(p []byte) {
	if l.headN < rawHalf {
		n := copy(l.head[l.headN:], p)
		l.headN += n
		p = p[n:]
	}
	if len(p) == 0 {
		return
	}
	if len(p) > rawHalf-l.tailN {
		l.omitted = true
	}
	if len(p) >= rawHalf {
		copy(l.tail[:], p[len(p)-rawHalf:])
		l.tailN = rawHalf
		l.tailAt = 0
		return
	}
	n := copy(l.tail[l.tailAt:], p)
	copy(l.tail[:], p[n:])
	l.tailAt = (l.tailAt + len(p)) % rawHalf
	l.tailN = min(rawHalf, l.tailN+len(p))
}

func (l *Lines) EOF() {
	if l.headN+l.tailN > 0 {
		l.flush(true)
	}
}

func (l *Lines) flush(unterminated bool) {
	head := l.head[:l.headN]
	tail := make([]byte, 0, l.tailN)
	if l.tailN == rawHalf {
		tail = append(tail, l.tail[l.tailAt:]...)
		tail = append(tail, l.tail[:l.tailAt]...)
	} else {
		tail = append(tail, l.tail[:l.tailN]...)
	}
	var value string
	if l.omitted {
		// Trim only fragments created by the middle cut; actual malformed input is
		// still represented by replacement characters during bounded formatting.
		start := len(head) - 1
		for start > 0 && head[start]&0xc0 == 0x80 {
			start--
		}
		if !utf8.FullRune(head[start:]) {
			head = head[:start]
		}
		for len(tail) > 0 && tail[0]&0xc0 == 0x80 {
			tail = tail[1:]
		}
		value = string(head) + Omitted + string(tail)
	} else {
		value = string(append(head[:len(head):len(head)], tail...))
	}
	value = strings.ToValidUTF8(value, "�")
	l.headN, l.tailN, l.tailAt = 0, 0, 0
	l.omitted = false
	l.emit(value, unterminated)
}
