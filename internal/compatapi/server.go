// Package compatapi provides a DVR HTTP API on a single port.
package compatapi

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
)

type serverAuthManager interface {
	Authenticate(req *auth.Request) (string, *auth.Error)
}

type pathAPIGetter interface {
	APIPathsGet(string) (*defs.APIPath, error)
}

// Server is the compat API server.
type Server struct {
	Address           string
	Encryption        bool
	ServerKey         string
	ServerCert        string
	DumpPackets       bool
	AllowOrigins      []string
	TrustedProxies    conf.IPNetworks
	ReadTimeout       conf.Duration
	WriteTimeout      conf.Duration
	TimeOffsetMinutes int
	PathConfs         map[string]*conf.Path
	PathManager       pathAPIGetter
	AuthManager       serverAuthManager
	HLSHandler        http.Handler
	DvrPlayer         fs.FS
	Parent            logger.Writer

	Index              *Index
	httpServer         *httpp.Server
	mutex              sync.RWMutex
	sessionsMu         sync.RWMutex
	sessions           map[uuid.UUID]*session
	sessionsBySecret   map[uuid.UUID]*session
	sessionCleanupStop chan struct{}
	sessionCleanupDone chan struct{}
	reconcileStop      chan struct{}
	reconcileDone      chan struct{}
	reconcileKick      chan struct{}
}

// Initialize initializes Server.
func (s *Server) Initialize() error {
	s.Index = NewIndex()
	before := readProcMem()
	s.Log(logger.Info, "loading recording index (%s)", before.logLine())
	t0 := time.Now()
	loadSt := s.Index.LoadFromDisk(s.PathConfs)
	elapsed := time.Since(t0)
	after := readProcMem()
	st := s.Index.MemStats()
	s.Log(logger.Info, "recording index loaded (%d segments, %d paths, %d from disk) in %s",
		loadSt.Segments, loadSt.Paths, loadSt.DiskPaths, elapsed)
	s.logIndexMem(st, before, after)

	s.sessions = make(map[uuid.UUID]*session)
	s.sessionsBySecret = make(map[uuid.UUID]*session)

	router := gin.New()
	router.SetTrustedProxies(s.TrustedProxies.ToTrustedProxies()) //nolint:errcheck
	router.Use(s.middlewarePreflightRequests)
	router.Use(s.middlewareSession)
	router.NoRoute(s.onRequest)

	s.httpServer = &httpp.Server{
		Address:           s.Address,
		AllowOrigins:      s.AllowOrigins,
		DumpPackets:       s.DumpPackets,
		DumpPacketsPrefix: "compatapi_server_conn",
		ReadTimeout:       time.Duration(s.ReadTimeout),
		WriteTimeout:      time.Duration(s.WriteTimeout),
		Encryption:        s.Encryption,
		ServerKey:         s.ServerKey,
		ServerCert:        s.ServerCert,
		Handler:           router,
		Parent:            s,
	}
	err := s.httpServer.Initialize()
	if err != nil {
		return err
	}

	proto := "TCP/HTTP"
	if s.Encryption {
		proto = "TCP/HTTPS"
	}
	s.Log(logger.Info, "started with listener on %s (%s)", s.Address, proto)
	s.startSessionCleanup()
	s.startBackgroundReconcile(loadSt)
	return nil
}

// OnSegmentCreate is called when a recording segment file is created.
func (s *Server) OnSegmentCreate(pathName, segmentPath string) {
	if s.Index == nil {
		return
	}
	s.Index.AddFromPath(pathName, segmentPath)
}

// OnSegmentComplete is called when a recording segment file is closed.
func (s *Server) OnSegmentComplete(pathName, segmentPath string, duration time.Duration) {
	if s.Index == nil {
		return
	}
	s.Index.CompleteSegment(pathName, segmentPath, duration)
}

// OnSegmentRemove is called when a recording segment file is deleted.
func (s *Server) OnSegmentRemove(segmentPath string) {
	if s.Index == nil {
		return
	}
	s.Index.Remove(segmentPath)
}

// Close closes Server.
func (s *Server) Close() {
	s.Log(logger.Info, "closing")
	s.stopSessionCleanup()
	s.stopBackgroundReconcile()
	s.sessionsKickAll()
	if s.Index != nil {
		t0 := time.Now()
		n := s.Index.ClosePersist()
		s.Log(logger.Info, "dvr index flushed to disk (%d paths) in %s", n, time.Since(t0))
	}
	s.httpServer.Close()
}

func (s *Server) startBackgroundReconcile(loadSt IndexLoadStats) {
	if s.Index == nil {
		return
	}
	s.reconcileStop = make(chan struct{})
	s.reconcileDone = make(chan struct{})
	s.reconcileKick = make(chan struct{}, 1)
	// Deleted/missing snapshot: rebuild now, no throttle, no 5-minute wait.
	// A healthy index is only edge-checked on the scheduler.
	missing := loadSt.DiskPaths < loadSt.Paths
	go func() {
		defer close(s.reconcileDone)
		if missing {
			s.runBackgroundReconcile(false)
		}
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.reconcileStop:
				return
			case <-s.reconcileKick:
				s.runBackgroundReconcile(false)
			case <-ticker.C:
				s.runBackgroundReconcile(true)
			}
		}
	}()
}

func (s *Server) kickBackgroundReconcile() {
	if s.reconcileKick == nil {
		return
	}
	select {
	case s.reconcileKick <- struct{}{}:
	default:
	}
}

func (s *Server) runBackgroundReconcile(slow bool) {
	kind := "background update"
	if !slow {
		kind = "full rebuild"
	}
	s.Log(logger.Info, "recording index %s started", kind)
	t0 := time.Now()
	st := s.Index.ReconcileAll(s.reconcileStop, slow)
	s.Log(logger.Info, "recording index %s done (built=%d added=%d removed=%d inspected=%d) in %s",
		kind, st.Built, st.Added, st.Removed, st.Inspected, time.Since(t0))
}

func (s *Server) stopBackgroundReconcile() {
	if s.reconcileStop == nil {
		return
	}
	select {
	case <-s.reconcileStop:
	default:
		close(s.reconcileStop)
	}
	if s.reconcileDone != nil {
		<-s.reconcileDone
	}
}

// Log implements logger.Writer.
func (s *Server) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[compatapi] "+format, args...)
}

func (s *Server) logIndexMem(st IndexMemStats, before, after procMemStats) {
	s.Log(logger.Info,
		"index memory: fpath=%s name=%s codec_payload=%s tracks=%d unique_track_objs=%d interned_sets=%d fmp4_ready=%d init_cache=%d est_live=%s",
		formatBytes(uint64(st.FpathBytes)),
		formatBytes(uint64(st.NameBytes)),
		formatBytes(uint64(st.CodecPayloadBytes)),
		st.TrackPtrs,
		st.UniqueTrackPtrs,
		st.InternedSets,
		st.FMP4Ready,
		st.InitCacheEntries,
		formatBytes(uint64(st.EstLiveBytes)),
	)
	s.Log(logger.Info, "process memory after index load (%s)", after.logLine())
	runtime.GC()
	gc := readProcMem()
	s.Log(logger.Info, "process memory after GC (%s) heap_delta=%s",
		gc.logLine(),
		formatBytes(diffUint(gc.heapAlloc, before.heapAlloc)),
	)
	for _, p := range st.PathsDetail {
		s.Log(logger.Info, "index path %s: %d segments, %d with tracks, %d interned codec sets, paths=%s names=%s",
			p.Name, p.Segments, p.WithTracks, p.InternedSets,
			formatBytes(uint64(p.FpathBytes)),
			formatBytes(uint64(p.NameBytes)),
		)
	}
}

func diffUint(after, before uint64) uint64 {
	if after > before {
		return after - before
	}
	return 0
}

// ReloadPathConfs is called by core.Core.
func (s *Server) ReloadPathConfs(pathConfs map[string]*conf.Path) {
	s.mutex.Lock()
	s.PathConfs = pathConfs
	s.mutex.Unlock()
	if s.Index == nil {
		return
	}
	st := s.Index.ReloadPathConfs(pathConfs)
	if st.Paths == 0 {
		return
	}
	s.Log(logger.Info, "recording index loaded for new paths (%d segments, %d new paths, %d from disk)",
		st.Segments, st.Paths, st.DiskPaths)
	if st.DiskPaths < st.Paths {
		s.kickBackgroundReconcile()
	}
}

func (s *Server) writeError(ctx *gin.Context, status int, err error) {
	s.Log(logger.Error, err.Error())
	ctx.AbortWithStatusJSON(status, &defs.APIError{
		Status: defs.APIErrorStatusError,
		Error:  err.Error(),
	})
}

func (s *Server) writeErrorNoLog(ctx *gin.Context, status int, err error) {
	ctx.AbortWithStatusJSON(status, &defs.APIError{
		Status: defs.APIErrorStatusError,
		Error:  err.Error(),
	})
}

func (s *Server) safeFindPathConf(name string) (*conf.Path, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	pathConf, _, err := conf.FindPathConf(s.PathConfs, name)
	return pathConf, err
}

func (s *Server) middlewarePreflightRequests(ctx *gin.Context) {
	if ctx.Request.Method == http.MethodOptions &&
		ctx.Request.Header.Get("Access-Control-Request-Method") != "" {
		ctx.Header("Access-Control-Allow-Methods", "OPTIONS, GET, HEAD")
		ctx.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		ctx.AbortWithStatus(http.StatusNoContent)
		return
	}
}

func playlistAuthQuery(ctx *gin.Context) string {
	q := ctx.Request.URL.Query()
	out := url.Values{}
	if t := q.Get("token"); t != "" {
		out.Set("token", t)
	}
	if t := q.Get("jwt"); t != "" {
		out.Set("jwt", t)
	}
	if len(out) == 0 {
		creds := httpp.Credentials(ctx.Request)
		if creds != nil && creds.Token != "" {
			out.Set("token", creds.Token)
		}
	}
	return out.Encode()
}

func (s *Server) doAuth(ctx *gin.Context, pathName string) bool {
	req := &auth.Request{
		Action:               conf.AuthActionPlayback,
		Path:                 pathName,
		Query:                ctx.Request.URL.RawQuery,
		Protocol:             auth.ProtocolHLS,
		Credentials:          httpp.Credentials(ctx.Request),
		IP:                   net.ParseIP(ctx.ClientIP()),
		EnableAskCredentials: true,
	}

	user, err := s.AuthManager.Authenticate(req)
	if err != nil {
		if err.AskCredentials {
			ctx.Header("WWW-Authenticate", `Basic realm="mediamtx"`)
			s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
			return false
		}

		auth.LogAndDelayError(&logger.InlineWriter{
			Parent: s,
			Prefix: fmt.Sprintf("[conn %v]", httpp.RemoteAddr(ctx)),
		}, err)

		s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
		return false
	}

	if sx := sessionFromGin(ctx); sx != nil && user != "" {
		sx.setUser(user)
	}

	return true
}
