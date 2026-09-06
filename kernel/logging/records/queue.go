package records

import (
	"container/list"
	"sync"
)

// Frame is already bounded and encoded for its next hop. The queue takes
// ownership of Payload; producers must not mutate it after admission.
type Frame struct {
	Payload       []byte
	Source, Level string
}
type queuedFrame struct {
	Frame
	order uint64
}

type QueueStats struct {
	PendingBytes   int       `json:"pending_bytes"`
	PendingRecords int       `json:"pending_records"`
	Dropped        [4]uint64 `json:"dropped"`
}

// Queue accounts for both waiting and currently consumed batches. A stalled
// socket/file write cannot release credit and hide another unbounded buffer.
// It reserves bytes and record slots for warnings/errors and evicts waiting
// low-severity records before dropping a newly arriving warning/error.
type Queue struct {
	mu                      sync.Mutex
	low, high               list.List
	maximum, reserve, slots int
	stats                   QueueStats
	order                   uint64
	notify                  chan struct{}
}

func NewQueue(maximum, reserve, slots int) *Queue {
	if maximum < MaxRecord || reserve < 0 || reserve >= maximum || slots < 4 {
		panic("invalid logging queue bounds")
	}
	return &Queue{maximum: maximum, reserve: reserve, slots: slots, notify: make(chan struct{}, 1)}
}

func (q *Queue) Notify() <-chan struct{} { return q.notify }

func (q *Queue) Add(frame Frame) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	level := Severity(frame.Level)
	if level < 0 || len(frame.Payload) == 0 || len(frame.Payload) > MaxFrame {
		return false
	}
	weight := len(frame.Payload) + 4
	limit, slots := q.maximum, q.slots
	if level < 2 {
		limit -= q.reserve
		slots -= max(1, q.slots/4)
	} else {
		for (q.stats.PendingBytes+weight > limit || q.stats.PendingRecords >= slots) && q.low.Len() > 0 {
			e := q.low.Front()
			old := e.Value.(queuedFrame)
			q.low.Remove(e)
			q.stats.PendingBytes -= len(old.Payload) + 4
			q.stats.PendingRecords--
			q.stats.Dropped[Severity(old.Level)]++
		}
	}
	if q.stats.PendingBytes+weight > limit || q.stats.PendingRecords >= slots {
		q.stats.Dropped[level]++
		return false
	}
	q.order++
	item := queuedFrame{Frame: frame, order: q.order}
	if level < 2 {
		q.low.PushBack(item)
	} else {
		q.high.PushBack(item)
	}
	q.stats.PendingBytes += weight
	q.stats.PendingRecords++
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

// Batch has one consumer; Release must be called after the downstream operation
// finishes, including failures. Internal ordering is not persisted in records.
type Batch struct {
	Frames   []Frame
	bytes    int
	queue    *Queue
	released bool
}

func (q *Queue) Take(maximum int) *Batch {
	q.mu.Lock()
	defer q.mu.Unlock()
	b := &Batch{queue: q}
	for {
		low, high := q.low.Front(), q.high.Front()
		var selected *list.Element
		owner := &q.low
		if low == nil {
			selected = high
			owner = &q.high
		} else if high == nil || low.Value.(queuedFrame).order < high.Value.(queuedFrame).order {
			selected = low
		} else {
			selected = high
			owner = &q.high
		}
		if selected == nil {
			break
		}
		item := selected.Value.(queuedFrame)
		weight := len(item.Payload) + 4
		if b.bytes+weight > maximum {
			break
		}
		owner.Remove(selected)
		b.Frames = append(b.Frames, item.Frame)
		b.bytes += weight
	}
	return b
}

func (b *Batch) Release() {
	q := b.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if b.released {
		return
	}
	b.released = true
	q.stats.PendingBytes -= b.bytes
	q.stats.PendingRecords -= len(b.Frames)
	clear(b.Frames)
	b.Frames = nil
	if q.low.Len()+q.high.Len() > 0 {
		select {
		case q.notify <- struct{}{}:
		default:
		}
	}
}

func (q *Queue) Stats() QueueStats { q.mu.Lock(); defer q.mu.Unlock(); return q.stats }

// Drop accounts for losses after admission (for example a failed socket write).
func (q *Queue) Drop(level string, count uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := Severity(level); n >= 0 {
		q.stats.Dropped[n] += count
	}
}
