package records

import (
	"bytes"
	"testing"
)

func TestQueueRetainsCreditWhileDownstreamStalls(t *testing.T) {
	q := NewQueue(64*1024, 16*1024, 16)
	frame := Frame{Payload: bytes.Repeat([]byte{'x'}, 12000), Level: "INFO", Source: "kernel"}
	for i := 0; i < 4; i++ {
		if !q.Add(frame) {
			t.Fatal("unexpected initial drop")
		}
	}
	b := q.Take(64 * 1024)
	if len(b.Frames) != 4 || q.Stats().PendingBytes != 4*(12000+4) {
		t.Fatal("in-flight accounting lost")
	}
	for i := 0; i < 10000; i++ {
		if q.Add(frame) {
			t.Fatal("stalled downstream released hidden credit")
		}
	}
	errorFrame := frame
	errorFrame.Level = "ERROR"
	if !q.Add(errorFrame) {
		t.Fatal("warning/error reserve unavailable")
	}
	if q.Add(errorFrame) {
		t.Fatal("sustained error flood exceeded bound")
	}
	stats := q.Stats()
	if stats.PendingBytes > 64*1024 || stats.Dropped[1] != 10000 || stats.Dropped[3] != 1 {
		t.Fatal(stats)
	}
	b.Release()
	b.Release()
	last := q.Take(64 * 1024)
	if len(last.Frames) != 1 || last.Frames[0].Level != "ERROR" {
		t.Fatal("reserved error lost")
	}
	last.Release()
	if q.Stats().PendingBytes != 0 || q.Stats().PendingRecords != 0 {
		t.Fatal("credit leaked")
	}
}

func TestQueueShedsWaitingInfoBeforeErrorsAndPreservesArrivalOrder(t *testing.T) {
	q := NewQueue(32*1024, 8*1024, 8)
	makeFrame := func(level, marker string) Frame {
		return Frame{Payload: append([]byte(marker), bytes.Repeat([]byte{'x'}, 7000)...), Level: level}
	}
	for _, f := range []Frame{makeFrame("INFO", "1"), makeFrame("WARN", "2"), makeFrame("DEBUG", "3"), makeFrame("ERROR", "4"), makeFrame("ERROR", "5")} {
		if !q.Add(f) {
			t.Fatal("high severity did not evict pending info")
		}
	}
	b := q.Take(64 * 1024)
	defer b.Release()
	want := []byte{'2', '3', '4', '5'}
	if len(b.Frames) != len(want) {
		t.Fatal(len(b.Frames))
	}
	for i, f := range b.Frames {
		if f.Payload[0] != want[i] {
			t.Fatal("changed surviving arrival order")
		}
	}
	if q.Stats().Dropped[1] != 1 {
		t.Fatal("eviction not counted")
	}
}
