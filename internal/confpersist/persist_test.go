package confpersist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONPath(t *testing.T) {
	require.Equal(t, "", JSONPath(""))
	require.Equal(t, "mediamtx.json", JSONPath("mediamtx.yml"))
	require.Equal(t, "mediamtx.json", JSONPath("mediamtx.yaml"))
	require.Equal(t, "mediamtx.json", JSONPath("mediamtx.json"))
	require.Equal(t, "/etc/mediamtx/mediamtx.json", JSONPath("/etc/mediamtx/mediamtx.yml"))
	require.Equal(t, "rtsp-conf.json", JSONPath("rtsp-conf"))
}

func TestSaveLoadRoundTrip(t *testing.T) {
	orig := map[string]any{
		"paths": map[string]any{
			"cam1": map[string]any{
				"source": "rtsp://cam",
				"record": true,
			},
		},
	}

	jsonPath := filepath.Join(t.TempDir(), "mediamtx.json")
	err := Save(jsonPath, orig)
	require.NoError(t, err)
	require.True(t, Exists(jsonPath))

	entries, err := os.ReadDir(filepath.Dir(jsonPath))
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"))
	}

	loaded := map[string]any{}
	err = Load(jsonPath, &loaded)
	require.NoError(t, err)

	got, err := json.Marshal(loaded)
	require.NoError(t, err)
	want, err := json.Marshal(orig)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
}

func TestSaveEmptyPaths(t *testing.T) {
	jsonPath := filepath.Join(t.TempDir(), "mediamtx.json")
	err := Save(jsonPath, map[string]any{
		"paths": map[string]any{},
	})
	require.NoError(t, err)

	loaded := map[string]any{}
	err = Load(jsonPath, &loaded)
	require.NoError(t, err)
	require.Equal(t, map[string]any{}, loaded["paths"])

	if runtime.GOOS != "windows" {
		st, err := os.Stat(jsonPath)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
}

func TestSaveEmptyPathNoop(t *testing.T) {
	err := Save("", map[string]any{})
	require.NoError(t, err)
}
