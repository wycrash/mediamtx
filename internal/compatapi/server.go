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
	"github.com/bluenviron/mediamtx/internal/recorder"
)

type serverAuthManager interface {
	Authenticate(req *auth.Request) (string, *auth.Error)
}

type pathAPIGetter interface {
	APIPathsGet(string) (*defs.APIPath, error)
}

// Server is the compat API server.
type Server struct {
	Address             string
	Encryption          bool
	ServerKey           string
	ServerCert          string
	DumpPackets         bool
	AllowOrigins        []string
	TrustedProxies      conf.IPNetworks
	ReadTimeout         conf.Duration
	WriteTimeout        conf.Duration
	TimeOffsetMinutes   int
	IndexUpdateInterval conf.Duration
	PathConfs           map[string]*conf.Path
	PathManager         pathAPIGetter
	AuthManager         serverAuthManager
	HLSHandler          http.Handler
	DvrPlayer           fs.FS
	Parent              logger.Writer
	// MaintMu serializes index reconcile/rebuild with recordcleaner ticks.
	MaintMu *sync.Mutex

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
	reconcileStatusMu  sync.RWMutex
	reconcileState     defs.APICompatIndexStatusState
	reconcileStarted   time.Time
	debugStop          chan struct{}
	debugDone          chan struct{}
}

// SetPathManager attaches the path manager after it is created.
func (s *Server) SetPathManager(pm pathAPIGetter) {
	s.mutex.Lock()
	s.PathManager = pm
	s.mutex.Unlock()
}

// SetHLSHandler attaches the live HLS backend after it is created.
func (s *Server) SetHLSHandler(h http.Handler) {
	s.mutex.Lock()
	s.HLSHandler = h
	s.mutex.Unlock()
}

// Initialize starts the HTTP listener immediately. Index load and the first
// reconcile run in the background so recorders can register live segments.
func (s *Server) Initialize() error {
	s.Index = NewIndex()
	s.Index.Parent = s
	s.Index.EnablePersist(s.PathConfs)

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
	s.reconcileStop = make(chan struct{})
	s.reconcileDone = make(chan struct{})
	s.reconcileKick = make(chan struct{}, 1)
	s.reconcileState = defs.APICompatIndexStatusIdle
	s.startSessionCleanup()
	s.startDebugStats()
	go s.runIndexLifecycle()
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
func (s *Server) OnSegmentComplete(pathName, segmentPath string, duration time.Duration, parts []recorder.SegmentPart) {
	if s.Index == nil {
		return
	}
	s.Index.CompleteSegment(pathName, segmentPath, duration, parts)
}

// OnSegmentRemove is called when a recording segment file is deleted.
func (s *Server) OnSegmentRemove(segmentPath string) {
	if s.Index == nil {
		return
	}
	s.Index.Remove(segmentPath)
}

// OnPathDisabled is called after a path is stopped because enabled=false.
func (s *Server) OnPathDisabled(pathName string) {
	if s.Index == nil {
		return
	}
	s.Index.OnPathDisabled(pathName)
}

// OnPathEnabled is called after a path is started because enabled=true.
func (s *Server) OnPathEnabled(pathName string) {
	if s.Index == nil {
		return
	}
	s.Index.OnPathEnabled(pathName)
}

// Close closes Server.
func (s *Server) Close() {
	s.Log(logger.Info, "closing")
	s.stopDebugStats()
	s.stopSessionCleanup()
	s.stopIndexLifecycle()
	s.sessionsKickAll()
	if s.Index != nil {
		t0 := time.Now()
		st := s.Index.ClosePersist()
		s.Log(logger.Info, "dvr index flushed to disk (%d dirty / %d paths) in %s",
			st.Dirty, st.Paths, time.Since(t0))
	}
	s.httpServer.Close()
}

func (s *Server) runIndexLifecycle() {
	defer close(s.reconcileDone)
	if s.Index == nil {
		return
	}

	before := readProcMem()
	s.Log(logger.Info, "loading recording index (%s)", before.logLine())
	s.beginReconcile(defs.APICompatIndexStatusUpdate)
	t0 := time.Now()
	loadSt := s.Index.loadFromDisk(s.PathConfs, s.reconcileStop)
	if stopped(s.reconcileStop) {
		return
	}
	elapsed := time.Since(t0)
	after := readProcMem()
	st := s.Index.MemStats()
	s.Log(logger.Info, "recording index loaded (%d segments, %d paths, %d from disk) in %s",
		loadSt.Segments, loadSt.Paths, loadSt.DiskPaths, elapsed)
	s.logIndexMem(st, before, after)

	if loadSt.DiskPaths < loadSt.Paths || s.Index.HasPendingDayRepairs() {
		s.Log(logger.Info, "recording index needs repair (fromDisk=%d/%d pendingDays=%v)",
			loadSt.DiskPaths, loadSt.Paths, s.Index.HasPendingDayRepairs())
	}
	// Warm older days in the background so the first timeline seek is not a
	// cold journal read. Must not block HTTP or the first reconcile pass.
	go s.Index.PrefetchDays(s.reconcileStop)
	go s.Index.RunChunkFill(s.reconcileStop)
	// First pass always: full rebuild for incomplete paths, day-level repair
	// for damaged journals, edge check for healthy paths.
	s.runBackgroundReconcile(false)
	if stopped(s.reconcileStop) {
		return
	}

	updateEvery := time.Duration(s.IndexUpdateInterval)
	if updateEvery <= 0 {
		s.Log(logger.Info, "recording index periodic update disabled")
		for {
			select {
			case <-s.reconcileStop:
				return
			case <-s.reconcileKick:
				s.drainForcedRebuilds()
			}
		}
	}
	ticker := time.NewTicker(updateEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.reconcileStop:
			return
		case <-s.reconcileKick:
			s.drainForcedRebuilds()
		case <-ticker.C:
			s.runBackgroundReconcile(true)
		}
	}
}

// drainForcedRebuilds runs forced rebuilds until the incomplete queue stops shrinking.
func (s *Server) drainForcedRebuilds() {
	for {
		if stopped(s.reconcileStop) {
			return
		}
		before := s.Index.NeedsRebuildCount()
		if before == 0 {
			return
		}
		s.runBackgroundReconcile(false)
		after := s.Index.NeedsRebuildCount()
		if after == 0 || after >= before {
			return
		}
	}
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
	if s.MaintMu != nil {
		s.MaintMu.Lock()
		defer s.MaintMu.Unlock()
	}

	state := defs.APICompatIndexStatusUpdate
	if !slow && s.Index.NeedsRebuildCount() > 0 {
		state = defs.APICompatIndexStatusRebuild
	}
	s.beginReconcile(state)
	defer s.endReconcile()

	kind := "background update"
	if state == defs.APICompatIndexStatusRebuild {
		kind = "full rebuild"
	}
	s.Log(logger.Info, "recording index %s started", kind)
	t0 := time.Now()
	st := s.Index.ReconcileAll(s.reconcileStop, slow)
	dbg := s.Index.DebugSnapshot()
	s.Log(logger.Info, "recording index %s done (built=%d added=%d removed=%d inspected=%d pendingRepairs=%d) in %s; %s",
		kind, st.Built, st.Added, st.Removed, st.Inspected, s.Index.PendingDayRepairCount(), time.Since(t0),
		dbg.logLine())
}

func (s *Server) beginReconcile(state defs.APICompatIndexStatusState) {
	s.reconcileStatusMu.Lock()
	s.reconcileState = state
	s.reconcileStarted = time.Now()
	s.reconcileStatusMu.Unlock()
}

func (s *Server) endReconcile() {
	if s.Index != nil {
		s.Index.setProgressPath("")
	}
	s.reconcileStatusMu.Lock()
	s.reconcileState = defs.APICompatIndexStatusIdle
	s.reconcileStarted = time.Time{}
	s.reconcileStatusMu.Unlock()
}

func (s *Server) stopIndexLifecycle() {
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

const debugStatsInterval = 30 * time.Second

func (s *Server) startDebugStats() {
	if s.Index == nil {
		return
	}
	s.debugStop = make(chan struct{})
	s.debugDone = make(chan struct{})
	go func() {
		defer close(s.debugDone)
		ticker := time.NewTicker(debugStatsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.debugStop:
				return
			case <-ticker.C:
				s.logDebugStatsInterval()
			}
		}
	}()
}

func (s *Server) stopDebugStats() {
	if s.debugStop == nil {
		return
	}
	select {
	case <-s.debugStop:
	default:
		close(s.debugStop)
	}
	if s.debugDone != nil {
		<-s.debugDone
	}
}

func (s *Server) logDebugStatsInterval() {
	if s.Index == nil {
		return
	}
	snap := s.Index.DebugSnapshot()
	mem := readProcMem()
	s.reconcileStatusMu.RLock()
	state := s.reconcileState
	s.reconcileStatusMu.RUnlock()
	if state == "" {
		state = defs.APICompatIndexStatusIdle
	}
	// Always log when there was work, or when heap/goroutines look elevated.
	busy := snap.SegmentCreate+snap.SegmentComplete+snap.SegmentRemove+
		snap.WriteMeta+snap.ReconcilePath+snap.InspectFMP4+snap.SlowOps > 0
	if !busy && mem.goroutines < 200 && mem.heapAlloc < 300<<20 {
		return
	}
	s.Log(logger.Info, "index debug (%s) state=%s segs=%d pendingRepairs=%d %s",
		mem.logLine(),
		state,
		s.Index.SegmentCount(),
		s.Index.PendingDayRepairCount(),
		snap.logLine(),
	)
}

// ErrPathNotFound is returned when a rebuild target path does not exist.
var ErrPathNotFound = conf.ErrPathNotFound

// APIIndexRebuild implements defs.APICompatServer.
// pathName empty queues a rebuild of every recording path. Concurrent calls
// only mark paths incomplete and coalesce into the single reconcile worker.
func (s *Server) APIIndexRebuild(pathName string) (*defs.APICompatIndexRebuild, error) {
	if s.Index == nil {
		return nil, fmt.Errorf("recording index is not available")
	}

	out := &defs.APICompatIndexRebuild{
		Status: defs.APIOKStatusOK,
	}

	if pathName == "" {
		s.mutex.RLock()
		names := recordingPathNames(s.PathConfs)
		s.mutex.RUnlock()
		for _, name := range names {
			s.Index.MarkNeedsRebuild(name)
		}
		// Also mark any already-loaded paths that recordingPathNames missed
		// (e.g. removed from disk but still in memory).
		s.Index.MarkAllNeedsRebuild()
		out.All = true
		out.Queued = s.Index.NeedsRebuildCount()
		s.Log(logger.Info, "recording index rebuild queued (all, pending=%d)", out.Queued)
	} else {
		if err := conf.IsValidPathName(pathName); err != nil {
			return nil, err
		}
		s.mutex.RLock()
		pathConfs := s.PathConfs
		s.mutex.RUnlock()
		if _, _, err := conf.FindPathConf(pathConfs, pathName); err != nil {
			return nil, conf.PathNotFound(pathName)
		}
		s.Index.MarkNeedsRebuild(pathName)
		out.Path = pathName
		out.Queued = s.Index.NeedsRebuildCount()
		s.Log(logger.Info, "recording index rebuild queued (path=%s, pending=%d)", pathName, out.Queued)
	}

	s.kickBackgroundReconcile()
	return out, nil
}

// APIIndexStatus implements defs.APICompatServer.
func (s *Server) APIIndexStatus() (*defs.APICompatIndexStatus, error) {
	if s.Index == nil {
		return nil, fmt.Errorf("recording index is not available")
	}

	s.reconcileStatusMu.RLock()
	state := s.reconcileState
	started := s.reconcileStarted
	s.reconcileStatusMu.RUnlock()
	if state == "" {
		state = defs.APICompatIndexStatusIdle
	}

	current := s.Index.ProgressPath()
	pending := s.Index.NeedsRebuildPaths()
	queue := make([]string, 0, len(pending))
	for _, name := range pending {
		if name == current {
			continue
		}
		queue = append(queue, name)
	}

	queued := len(queue)
	if current != "" {
		for _, name := range pending {
			if name == current {
				queued++
				break
			}
		}
	}

	out := &defs.APICompatIndexStatus{
		State:             state,
		Current:           current,
		Queue:             queue,
		Queued:            queued,
		Segments:          s.Index.SegmentCount(),
		PendingDayRepairs: s.Index.PendingDayRepairCount(),
	}
	mem := readProcMem()
	out.HeapAlloc = mem.heapAlloc
	out.Goroutines = mem.goroutines
	dbg := s.Index.DebugSnapshot()
	out.Debug = &defs.APICompatIndexDebug{
		SegmentCreate:      dbg.SegmentCreate,
		SegmentComplete:    dbg.SegmentComplete,
		SegmentRemove:      dbg.SegmentRemove,
		WriteMeta:          dbg.WriteMeta,
		WriteMetaAvgMs:     dbg.WriteMetaAvgMs,
		PersistUpsert:      dbg.PersistUpsert,
		ReconcilePath:      dbg.ReconcilePath,
		ReconcilePathAvgMs: dbg.ReconcilePathAvgMs,
		InspectFMP4:        dbg.InspectFMP4,
		InspectFMP4AvgMs:   dbg.InspectFMP4AvgMs,
		SlowOps:            dbg.SlowOps,
		LastCompleteAgo:    formatAgo(dbg.LastComplete),
		LastWriteMetaAgo:   formatAgo(dbg.LastWriteMeta),
	}
	if !started.IsZero() && state != defs.APICompatIndexStatusIdle {
		t := started
		out.Started = &t
	}
	if out.Queue == nil {
		out.Queue = []string{}
	}
	return out, nil
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
	// Sticky session for players that do not store cookies (ExoPlayer).
	if sx := sessionFromGin(ctx); sx != nil {
		out.Set(sessionQueryParamName, sx.secret.String())
	} else if sid := q.Get(sessionQueryParamName); sid != "" {
		out.Set(sessionQueryParamName, sid)
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
