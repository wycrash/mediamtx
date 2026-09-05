package api //nolint:revive

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/test"
)

func TestLogsList(t *testing.T) {
	ring := logger.NewRing(16)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	ring.Push(t0, logger.Debug, "[path cam1] debug")
	ring.Push(t0.Add(time.Second), logger.Info, "[path cam1] ready")
	ring.Push(t0.Add(2*time.Second), logger.Info, "[path cam10] other")
	ring.Push(t0.Add(3*time.Second), logger.Warn, "unrelated")
	ring.Push(t0.Add(4*time.Second), logger.Error, "[RTSP] is publishing to path 'cam1'")
	ring.Push(t0.Add(5*time.Second), logger.Warn, "[HLS] [muxer cam1] skipping track 2 (G711)")

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Logs:         ring,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	t.Run("newest first", func(t *testing.T) {
		var out defs.APILogList
		httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/logs/list", nil, &out)
		require.Equal(t, 6, out.ItemCount)
		require.Equal(t, 1, out.PageCount)
		require.Equal(t, []string{
			"[HLS] [muxer cam1] skipping track 2 (G711)",
			"[RTSP] is publishing to path 'cam1'",
			"unrelated",
			"[path cam10] other",
			"[path cam1] ready",
			"[path cam1] debug",
		}, logMessages(out.Items))
		require.Equal(t, conf.LogLevel(logger.Warn), out.Items[0].Level)
	})

	t.Run("paginate", func(t *testing.T) {
		var out defs.APILogList
		httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/logs/list?itemsPerPage=2&page=0", nil, &out)
		require.Equal(t, 6, out.ItemCount)
		require.Equal(t, 3, out.PageCount)
		require.Equal(t, []string{
			"[HLS] [muxer cam1] skipping track 2 (G711)",
			"[RTSP] is publishing to path 'cam1'",
		}, logMessages(out.Items))
	})

	t.Run("path", func(t *testing.T) {
		var out defs.APILogList
		httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/logs/list?path=cam1", nil, &out)
		require.Equal(t, []string{
			"[HLS] [muxer cam1] skipping track 2 (G711)",
			"[RTSP] is publishing to path 'cam1'",
			"[path cam1] ready",
			"[path cam1] debug",
		}, logMessages(out.Items))
	})
}

func TestLogsListEmpty(t *testing.T) {
	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out defs.APILogList
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/logs/list", nil, &out)
	require.Equal(t, 0, out.ItemCount)
	require.Equal(t, 0, out.PageCount)
	require.NotNil(t, out.Items)
	require.Empty(t, out.Items)
}

func logMessages(items []defs.APILogEntry) []string {
	out := make([]string, len(items))
	for i, e := range items {
		out[i] = e.Message
	}
	return out
}
