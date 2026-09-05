package compatapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
)

const (
	sessionCookieName = "compatSession"
	sessionGinKey     = "compatSession"
)

var (
	sessionCloseAfter    = 30 * time.Second
	sessionCleanupPeriod = 10 * time.Second
)

// ErrSessionNotFound is returned when a session is not found.
var ErrSessionNotFound = errors.New("session not found")

type session struct {
	uuid       uuid.UUID
	secret     uuid.UUID
	created    time.Time
	remoteAddr string
	path       string
	query      string
	userAgent  string
	bytes      atomic.Uint64
	lastReq    atomic.Int64
	killed     atomic.Bool

	mutex   sync.Mutex
	user    string
	cancels map[uint64]context.CancelFunc
	nextID  uint64
}

func (sx *session) setUser(user string) {
	sx.mutex.Lock()
	sx.user = user
	sx.mutex.Unlock()
}

func (sx *session) addCancel(cancel context.CancelFunc) uint64 {
	sx.mutex.Lock()
	defer sx.mutex.Unlock()
	if sx.cancels == nil {
		sx.cancels = make(map[uint64]context.CancelFunc)
	}
	sx.nextID++
	id := sx.nextID
	sx.cancels[id] = cancel
	return id
}

func (sx *session) removeCancel(id uint64) {
	sx.mutex.Lock()
	delete(sx.cancels, id)
	sx.mutex.Unlock()
}

func (sx *session) kick() {
	sx.killed.Store(true)
	sx.mutex.Lock()
	for _, cancel := range sx.cancels {
		cancel()
	}
	sx.cancels = nil
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
		IsCDN:         false,
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

func skipCompatSession(rawPath string) bool {
	pa := strings.TrimPrefix(rawPath, "/")
	if pa == "" {
		return true
	}
	if pa == "lib/dvrplayer" || strings.HasPrefix(pa, "lib/dvrplayer/") {
		return true
	}
	return strings.HasSuffix(pa, "/embed.html")
}

func (s *Server) middlewareSession(ctx *gin.Context) {
	if ctx.Request.Method != http.MethodGet || ctx.IsAborted() || skipCompatSession(ctx.Request.URL.Path) {
		return
	}

	sx, created := s.sessionForRequest(ctx)
	if sx == nil {
		return
	}

	reqCtx, cancel := context.WithCancel(ctx.Request.Context())
	ctx.Request = ctx.Request.WithContext(reqCtx)
	cancelID := sx.addCancel(cancel)

	ctx.Writer = &sessionWriter{
		ResponseWriter: ctx.Writer,
		sess:           sx,
	}
	ctx.Set(sessionGinKey, sx)

	defer sx.removeCancel(cancelID)
	ctx.Next()

	if created && s.Parent != nil {
		s.Log(logger.Info, "[session %s] created by %s path=%s", sx.uuid, sx.remoteAddr, sx.path)
	}
}

func (s *Server) sessionForRequest(ctx *gin.Context) (*session, bool) {
	if sx := s.sessionFromCookie(ctx); sx != nil {
		sx.lastReq.Store(time.Now().UnixNano())
		return sx, false
	}

	creds := httpp.Credentials(ctx.Request)
	pathName := requestPathName(ctx.Request.URL.Path)
	sx := &session{
		uuid:       uuid.New(),
		secret:     uuid.New(),
		created:    time.Now(),
		remoteAddr: httpp.RemoteAddr(ctx),
		path:       pathName,
		query:      ctx.Request.URL.RawQuery,
		userAgent:  ctx.Request.UserAgent(),
		user:       creds.User,
	}
	sx.lastReq.Store(sx.created.UnixNano())

	if !s.sessionsAdd(sx) {
		sx.kick()
		return nil, false
	}

	http.SetCookie(ctx.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sx.secret.String(),
		Path:     cookiePath(pathName),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return sx, true
}

func cookiePath(pathName string) string {
	if pathName == "" {
		return "/"
	}
	return "/" + pathName
}

func (s *Server) sessionFromCookie(ctx *gin.Context) *session {
	c, err := ctx.Request.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	secret, err := uuid.Parse(c.Value)
	if err != nil {
		return nil
	}

	s.sessionsMu.RLock()
	sx := s.sessionsBySecret[secret]
	s.sessionsMu.RUnlock()
	if sx == nil || sx.killed.Load() {
		return nil
	}
	return sx
}

func (s *Server) sessionsAdd(sx *session) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.sessions == nil {
		return false
	}
	if s.sessionsBySecret == nil {
		s.sessionsBySecret = make(map[uuid.UUID]*session)
	}
	s.sessions[sx.uuid] = sx
	s.sessionsBySecret[sx.secret] = sx
	return true
}

func (s *Server) sessionsKickAll() {
	s.sessionsMu.Lock()
	for _, sx := range s.sessions {
		sx.kick()
	}
	s.sessions = nil
	s.sessionsBySecret = nil
	s.sessionsMu.Unlock()
}

func (s *Server) startSessionCleanup() {
	s.sessionCleanupStop = make(chan struct{})
	s.sessionCleanupDone = make(chan struct{})
	go func() {
		defer close(s.sessionCleanupDone)
		ticker := time.NewTicker(sessionCleanupPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-s.sessionCleanupStop:
				return
			case <-ticker.C:
				s.expireSessions()
			}
		}
	}()
}

func (s *Server) stopSessionCleanup() {
	if s.sessionCleanupStop == nil {
		return
	}
	close(s.sessionCleanupStop)
	<-s.sessionCleanupDone
	s.sessionCleanupStop = nil
}

func (s *Server) expireSessions() {
	now := time.Now()
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	for id, sx := range s.sessions {
		last := time.Unix(0, sx.lastReq.Load())
		if now.Sub(last) < sessionCloseAfter {
			continue
		}
		delete(s.sessions, id)
		delete(s.sessionsBySecret, sx.secret)
		sx.kick()
	}
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
	delete(s.sessionsBySecret, sx.secret)
	s.sessionsMu.Unlock()

	sx.kick()
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
