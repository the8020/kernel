package records

import (
	"path/filepath"
	"time"
)

const ProtocolVersion = 1

// Transport admission includes the kernel plus the current maximum 100,000
// live sandboxes, including development. The kernel supplies its smaller
// configured live limit.
const MaxProducers = 100001

// Initialization is delivered only through the inherited private kernel control
// descriptor. Configuration and credentials never enter process arguments.
type Initialization struct {
	Version      int    `json:"version"`
	Directory    string `json:"directory"`
	Socket       string `json:"socket"`
	NodeID       string `json:"node_id"`
	Policy       Policy `json:"policy"`
	MaxProducers int    `json:"max_producers"`
}

// Binding uses the existing node ID for the kernel producer or sandbox ID for
// its supervisor. It introduces no additional producer-lifetime identity.
// An empty sandbox token is private-control-only raw FIFO recovery; socket
// authentication remains disabled until the original token is registered.
type Binding struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

func (b Binding) IngressDirectory(socket string) string {
	return filepath.Join(filepath.Dir(socket), "log-ingress")
}

func (b Binding) IngressPaths(socket string) RawPaths {
	dir := b.IngressDirectory(socket)
	return RawPaths{Stdout: filepath.Join(dir, b.ID+"-stdout"), Stderr: filepath.Join(dir, b.ID+"-stderr")}
}

type Hello struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Token   string `json:"token"`
}
type RawPaths struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type ProducerPacket struct {
	Record  *Record    `json:"record,omitempty"`
	Dropped *[4]uint64 `json:"dropped,omitempty"`
}

type ProducerPolicy struct {
	Revision uint64 `json:"revision"`
	Enabled  bool   `json:"enabled"`
	Level    string `json:"level"`
}

type Status struct {
	StartedAt       time.Time         `json:"started_at"`
	Policy          Policy            `json:"policy"`
	PolicyRevision  uint64            `json:"policy_revision"`
	Available       bool              `json:"available"`
	LastError       string            `json:"last_error,omitempty"`
	StorageErrors   uint64            `json:"storage_errors"`
	Accepted        uint64            `json:"accepted"`
	Written         uint64            `json:"written"`
	WrittenBytes    uint64            `json:"written_bytes"`
	Invalid         uint64            `json:"invalid"`
	UpstreamDropped [4]uint64         `json:"upstream_dropped"`
	Queue           QueueStats        `json:"queue"`
	Producers       int               `json:"producers"`
	Connected       int               `json:"connected"`
	TotalBytes      int64             `json:"total_bytes"`
	Segments        int               `json:"segments"`
	ActiveFiles     map[string]string `json:"active_files"`
	ReadPosition    string            `json:"read_position,omitempty"`
}

type ControlRequest struct {
	ID          string   `json:"id"`
	Operation   string   `json:"operation"`
	Binding     *Binding `json:"binding,omitempty"`
	Producer    string   `json:"producer,omitempty"`
	Policy      *Policy  `json:"policy,omitempty"`
	Preparation string   `json:"preparation,omitempty"`
	Query       *Query   `json:"query,omitempty"`
}

type ControlReply struct {
	ID          string    `json:"id,omitempty"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Status      *Status   `json:"status,omitempty"`
	Paths       *RawPaths `json:"paths,omitempty"`
	Preparation string    `json:"preparation,omitempty"`
	Page        *Page     `json:"page,omitempty"`
}
