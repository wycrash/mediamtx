package core

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/confpersist"
)

func TestConfPersistAPI(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "mediamtx.yml")
	seed := []byte("api: yes\n" +
		"apiAddress: 127.0.0.1:19997\n" +
		"rtsp: no\n" +
		"rtmp: no\n" +
		"hls: no\n" +
		"webrtc: no\n" +
		"srt: no\n" +
		"moq: no\n" +
		"paths:\n" +
		"  seedcam:\n" +
		"    source: rtsp://seed\n")
	err := os.WriteFile(confPath, seed, 0o644)
	require.NoError(t, err)

	p, ok := New([]string{confPath})
	require.True(t, ok)

	jp := confpersist.JSONPath(confPath)
	require.False(t, confpersist.Exists(jp))

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}
	apiBase := "http://127.0.0.1:19997"

	httpRequest(t, hc, http.MethodPost, apiBase+"/v3/config/paths/add/newcam",
		map[string]any{"source": "rtsp://new"}, nil)

	require.Eventually(t, func() bool {
		return confpersist.Exists(jp)
	}, 3*time.Second, 20*time.Millisecond)

	ymlAfter, err := os.ReadFile(confPath)
	require.NoError(t, err)
	require.Equal(t, seed, ymlAfter)

	p.Close()

	p, ok = New([]string{confPath})
	require.True(t, ok)

	var out map[string]any
	httpRequest(t, hc, http.MethodGet, apiBase+"/v3/config/paths/get/newcam", nil, &out)
	require.Equal(t, "rtsp://new", out["source"])

	httpRequest(t, hc, http.MethodDelete, apiBase+"/v3/config/paths/delete/newcam", nil, nil)
	require.Eventually(t, func() bool {
		cnf, _, loadErr := conf.Load(confPath, nil, nil)
		if loadErr != nil {
			return false
		}
		_, has := cnf.Paths["newcam"]
		return !has
	}, 3*time.Second, 20*time.Millisecond)

	p.Close()

	err = os.Remove(jp)
	require.NoError(t, err)

	p, ok = New([]string{confPath})
	require.True(t, ok)
	defer p.Close()

	req, err := http.NewRequest(http.MethodGet, apiBase+"/v3/config/paths/get/newcam", nil)
	require.NoError(t, err)
	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	httpRequest(t, hc, http.MethodGet, apiBase+"/v3/config/paths/get/seedcam", nil, &out)
	require.Equal(t, "rtsp://seed", out["source"])
}
