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
}

// APICompatSessionList is a list of Compat API sessions.
type APICompatSessionList struct {
	ItemCount int                `json:"itemCount"`
	PageCount int                `json:"pageCount"`
	Items     []APICompatSession `json:"items"`
}

// APICompatSession is an in-flight Compat API HTTP request.
type APICompatSession struct {
	ID            uuid.UUID `json:"id"`
	Created       time.Time `json:"created"`
	RemoteAddr    string    `json:"remoteAddr"`
	Path          string    `json:"path"`
	Query         string    `json:"query"`
	User          string    `json:"user"`
	UserAgent     string    `json:"userAgent"`
	OutboundBytes uint64    `json:"outboundBytes"`
}
