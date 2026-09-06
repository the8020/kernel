package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"the8020/kernel/logging/records"
)

var streamNames = [4]string{"all", "kernel", "deno", "logd"}

type position struct {
	Segment string `json:"s,omitempty"`
	Offset  int64  `json:"o,omitempty"`
	Skip    bool   `json:"k,omitempty"`
}
type cursor struct {
	Version   int         `json:"v"`
	Filter    string      `json:"f"`
	Positions [4]position `json:"p"`
}

func filterKey(f records.Filter) string {
	encoded, _ := json.Marshal(f)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:12])
}
func encodeCursor(c cursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}
func decodeCursor(raw string, key string) (cursor, error) {
	c := cursor{Version: 1, Filter: key}
	if raw == "" {
		return c, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, errors.New("invalid log cursor")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil || c.Version != 1 || c.Filter != key {
		return c, errors.New("log cursor does not match this query")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("invalid log cursor suffix")
	}
	for _, p := range c.Positions {
		if p.Offset < 0 {
			return c, errors.New("invalid log cursor offset")
		}
		if p.Segment != "" {
			if _, err := segmentNumber(p.Segment); err != nil {
				return c, err
			}
		} else if p.Offset != 0 || p.Skip {
			return c, errors.New("log cursor offset lacks a segment")
		}
	}
	return c, nil
}
func segmentNumber(s string) (uint64, error) {
	if len(s) != 20 {
		return 0, errors.New("invalid log cursor segment")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid log cursor segment")
		}
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid log cursor segment")
	}
	return id, nil
}
func segmentText(id uint64) string { return fmt.Sprintf("%020d", id) }

// readPosition is a filter-independent boundary for future execution views.
// Publishing four positions in cached status avoids a query/RPC per execution.
// The snapshot may precede the invocation; its first record must be emitted
// after the invocation owner takes the reference.
func (s *store) readPosition() string {
	c := cursor{Version: 1}
	for i, name := range streamNames {
		if list := s.streams[name]; list != nil && list.Back() != nil {
			seg := list.Back().Value.(*segment)
			c.Positions[i] = position{Segment: segmentText(seg.id), Offset: seg.size}
		}
	}
	return encodeCursor(c)
}

type streamReader struct {
	committed             *position
	seg                   *segment
	offset                int64
	file                  *os.File
	reader                *bufio.Reader
	head                  *records.LocatedRecord
	after                 position
	done, align, skipping bool
}

func (r *streamReader) close() {
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
		r.reader = nil
	}
}
func (r *streamReader) open() error {
	f, err := os.Open(r.seg.path)
	if err != nil {
		return err
	}
	r.file = f
	r.reader = bufio.NewReaderSize(io.NewSectionReader(f, r.offset, r.seg.size-r.offset), records.MaxRecord+1)
	return nil
}

func (r *streamReader) advanceSegment() bool {
	r.close()
	next := r.seg.streamOrder.Next()
	if next == nil {
		r.done = true
		return false
	}
	r.seg = next.Value.(*segment)
	r.offset = 0
	r.skipping = false
	*r.committed = position{Segment: segmentText(r.seg.id)}
	return true
}

// fill peeks one matching record, retaining its predecessor cursor until that
// record is returned. Filtered/corrupt bytes still advance the bounded scan.
func (r *streamReader) fill(ctx context.Context, filter records.Filter, page *records.Page) error {
	for r.head == nil && !r.done && !page.More && page.ScannedBytes < records.MaxQueryScan {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.offset >= r.seg.size {
			if !r.advanceSegment() {
				return nil
			}
		}
		if r.reader == nil {
			if err := r.open(); err != nil {
				return err
			}
		}
		start := r.offset
		line, err := r.reader.ReadSlice('\n')
		// Never consume more than the scan budget: retry this bounded fragment on
		// the next query with its unchanged public cursor.
		if page.ScannedBytes+len(line) > records.MaxQueryScan {
			r.close()
			page.More = true
			return nil
		}
		page.ScannedBytes += len(line)
		r.offset += int64(len(line))
		r.after = position{Segment: segmentText(r.seg.id), Offset: r.offset}
		if r.align {
			r.align = err == bufio.ErrBufferFull
			r.after.Skip = r.align
			*r.committed = r.after
			if err == io.EOF {
				r.done = !r.advanceSegment()
			}
			continue
		}
		if err == bufio.ErrBufferFull {
			if !r.skipping {
				page.CorruptRecords++
			}
			r.skipping = true
			r.after.Skip = true
			*r.committed = r.after
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		if r.skipping {
			r.skipping = false
			*r.committed = r.after
			continue
		}
		if err == io.EOF {
			if len(line) > 0 {
				page.CorruptRecords++
			}
			*r.committed = r.after
			continue
		}
		record, decodeErr := records.Decode(line)
		if decodeErr != nil {
			page.CorruptRecords++
			*r.committed = r.after
			continue
		}
		if !filter.Matches(record) {
			*r.committed = r.after
			continue
		}
		r.head = &records.LocatedRecord{Record: record, Segment: segmentText(r.seg.id), Offset: start}
	}
	return nil
}

func (s *store) query(ctx context.Context, input records.Query) (records.Page, error) {
	page := records.Page{State: "ok", Records: make([]records.LocatedRecord, 0)}
	q, err := input.Normalize()
	if err != nil {
		return page, err
	}
	c, err := decodeCursor(q.Cursor, filterKey(q.Filter))
	if err != nil {
		return page, err
	}
	if q.Position != "" {
		c, err = decodeCursor(q.Position, "")
		if err != nil {
			return page, err
		}
		c.Filter = filterKey(q.Filter)
	}
	var readers []*streamReader
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	for i, name := range streamNames {
		if q.Source != "" && name != "all" && name != q.Source {
			continue
		}
		list := s.streams[name]
		p := &c.Positions[i]
		var seg *segment
		if p.Segment != "" {
			id, _ := segmentNumber(p.Segment)
			seg = s.segments[id]
			if seg == nil {
				page.State = "expired"
				page.Reason = "A referenced log segment has expired."
				return page, nil
			}
			if seg.stream != name || p.Offset > seg.size {
				return page, errors.New("invalid log cursor position")
			}
		} else if list != nil && list.Front() != nil {
			seg = list.Front().Value.(*segment)
		}
		if seg == nil {
			continue
		}
		align := false
		if q.Tail {
			// At most 256 KiB of physical suffix per stream is materialized. Earlier
			// matches outside that window are explicitly reported as limited.
			remaining := int64(records.MaxQueryBytes)
			for e := list.Back(); e != nil; e = e.Prev() {
				candidate := e.Value.(*segment)
				seg = candidate
				if candidate.size >= remaining {
					p.Offset = candidate.size - remaining
					align = p.Offset > 0
					break
				}
				remaining -= candidate.size
				if e.Prev() == nil {
					p.Offset = 0
				}
			}
			page.TailLimited = page.TailLimited || align || seg.streamOrder.Prev() != nil
		}
		*p = position{Segment: segmentText(seg.id), Offset: p.Offset, Skip: p.Skip || align}
		r := &streamReader{committed: p, seg: seg, offset: p.Offset, align: align, skipping: p.Skip && !align}
		readers = append(readers, r)
		if p.Offset > 0 && !align && !p.Skip {
			if err := r.open(); err != nil {
				page.State = "unavailable"
				page.Reason = "A log segment is unavailable."
				return page, nil
			}
			var previous [1]byte
			n, err := r.file.ReadAt(previous[:], p.Offset-1)
			// EOF after a sealed partial record is a valid cursor generated by this
			// reader; a mid-record caller-supplied offset is not.
			if n != 1 || err != nil || previous[0] != '\n' && p.Offset != seg.size {
				return page, errors.New("log cursor is not at a record boundary")
			}
		}
	}
	responseBytes := 4096 // bounded cursor and outer response fields
	var sizes []int
	for {
		var selected *streamReader
		for _, r := range readers {
			if err := r.fill(ctx, q.Filter, &page); err != nil {
				if ctx.Err() != nil {
					return page, ctx.Err()
				}
				page.State = "unavailable"
				page.Reason = "A log segment could not be read."
				page.Cursor = encodeCursor(c)
				return page, nil
			}
			if r.head != nil && (selected == nil || r.head.Time.Before(selected.head.Time) || r.head.Time.Equal(selected.head.Time) && r.seg.id < selected.seg.id) {
				selected = r
			}
		}
		if selected == nil {
			break
		}
		encoded, _ := json.Marshal(selected.head)
		size := len(encoded) + 1
		if !q.Tail && (len(page.Records) >= q.Limit || responseBytes+size > records.MaxQueryBytes) {
			page.More = true
			break
		}
		page.Records = append(page.Records, *selected.head)
		sizes = append(sizes, size)
		responseBytes += size
		*selected.committed = selected.after
		selected.head = nil
		if q.Tail {
			for len(page.Records) > q.Limit || responseBytes > records.MaxQueryBytes {
				responseBytes -= sizes[0]
				sizes = sizes[1:]
				page.Records[0] = records.LocatedRecord{}
				page.Records = page.Records[1:]
			}
		}
	}
	if page.ScannedBytes >= records.MaxQueryScan {
		page.More = true
	}
	page.Cursor = encodeCursor(c)
	return page, nil
}
