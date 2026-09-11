package defs

import (
	"time"

	"github.com/google/uuid"
)

// APICompatServer contains methods used by the API server.
type APICompatServer interface {
	APISessionsList() (*APICompatSessionList, error)
	APISessionsGet(uuid.UUID) (*APICompatSession, error)
	APISessionsKick(uuid.UUID) error
	// APIIndexRebuild queues a forced DVR index rebuild.
	// Empty pathName rebuilds all recording paths.
	APIIndexRebuild(pathName string) (*APICompatIndexRebuild, error)
	APIIndexStatus() (*APICompatIndexStatus, error)
}

// APICompatSession is an in-flight Compat API HTTP request.
// JSON format is identical to APIHLSSession.
type APICompatSession = APIHLSSession

// APICompatSessionList is a list of Compat API sessions.
type APICompatSessionList = APIHLSSessionList

// APICompatIndexRebuild is returned when a DVR index rebuild is queued.
type APICompatIndexRebuild struct {
	// Status is always "ok" when the request was accepted.
	Status APIOKStatus `json:"status"`

	// All is true when every recording path was queued.
	All bool `json:"all"`

	// Path is set when a single path was queued.
	Path string `json:"path,omitempty"`

	// Queued is how many paths are marked for rebuild after this request
	// (deduplicated; concurrent requests coalesce into one worker).
	Queued int `json:"queued"`
}

// APICompatIndexStatusState is the worker state for the Compat DVR index.
type APICompatIndexStatusState string

const (
	APICompatIndexStatusIdle     APICompatIndexStatusState = "idle"
	APICompatIndexStatusRebuild  APICompatIndexStatusState = "rebuild"
	APICompatIndexStatusUpdate   APICompatIndexStatusState = "update"
)

// APICompatIndexStatus is the current DVR index rebuild/update worker status.
type APICompatIndexStatus struct {
	// State is idle, rebuild (forced disk rebuild), or update (periodic edge scan).
	State APICompatIndexStatusState `json:"state"`

	// Current is the path currently being processed. Empty when idle.
	Current string `json:"current"`

	// Queue is paths waiting for a forced rebuild (incomplete), excluding Current.
	Queue []string `json:"queue"`

	// Queued is len(Queue) plus 1 when Current is a forced rebuild target.
	Queued int `json:"queued"`

	// Started is when the current worker pass began. Null when idle.
	Started *time.Time `json:"started"`

	// Segments is the number of in-memory index segments.
	Segments int `json:"segments"`

	// PendingDayRepairs is how many damaged day journals still need rebuild.
	PendingDayRepairs int `json:"pendingDayRepairs"`

	// HeapAlloc is Go heap allocated bytes (runtime.MemStats.HeapAlloc).
	HeapAlloc uint64 `json:"heapAlloc"`

	// Goroutines is runtime.NumGoroutine().
	Goroutines int `json:"goroutines"`

	// Debug holds hot-path counters since process start (or last reset interval).
	Debug *APICompatIndexDebug `json:"debug,omitempty"`
}

// APICompatIndexDebug exposes compatapi index hot-path counters for load diagnosis.
type APICompatIndexDebug struct {
	SegmentCreate      uint64  `json:"segmentCreate"`
	SegmentComplete    uint64  `json:"segmentComplete"`
	SegmentRemove      uint64  `json:"segmentRemove"`
	WriteMeta          uint64  `json:"writeMeta"`
	WriteMetaAvgMs     float64 `json:"writeMetaAvgMs"`
	PersistUpsert      uint64  `json:"persistUpsert"`
	ReconcilePath      uint64  `json:"reconcilePath"`
	ReconcilePathAvgMs float64 `json:"reconcilePathAvgMs"`
	InspectFMP4        uint64  `json:"inspectFmp4"`
	InspectFMP4AvgMs   float64 `json:"inspectFmp4AvgMs"`
	SlowOps            uint64  `json:"slowOps"`
	LastCompleteAgo    string  `json:"lastCompleteAgo,omitempty"`
	LastWriteMetaAgo   string  `json:"lastWriteMetaAgo,omitempty"`
}
