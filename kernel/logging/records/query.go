package records

import (
	"errors"
	"strings"
	"time"
)

const (
	MaxQueryRecords = 500
	MaxQueryBytes   = 256 * 1024
	MaxQueryScan    = 4 * 1024 * 1024
)

// Filter applies to the original capture timestamp and structured metadata.
// From is inclusive, Until exclusive. Empty fields impose no restriction.
type Filter struct {
	From            time.Time `json:"from,omitempty"`
	Until           time.Time `json:"until,omitempty"`
	Level           string    `json:"level,omitempty"`
	Source          string    `json:"source,omitempty"`
	NodeID          string    `json:"node_id,omitempty"`
	SandboxID       string    `json:"sandbox_id,omitempty"`
	WorkerID        string    `json:"worker_id,omitempty"`
	ContextID       string    `json:"context_id,omitempty"`
	ParentContextID string    `json:"parent_context_id,omitempty"`
	JobID           string    `json:"job_id,omitempty"`
	ServiceID       string    `json:"service_id,omitempty"`
	PersistentID    string    `json:"persistent_id,omitempty"`
	Object          string    `json:"object,omitempty"`
	Username        string    `json:"username,omitempty"`
}

type Query struct {
	Filter
	Position string `json:"position,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Tail     bool   `json:"tail,omitempty"`
}

func (q Query) Normalize() (Query, error) {
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > MaxQueryRecords || len(q.Cursor) > 4096 || len(q.Position) > 4096 {
		return q, errors.New("invalid log query limits")
	}
	if q.Tail && q.Cursor != "" {
		return q, errors.New("tail cannot be combined with a continuation cursor")
	}
	if q.Position != "" && (q.Tail || q.Cursor != "") {
		return q, errors.New("a starting log position cannot be combined with tail or a continuation cursor")
	}
	q.Level = strings.ToUpper(q.Level)
	q.From = q.From.UTC()
	q.Until = q.Until.UTC()
	if !q.From.IsZero() && !q.Until.IsZero() && !q.From.Before(q.Until) {
		return q, errors.New("log time range must increase")
	}
	r := Record{Time: time.Now().UTC(), Level: q.Level, Source: q.Source, Component: "query", NodeID: q.NodeID, SandboxID: q.SandboxID, WorkerID: q.WorkerID, ContextID: q.ContextID, ParentContextID: q.ParentContextID, JobID: q.JobID, ServiceID: q.ServiceID, PersistentID: q.PersistentID, Object: q.Object, Username: q.Username}
	if r.Level == "" {
		r.Level = "INFO"
	}
	if r.Source == "" {
		r.Source = "kernel"
	}
	if r.NodeID == "" {
		r.NodeID = "nod-0000000000"
	}
	return q, r.validate()
}

func (f Filter) Matches(r Record) bool {
	return (f.From.IsZero() || !r.Time.Before(f.From)) && (f.Until.IsZero() || r.Time.Before(f.Until)) &&
		(f.Level == "" || Severity(r.Level) >= Severity(f.Level)) && (f.Source == "" || r.Source == f.Source) &&
		(f.NodeID == "" || r.NodeID == f.NodeID) && (f.SandboxID == "" || r.SandboxID == f.SandboxID) &&
		(f.WorkerID == "" || r.WorkerID == f.WorkerID) && (f.ContextID == "" || r.ContextID == f.ContextID) &&
		(f.ParentContextID == "" || r.ParentContextID == f.ParentContextID) && (f.JobID == "" || r.JobID == f.JobID) &&
		(f.ServiceID == "" || r.ServiceID == f.ServiceID) && (f.PersistentID == "" || r.PersistentID == f.PersistentID) &&
		(f.Object == "" || r.Object == f.Object) && (f.Username == "" || r.Username == f.Username)
}

type LocatedRecord struct {
	Record
	Segment string `json:"segment"`
	Offset  int64  `json:"offset"`
}

type Page struct {
	State          string          `json:"state"` // ok, expired, unavailable
	Reason         string          `json:"reason,omitempty"`
	Records        []LocatedRecord `json:"records"`
	Cursor         string          `json:"cursor,omitempty"`
	More           bool            `json:"more"`
	ScannedBytes   int             `json:"scanned_bytes"`
	CorruptRecords int             `json:"corrupt_records,omitempty"`
	TailLimited    bool            `json:"tail_limited,omitempty"`
}
