package compatapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

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
	require.Equal(t, "cam1", requestPathName("/cam1/index-1000-60.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/archive-1000-60.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/archive-1000-60.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/timeshift_abs-1000.m3u8"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/timeshift_abs-1000.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/index-1786648330-89.fmp4.m3u8"))
	require.Equal(t, "cam1", requestPathName("/cam1/archive-1786643672-658.mp4"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/archive-1786643672-658.mp4"))
	require.Equal(t, "group/cam1", requestPathName("/group/cam1/index-1000-60.m3u8"))
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
		cancel:     cancel,
	}
	sx.bytes.Store(111)

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

	err = s.APISessionsKick(list.Items[0].ID)
	require.NoError(t, err)
	<-done
}
