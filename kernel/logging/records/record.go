// Package records owns the bounded unified log record and its stable text codec.
package records

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"the8020/kernel/identity"
)

const (
	MaxRecord       = 16 * 1024
	MaxFrame        = 32 * 1024
	BatchBytes      = 64 * 1024
	PendingBytes    = 1024 * 1024
	PriorityReserve = PendingBytes / 4
	FlushInterval   = 100 * time.Millisecond
	SyncInterval    = time.Second
	Omitted         = "…[middle omitted]…"
)

// Record is captured at the emitting boundary. A trusted connection supplies
// fixed producer fields; callers cannot relabel another node or sandbox.
type Record struct {
	Time            time.Time         `json:"time"`
	Level           string            `json:"level"`
	Source          string            `json:"source"`
	Component       string            `json:"component"`
	NodeID          string            `json:"node_id"`
	SandboxID       string            `json:"sandbox_id,omitempty"`
	WorkerID        string            `json:"worker_id,omitempty"`
	ContextID       string            `json:"context_id,omitempty"`
	ParentContextID string            `json:"parent_context_id,omitempty"`
	JobID           string            `json:"job_id,omitempty"`
	ServiceID       string            `json:"service_id,omitempty"`
	PersistentID    string            `json:"persistent_id,omitempty"`
	Object          string            `json:"object,omitempty"`
	Username        string            `json:"username,omitempty"`
	Stream          string            `json:"stream,omitempty"`
	Message         string            `json:"message"`
	Attributes      map[string]string `json:"attributes,omitempty"`
}

func Severity(level string) int {
	switch level {
	case "DEBUG":
		return 0
	case "INFO":
		return 1
	case "WARN":
		return 2
	case "ERROR":
		return 3
	default:
		return -1
	}
}

func ValidSource(source string) bool {
	return source == "kernel" || source == "deno" || source == "logd"
}

func validToken(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 || strings.ContainsRune("[]\\\"", c) {
			return false
		}
	}
	return true
}

func (r Record) validate() error {
	if r.Time.IsZero() || r.Time.Year() < 1 || r.Time.Year() > 9999 || Severity(r.Level) < 0 || !ValidSource(r.Source) || !validToken(r.Component, 64) {
		return errors.New("invalid log timestamp, severity, source, or component")
	}
	if !identity.Is(r.NodeID, "nod") {
		return errors.New("invalid log node identity")
	}
	for _, id := range []struct{ value, prefix string }{
		{r.SandboxID, "sbx"}, {r.WorkerID, "wrk"}, {r.ContextID, "ctx"},
		{r.ParentContextID, "ctx"}, {r.JobID, "job"}, {r.ServiceID, "srv"}, {r.PersistentID, "pex"},
	} {
		if id.value != "" && !identity.Is(id.value, id.prefix) {
			return fmt.Errorf("invalid log %s identity", id.prefix)
		}
	}
	if r.Stream != "" && r.Stream != "stdout" && r.Stream != "stderr" {
		return errors.New("invalid raw log stream")
	}
	// Execution owns principal validation. This codec requires a bounded token
	// that can be represented without changing the original username.
	if r.Username != "" && !validToken(r.Username, 32) {
		return errors.New("invalid log username encoding")
	}
	if r.Object != "" {
		kind, name, ok := strings.Cut(r.Object, ":")
		if !ok || (kind != "service" && kind != "job" && kind != "program") || !validToken(name, 512) {
			return errors.New("invalid declared log object")
		}
	}
	return nil
}

// escapedRune emits only JSON string escapes so Deno and standard tools can
// decode the same physical-line format. Invalid UTF-8 becomes a replacement rune.
func escapedRune(dst []byte, r rune) []byte {
	switch r {
	case '\\':
		return append(dst, '\\', '\\')
	case '"':
		return append(dst, '\\', '"')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	}
	if r < 32 || (r >= 127 && r <= 159) || r == 0x2028 || r == 0x2029 {
		const hex = "0123456789abcdef"
		return append(dst, '\\', 'u', hex[(r>>12)&15], hex[(r>>8)&15], hex[(r>>4)&15], hex[r&15])
	}
	return utf8.AppendRune(dst, r)
}

func prefix(s string, budget int) ([]byte, int) {
	out := make([]byte, 0, min(len(s), budget))
	consumed := 0
	for consumed < len(s) {
		r, n := utf8.DecodeRuneInString(s[consumed:])
		var buf [6]byte
		unit := escapedRune(buf[:0], r)
		if len(out)+len(unit) > budget {
			break
		}
		out = append(out, unit...)
		consumed += n
	}
	return out, consumed
}

// escapedSize examines at most budget bytes. A negative result means invalid
// UTF-8 or text that needs the bounded prefix/suffix formatter.
func escapedSize(s string, budget int) int {
	if len(s) > budget {
		return -1
	}
	size := len(s)
	for start := 0; start < len(s); {
		r, n := utf8.DecodeRuneInString(s[start:])
		if r == utf8.RuneError && n == 1 {
			return -1
		}
		switch r {
		case '\\', '"', '\n', '\r', '\t':
			size += 2 - n
		default:
			if r < 32 || (r >= 127 && r <= 159) || r == 0x2028 || r == 0x2029 {
				size += 6 - n
			}
		}
		if size > budget {
			return -1
		}
		start += n
	}
	return size
}

// Escape bounds work and allocations independently of the size of s. It keeps
// both ends while charging escapes, rather than truncating already-serialized data.
func Escape(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if escapedSize(s, budget) == len(s) {
		return s
	}
	out, n := prefix(s, budget)
	if n == len(s) {
		return string(out)
	}
	if budget < len(Omitted) {
		return strings.Repeat(".", budget)
	}
	headBudget := (budget - len(Omitted)) / 2
	head, _ := prefix(s, headBudget)
	tailBudget := budget - len(Omitted) - len(head)
	start, cost := len(s), 0
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		var buf [6]byte
		next := len(escapedRune(buf[:0], r))
		if cost+next > tailBudget {
			break
		}
		cost += next
		start -= size
	}
	tail, _ := prefix(s[start:], tailBudget)
	return string(head) + Omitted + string(tail)
}

func unescape(s string) (string, error) {
	var decoded string
	err := json.Unmarshal([]byte("\""+s+"\""), &decoded)
	return decoded, err
}

// Text bounds a string in escaped bytes, returning its readable form.
func Text(s string, budget int) string {
	if escapedSize(s, budget) >= 0 {
		return s
	}
	value, _ := unescape(Escape(s, budget))
	return value
}

func boundAttributes(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, min(16, len(attrs)))
	budget := 2048
	visited := 0
	for k, v := range attrs {
		if visited == 16 || budget < 64 {
			out["attributes_omitted"] = "true"
			break
		}
		visited++
		key := Text(k, 64)
		value := Text(v, min(512, budget-64))
		out[key] = value
		budget -= len(Escape(k, 64)) + len(Escape(v, min(512, budget-64))) + 6
	}
	return out
}

func header(r Record) []byte {
	out := []byte(r.Time.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	out = append(out, ' ')
	out = append(out, r.Level...)
	out = append(out, ' ')
	out = append(out, r.Source...)
	out = append(out, '/')
	out = append(out, r.Component...)
	out = append(out, " ["...)
	out = append(out, r.NodeID...)
	for _, id := range []string{r.SandboxID, r.WorkerID, r.ContextID, r.JobID, r.ServiceID, r.PersistentID} {
		if id != "" {
			out = append(out, ' ')
			out = append(out, id...)
		}
	}
	if r.ParentContextID != "" {
		out = append(out, " parent:"...)
		out = append(out, r.ParentContextID...)
	}
	if r.Object != "" {
		out = append(out, ' ')
		out = append(out, r.Object...)
	}
	if r.Username != "" {
		out = append(out, " user:"...)
		out = append(out, r.Username...)
	}
	if r.Stream != "" {
		out = append(out, ' ')
		out = append(out, r.Stream...)
	}
	return append(out, "] "...)
}

// Normalize bounds a record before JSON wire serialization. Encode applies the
// same bound after trusted metadata is stamped at the receiving boundary.
func Normalize(r Record) (Record, error) {
	if err := r.validate(); err != nil {
		return Record{}, err
	}
	r.Attributes = boundAttributes(r.Attributes)
	attrs, _ := json.Marshal(r.Attributes)
	r.Message = Text(r.Message, MaxRecord-len(header(r))-len(attrs)-2)
	return r, nil
}

// Marshal prepares a record for its length-prefixed wire frame. JSON HTML
// escaping is disabled, matching JSON.stringify and the ordinary text encoding.
func Marshal(r Record) ([]byte, error) {
	r, err := Normalize(r)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(ProducerPacket{Record: &r}); err != nil {
		return nil, err
	}
	if b.Len() > MaxFrame {
		return nil, errors.New("encoded log frame exceeds limit")
	}
	return b.Bytes(), nil
}

func Encode(r Record) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	r.Attributes = boundAttributes(r.Attributes)
	var attrs []byte
	if len(r.Attributes) > 0 {
		attrs, _ = json.Marshal(r.Attributes)
	}
	out := header(r)
	budget := MaxRecord - len(out) - len(attrs) - 2
	out = append(out, Escape(r.Message, budget)...)
	if len(attrs) > 0 {
		out = append(out, '\t')
		out = append(out, attrs...)
	}
	return append(out, '\n'), nil
}

func Decode(line []byte) (Record, error) {
	var r Record
	if len(line) > MaxRecord || len(line) == 0 || line[len(line)-1] != '\n' || bytes.Count(line, []byte{'\n'}) != 1 {
		return r, errors.New("invalid log line boundary")
	}
	fields := strings.SplitN(string(line[:len(line)-1]), " ", 4)
	if len(fields) != 4 {
		return r, errors.New("invalid log header")
	}
	var err error
	r.Time, err = time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return r, err
	}
	r.Level = fields[1]
	r.Source, r.Component, _ = strings.Cut(fields[2], "/")
	meta, msg, ok := strings.Cut(fields[3], "] ")
	if !ok || !strings.HasPrefix(meta, "[") {
		return r, errors.New("invalid log metadata")
	}
	seen := make(map[string]bool, 12)
	for _, token := range strings.Fields(meta[1:]) {
		var target *string
		key := token
		switch {
		case strings.HasPrefix(token, "user:"):
			key = "username"
			target = &r.Username
			token = strings.TrimPrefix(token, "user:")
		case strings.HasPrefix(token, "parent:"):
			key = "parent"
			target = &r.ParentContextID
			token = strings.TrimPrefix(token, "parent:")
		case strings.HasPrefix(token, "nod-"):
			key = "nod"
			target = &r.NodeID
		case strings.HasPrefix(token, "sbx-"):
			key = "sbx"
			target = &r.SandboxID
		case strings.HasPrefix(token, "wrk-"):
			key = "wrk"
			target = &r.WorkerID
		case strings.HasPrefix(token, "ctx-"):
			key = "ctx"
			target = &r.ContextID
		case strings.HasPrefix(token, "job-"):
			key = "job"
			target = &r.JobID
		case strings.HasPrefix(token, "srv-"):
			key = "srv"
			target = &r.ServiceID
		case strings.HasPrefix(token, "pex-"):
			key = "pex"
			target = &r.PersistentID
		case token == "stdout" || token == "stderr":
			key = "stream"
			target = &r.Stream
		case strings.Contains(token, ":"):
			key = "object"
			target = &r.Object
		default:
			return r, errors.New("unknown log metadata")
		}
		if token == "" {
			return r, errors.New("empty log metadata")
		}
		if seen[key] {
			return r, errors.New("duplicate log metadata")
		}
		seen[key] = true
		*target = token
	}
	message, attrs, hasAttrs := strings.Cut(msg, "\t")
	r.Message, err = unescape(message)
	if err != nil {
		return r, err
	}
	if hasAttrs {
		if err = json.Unmarshal([]byte(attrs), &r.Attributes); err != nil {
			return r, err
		}
	}
	return r, r.validate()
}
