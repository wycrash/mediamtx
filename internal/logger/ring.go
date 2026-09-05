package logger

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultRingSize is the number of entries kept in the in-memory log ring.
const DefaultRingSize = 2048

// Entry is a stored log line.
type Entry struct {
	Time    time.Time
	Level   Level
	Message string
}

// ListFilter selects entries returned by Ring.List.
type ListFilter struct {
	Path string
}

// Ring is an in-memory circular buffer of log entries.
type Ring struct {
	capacity int
	mutex    sync.Mutex
	entries  []Entry
	next     int
	size     int
}

// NewRing allocates a ring. A non-positive capacity uses DefaultRingSize.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultRingSize
	}

	return &Ring{
		capacity: capacity,
		entries:  make([]Entry, capacity),
	}
}

func (r *Ring) log(t time.Time, level Level, format string, args ...any) {
	r.Push(t, level, fmt.Sprintf(format, args...))
}

func (r *Ring) close() {
}

// Push stores an entry, overwriting the oldest one when the ring is full.
func (r *Ring) Push(t time.Time, level Level, message string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.entries[r.next] = Entry{
		Time:    t,
		Level:   level,
		Message: message,
	}
	r.next = (r.next + 1) % r.capacity
	if r.size < r.capacity {
		r.size++
	}
}

// List returns matching entries in chronological order (oldest first).
func (r *Ring) List(filter ListFilter) []Entry {
	r.mutex.Lock()
	out := r.snapshot()
	r.mutex.Unlock()

	if filter.Path == "" {
		return out
	}

	filtered := make([]Entry, 0, len(out))
	for _, e := range out {
		if entryMatchesPath(e.Message, filter.Path) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (r *Ring) snapshot() []Entry {
	out := make([]Entry, r.size)
	start := 0
	if r.size == r.capacity {
		start = r.next
	}
	for i := range r.size {
		out[i] = r.entries[(start+i)%r.capacity]
	}
	return out
}

func entryMatchesPath(msg, pathName string) bool {
	if strings.Contains(msg, "[path "+pathName+"]") {
		return true
	}
	if strings.Contains(msg, "path '"+pathName+"'") {
		return true
	}
	if strings.Contains(msg, "muxer '"+pathName+"'") {
		return true
	}

	needle := "path " + pathName
	from := 0
	for {
		i := strings.Index(msg[from:], needle)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(needle)
		if end == len(msg) || !isPathNameByte(msg[end]) {
			return true
		}
		from = i + 1
	}
}

func isPathNameByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '.' || b == '-' || b == '/'
}
