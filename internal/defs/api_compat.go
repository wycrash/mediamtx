package defs

import (
	"github.com/google/uuid"
)

// APICompatServer contains methods used by the API server.
type APICompatServer interface {
	APISessionsList() (*APICompatSessionList, error)
	APISessionsGet(uuid.UUID) (*APICompatSession, error)
	APISessionsKick(uuid.UUID) error
}

// APICompatSession is an in-flight Compat API HTTP request.
// JSON format is identical to APIHLSSession.
type APICompatSession = APIHLSSession

// APICompatSessionList is a list of Compat API sessions.
type APICompatSessionList = APIHLSSessionList
