package compatapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
)

type testParent struct{}

func (testParent) Log(_ logger.Level, _ string, _ ...any) {}

func TestRequestPathName(t *testing.T) {
	require.Equal(t, "", requestPathName("/"))
	require.Equal(t, "cam1", requestPathName("/cam1/info.json"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/info.json"))
	require.Equal(t, "cam1", requestPathName("/cam1/recording_status.json"))
	require.Equal(t, "cam1", requestPathName("/cam1/ranges.json"))
	require.Equal(t, "cam1", requestPathName("/cam1/ranges.json/"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/ranges.json"))
	require.Equal(t, "cam1", requestPathName("/cam1/preview.mp4"))
	require.Equal(t, "cam1", requestPathName("/cam1/preview.jpeg"))
	require.Equal(t, "cam1", requestPathName("/cam1/preview.jpg"))
	require.Equal(t, "cam1", requestPathName("/cam1/embed.html"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/embed.html"))
	require.Equal(t, "cam1", requestPathName("/cam1/index-1000-60.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/archive-1000-60.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/mono-1000-60.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/archive-1000-60.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/mono-1000-60.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/timeshift_abs-1000.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/mono-timeshift_abs-1000.m3u8"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/timeshift_abs-1000.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/timeshift_rel-3582.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/mono-timeshift_rel-3582.m3u8"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/mono-timeshift_rel-3600.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/index-1786648330-89.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/archive-1786643672-658.mp4"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/archive-1786643672-658.mp4"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/index-1000-60.m3u8"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/mono-1000-60.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/2024/01/02/03/04/05.mp4"))
	require.Equal(t, "cam1", requestPathName("/cam1/2024/01/02/03/04/05-preview.mp4"))
	require.Equal(t, "cam1", requestPathName("/cam1/1786648428-preview.mp4"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/1786648428-preview.mp4"))
	require.Equal(t, "cam1", requestPathName("/cam1/index.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/video.m3u8"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/video.m3u8"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/seg.mp4"))
}

func TestArchivePlaylistRegexpFMP4Alias(t *testing.T) {
	m := archivePlaylistRegexp.FindStringSubmatch("cam1/index-1786648330-89.fmp4.m3u8")
	require.Equal(t, []string{
		"cam1/index-1786648330-89.fmp4.m3u8",
		"cam1",
		"1786648330",
		"89",
	}, m)

	m = archivePlaylistRegexp.FindStringSubmatch("cam1/archive-1786648330-89.fmp4.m3u8")
	require.Equal(t, "cam1", m[1])
	require.Equal(t, "1786648330", m[2])
	require.Equal(t, "89", m[3])

	m = archivePlaylistRegexp.FindStringSubmatch("cam1/mono-1788667800-7200.m3u8")
	require.Equal(t, []string{
		"cam1/mono-1788667800-7200.m3u8",
		"cam1",
		"1788667800",
		"7200",
	}, m)

	m = archivePlaylistRegexp.FindStringSubmatch("cam1/mono-1788667800-7200.fmp4.m3u8")
	require.Equal(t, []string{
		"cam1/mono-1788667800-7200.fmp4.m3u8",
		"cam1",
		"1788667800",
		"7200",
	}, m)
}

func TestAPISessionsListGetKick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	id := uuid.MustParse("18294761-f9d1-4ea9-9a35-fe265b62eb41")
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	sx := &session{
		uuid:       id,
		created:    created,
		remoteAddr: "192.168.1.1:5000",
		path:       "cam1",
		query:      "key=val",
		user:       "user1",
		userAgent:  "test-agent",
	}
	sx.addCancel(cancel)
	sx.bytes.Store(111)
	sx.lastReq.Store(created.UnixNano())

	s := &Server{
		sessions: map[uuid.UUID]*session{
			id: sx,
		},
	}

	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, id, list.Items[0].ID)
	require.Equal(t, "cam1", list.Items[0].Path)
	require.Equal(t, "user1", list.Items[0].User)
	require.Equal(t, uint64(111), list.Items[0].OutboundBytes)

	got, err := s.APISessionsGet(id)
	require.NoError(t, err)
	require.Equal(t, "192.168.1.1:5000", got.RemoteAddr)

	_, err = s.APISessionsGet(uuid.New())
	require.ErrorIs(t, err, ErrSessionNotFound)

	err = s.APISessionsKick(id)
	require.NoError(t, err)
	<-ctx.Done()
	require.True(t, sx.killed.Load())

	_, err = s.APISessionsGet(id)
	require.ErrorIs(t, err, ErrSessionNotFound)

	err = s.APISessionsKick(id)
	require.ErrorIs(t, err, ErrSessionNotFound)
}

func TestMiddlewareSessionKick(t *testing.T) {
	s := &Server{
		Parent:   testParent{},
		sessions: make(map[uuid.UUID]*session),
	}

	started := make(chan struct{})
	r := gin.New()
	r.Use(s.middlewareSession)
	r.GET("/cam1/index.m3u8", func(ctx *gin.Context) {
		close(started)
		<-ctx.Request.Context().Done()
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/cam1/index.m3u8?key=val", nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", "compat-test")
		req.SetBasicAuth("alice", "secret")
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
		}
	}()

	<-started

	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, "cam1", list.Items[0].Path)
	require.Equal(t, "key=val", list.Items[0].Query)
	require.Equal(t, "alice", list.Items[0].User)
	require.Equal(t, "compat-test", list.Items[0].UserAgent)
	require.False(t, list.Items[0].IsCDN)

	err = s.APISessionsKick(list.Items[0].ID)
	require.NoError(t, err)
	<-done
}

func TestMiddlewareSessionPersistsAfterRequest(t *testing.T) {
	s := &Server{
		Parent:   testParent{},
		sessions: make(map[uuid.UUID]*session),
	}

	r := gin.New()
	r.Use(s.middlewareSession)
	r.NoRoute(func(ctx *gin.Context) {
		ctx.String(http.StatusOK, "archive")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cam1/archive-1000-60.mp4?from=1", nil)
	req.Header.Set("User-Agent", "dvr-player")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, "cam1", list.Items[0].Path)
	require.Equal(t, "from=1", list.Items[0].Query)
	require.Equal(t, "dvr-player", list.Items[0].UserAgent)
	require.Equal(t, uint64(7), list.Items[0].OutboundBytes)
	id := list.Items[0].ID

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
			break
		}
	}
	require.NotNil(t, cookie)
	require.Equal(t, "/cam1", cookie.Path)

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/cam1/archive-1000-60.m3u8", nil)
	req2.AddCookie(cookie)
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	list, err = s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, id, list.Items[0].ID)
	require.Greater(t, list.Items[0].OutboundBytes, uint64(7))
}

func TestMiddlewareSessionPersistsViaQuery(t *testing.T) {
	s := &Server{
		Parent:   testParent{},
		sessions: make(map[uuid.UUID]*session),
	}

	r := gin.New()
	r.Use(s.middlewareSession)
	r.NoRoute(func(ctx *gin.Context) {
		ctx.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cam1/index-1000-60.m3u8?token=secret", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	id := list.Items[0].ID

	var secret string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			secret = c.Value
			break
		}
	}
	require.NotEmpty(t, secret)

	// ExoPlayer-style: no cookie, only ?session= from playlist URIs.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet,
		"/cam1/seg.ts?token=secret&session="+secret, nil)
	req2.RemoteAddr = "192.0.2.10:5678"
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	list, err = s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, id, list.Items[0].ID)

	// Different client IP must not reuse the query session.
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet,
		"/cam1/seg2.ts?session="+secret, nil)
	req3.RemoteAddr = "198.51.100.1:9"
	r.ServeHTTP(w3, req3)
	require.Equal(t, http.StatusOK, w3.Code)

	list, err = s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
}

func TestArchivePlaylistForwardsSessionToSegments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	segName := "2026-08-31-21-59-51-1788202791-801076.mp4"
	fpath := filepath.Join(dir, segName)
	writeArchiveTestSegment(t, fpath)

	start := time.Unix(1788202791, 0).UTC()
	idx := NewIndex()
	idx.Add("cam1", fpath, start)
	idx.SetFMP4Meta("cam1", fpath, fmp4SegMeta{
		Duration:  2 * time.Second,
		MoofCount: 1,
		Ready:     true,
	})

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:                  "cam1",
				RecordFormat:          conf.RecordFormatFMP4,
				RecordSegmentDuration: conf.Duration(10 * time.Second),
			},
		},
		AuthManager: tokenAuthManager(),
		Parent:      testParent{},
		Index:       idx,
		sessions:    make(map[uuid.UUID]*session),
	}
	r := gin.New()
	r.Use(s.middlewareSession)
	r.NoRoute(s.onRequest)

	req := httptest.NewRequest(http.MethodGet,
		"/cam1/index-1788202791-10.m3u8?token=secret", nil)
	req.RemoteAddr = "192.0.2.10:1"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var secret string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			secret = c.Value
			break
		}
	}
	require.NotEmpty(t, secret)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	got := string(body)
	require.Contains(t, got, "token=secret")
	require.Contains(t, got, "session="+secret)
	require.Contains(t, got, segName)

	// Segment fetch with session query only — same compat session.
	req2 := httptest.NewRequest(http.MethodGet,
		"/cam1/"+segName+"?hls=media&sn=0&td=0&token=secret&session="+secret, nil)
	req2.RemoteAddr = "192.0.2.10:2"
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
}

func TestExpireSessions(t *testing.T) {
	orig := sessionCloseAfter
	sessionCloseAfter = time.Millisecond
	t.Cleanup(func() { sessionCloseAfter = orig })

	id := uuid.New()
	secret := uuid.New()
	sx := &session{
		uuid:   id,
		secret: secret,
		path:   "cam1",
	}
	sx.lastReq.Store(time.Now().Add(-time.Second).UnixNano())

	s := &Server{
		sessions: map[uuid.UUID]*session{
			id: sx,
		},
		sessionsBySecret: map[uuid.UUID]*session{
			secret: sx,
		},
	}
	s.expireSessions()

	list, err := s.APISessionsList()
	require.NoError(t, err)
	require.Empty(t, list.Items)
}
