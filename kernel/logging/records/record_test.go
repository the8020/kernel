package records

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func fixture(message string) Record {
	return Record{Time: time.Date(2026, 9, 5, 12, 34, 56, 790000000, time.UTC), Level: "ERROR", Source: "deno", Component: "worker", NodeID: "nod-0123456789", SandboxID: "sbx-0123456789", WorkerID: "wrk-0123456789", ContextID: "ctx-0123456789", ParentContextID: "ctx-abcdefghij", JobID: "job-0123456789", Object: "program:acme/billing/invoices", Message: message}
}

func TestRecordRoundTripAndSinglePhysicalBoundary(t *testing.T) {
	want := fixture("TypeError: broken\n  at line:10\r\ncaused by: \"日本語\"\\\t\x00\x1b\u2028")
	want.Attributes = map[string]string{"reason": "a\nb", "exit": "2"}
	want.Username = "alice"
	line, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(line, []byte{'\n'}) != 1 || !bytes.Contains(line, []byte("parent:ctx-abcdefghij")) || !bytes.Contains(line, []byte("user:alice")) {
		t.Fatalf("invalid boundary/parent: %q", line)
	}
	got, err := Decode(line)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip: %#v", got)
	}
}

func TestOversizedTextPreservesBothEndsAndMetadata(t *testing.T) {
	for _, filler := range []string{"a", "\n", "💡", "\xff", "<"} {
		r := fixture("BEGIN STACK " + strings.Repeat(filler, 1024*1024) + " END CAUSE")
		r.Attributes = map[string]string{"data": strings.Repeat(filler, 1024*1024)}
		line, err := Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(line) > MaxRecord || !utf8.Valid(line) {
			t.Fatalf("unbounded or invalid record: %d", len(line))
		}
		got, err := Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got.Message, "BEGIN STACK ") || !strings.HasSuffix(got.Message, " END CAUSE") || !strings.Contains(got.Message, Omitted) || got.ContextID != r.ContextID {
			t.Fatal("lost text ends or identity")
		}
		wire, err := Marshal(r)
		if err != nil || len(wire) > MaxFrame {
			t.Fatalf("wire bound: %d %v", len(wire), err)
		}
	}
}

func TestRecordRejectsAttributionAndBoundaryInjection(t *testing.T) {
	for _, mutate := range []func(*Record){
		func(r *Record) { r.WorkerID = "wrk-spoof]\n" },
		func(r *Record) { r.Component = "worker\n" },
		func(r *Record) { r.Object = "service:a b" },
		func(r *Record) { r.Stream = "other" },
		func(r *Record) { r.Username = "alice] user:bob" },
	} {
		r := fixture("text")
		mutate(&r)
		if _, err := Encode(r); err == nil {
			t.Fatal("accepted forged metadata")
		}
	}
	line, _ := Encode(fixture("text"))
	if _, err := Decode(append(line, []byte("injected\n")...)); err == nil {
		t.Fatal("accepted two physical records")
	}
}

func TestEscapeEverySmallBudget(t *testing.T) {
	for n := 0; n < 160; n++ {
		s := Escape(strings.Repeat("a\n💡日本語\xff", 100), n)
		if len(s) > n || !utf8.ValidString(s) {
			t.Fatalf("budget %d: %q", n, s)
		}
		if _, err := unescape(s); err != nil {
			t.Fatalf("invalid escape %d: %v", n, err)
		}
	}
}

func TestTextAndEscapeChargeUTF8AndControlBytes(t *testing.T) {
	for _, tc := range []struct {
		input, escaped, readable string
		budget                   int
	}{
		{"plain", "plain", "plain", 5},
		{"日本語💡", "日本語💡", "日本語💡", 13},
		{"\n\"\\", `\n\"\\`, "\n\"\\", 6},
		{"\x00\u0080\u2028", `\u0000\u0080\u2028`, "\x00\u0080\u2028", 18},
		{"\xff", "�", "�", 3},
		{"\n", ".", ".", 1},
		{"💡", "...", "...", 3},
		{"\xff", ".", ".", 1},
		{"text", "", "", 0},
		{"text", "", "", -1},
	} {
		if got := Escape(tc.input, tc.budget); got != tc.escaped {
			t.Errorf("Escape(%q, %d) = %q, want %q", tc.input, tc.budget, got, tc.escaped)
		}
		if got := Text(tc.input, tc.budget); got != tc.readable {
			t.Errorf("Text(%q, %d) = %q, want %q", tc.input, tc.budget, got, tc.readable)
		}
	}
	for _, input := range []string{"ordinary text", "日本語💡", "stack\n\tat call\u2028"} {
		if allocations := testing.AllocsPerRun(100, func() {
			if Text(input, 512) != input {
				t.Fatal("bounded readable text changed")
			}
		}); allocations != 0 {
			t.Fatalf("already bounded text allocated: %g", allocations)
		}
	}
}

type partialWriter struct{ bytes.Buffer }

func (w *partialWriter) Write(p []byte) (int, error) { return w.Buffer.Write(p[:min(3, len(p))]) }

func TestFramingBoundsBeforeAllocationAndHandlesPartialIO(t *testing.T) {
	var w partialWriter
	if err := WriteFrame(&w, []byte(`{"message":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&w)
	if err != nil || string(got) != `{"message":"hello"}` {
		t.Fatalf("partial roundtrip: %s %v", got, err)
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], MaxFrame+1)
	if _, err := ReadFrame(bytes.NewReader(size[:])); err == nil {
		t.Fatal("accepted oversized frame")
	}
	binary.BigEndian.PutUint32(size[:], 12)
	if _, err := ReadFrame(bytes.NewReader(size[:])); err != io.EOF {
		t.Fatalf("missing payload: %v", err)
	}
}

func TestRawFragmentsOversizeAndEOF(t *testing.T) {
	var messages []string
	var partials []bool
	f := NewLines(func(message string, partial bool) {
		messages = append(messages, message)
		partials = append(partials, partial)
	})
	input := []byte("α💡日本語\n\nBEGIN" + strings.Repeat("世", 100000) + "END\nunterminated💡")
	for len(input) > 0 {
		n := min(7, len(input))
		f.Feed(input[:n])
		input = input[n:]
	}
	f.EOF()
	f.EOF()
	if len(messages) != 4 || messages[0] != "α💡日本語" || messages[1] != "" || messages[3] != "unterminated💡" || !reflect.DeepEqual(partials, []bool{false, false, false, true}) {
		t.Fatalf("raw frames: %#v %v", messages, partials)
	}
	if !strings.HasPrefix(messages[2], "BEGIN") || !strings.HasSuffix(messages[2], "END") || !strings.Contains(messages[2], Omitted) || len(messages[2]) > 2*rawHalf+len(Omitted) || !utf8.ValidString(messages[2]) {
		t.Fatal("invalid bounded raw frame")
	}
}
