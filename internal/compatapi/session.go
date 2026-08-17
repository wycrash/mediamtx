package compatapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
)

const sessionGinKey = "compatSession"

// ErrSessionNotFound is returned when a session is not found.
var ErrSessionNotFound = errors.New("session not found")

type session struct {
	uuid       uuid.UUID
	created    time.Time
	remoteAddr string
	path       string
	query      string
	userAgent  string
	bytes      atomic.Uint64
	cancel     context.CancelFunc
	killed     atomic.Bool

	mutex sync.Mutex
	user  string
}

func (sx *session) setUser(user string) {
	sx.mutex.Lock()
	sx.user = user
	sx.mutex.Unlock()
}

func (sx *session) apiItem() *defs.APICompatSession {
	sx.mutex.Lock()
	user := sx.user
	sx.mutex.Unlock()

	return &defs.APICompatSession{
		ID:            sx.uuid,
		Created:       sx.created,
		RemoteAddr:    sx.remoteAddr,
		Path:          sx.path,
		Query:         sx.query,
		User:          user,
		UserAgent:     sx.userAgent,
		OutboundBytes: sx.bytes.Load(),
	}
}

type sessionWriter struct {
	gin.ResponseWriter
	sess *session
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	if w.sess.killed.Load() {
		return 0, net.ErrClosed
	}
	n, err := w.ResponseWriter.Write(p)
	w.sess.bytes.Add(uint64(n))
	return n, err
}

func (s *Server) middlewareSession(ctx *gin.Context) {
	if ctx.Request.Method != http.MethodGet || ctx.IsAborted() {
		return
	}

	reqCtx, cancel := context.WithCancel(ctx.Request.Context())
	ctx.Request = ctx.Request.WithContext(reqCtx)

	creds := httpp.Credentials(ctx.Request)
	sx := &session{
		uuid:       uuid.New(),
		created:    time.Now(),
		remoteAddr: httpp.RemoteAddr(ctx),
		path:       requestPathName(ctx.Request.URL.Path),
		query:      ctx.Request.URL.RawQuery,
		userAgent:  ctx.Request.UserAgent(),
		user:       creds.User,
		cancel:     cancel,
	}

	ctx.Writer = &sessionWriter{
		ResponseWriter: ctx.Writer,
		sess:           sx,
	}
	ctx.Set(sessionGinKey, sx)

	s.sessionsAdd(sx)
	defer s.sessionsRemove(sx.uuid)

	ctx.Next()
}

func (s *Server) sessionsAdd(sx *session) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.sessions == nil {
		sx.killed.Store(true)
		sx.cancel()
		return
	}
	s.sessions[sx.uuid] = sx
}

func (s *Server) sessionsRemove(id uuid.UUID) {
	s.sessionsMu.Lock()
	delete(s.sessions, id)
	s.sessionsMu.Unlock()
}

func (s *Server) sessionsKickAll() {
	s.sessionsMu.Lock()
	for _, sx := range s.sessions {
		sx.killed.Store(true)
		sx.cancel()
	}
	s.sessions = nil
	s.sessionsMu.Unlock()
}

// APISessionsList implements defs.APICompatServer.
func (s *Server) APISessionsList() (*defs.APICompatSessionList, error) {
	s.sessionsMu.RLock()
	items := make([]defs.APICompatSession, 0, len(s.sessions))
	for _, sx := range s.sessions {
		items = append(items, *sx.apiItem())
	}
	s.sessionsMu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		return items[i].Created.Before(items[j].Created)
	})

	return &defs.APICompatSessionList{Items: items}, nil
}

// APISessionsGet implements defs.APICompatServer.
func (s *Server) APISessionsGet(id uuid.UUID) (*defs.APICompatSession, error) {
	s.sessionsMu.RLock()
	sx, ok := s.sessions[id]
	s.sessionsMu.RUnlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	return sx.apiItem(), nil
}

// APISessionsKick implements defs.APICompatServer.
func (s *Server) APISessionsKick(id uuid.UUID) error {
	s.sessionsMu.Lock()
	sx, ok := s.sessions[id]
	if !ok {
		s.sessionsMu.Unlock()
		return ErrSessionNotFound
	}
	delete(s.sessions, id)
	s.sessionsMu.Unlock()

	sx.killed.Store(true)
	sx.cancel()
	return nil
}

func sessionFromGin(ctx *gin.Context) *session {
	v, ok := ctx.Get(sessionGinKey)
	if !ok {
		return nil
	}
	sx, _ := v.(*session)
	return sx
}
