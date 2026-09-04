package api //nolint:revive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/test"
)

type testParent struct {
	log            func(_ logger.Level, _ string, _ ...any)
	onRestart      func()
	onUpgradeCheck func() (*defs.APIUpgrade, error)
	onUpgrade      func() (*defs.APIUpgrade, error)
}

func (p testParent) Log(l logger.Level, s string, a ...any) {
	if p.log != nil {
		p.log(l, s, a...)
	}
}

func (testParent) APIConfigSet(_ *conf.Conf) {}

func (p testParent) APIRestart() {
	if p.onRestart != nil {
		p.onRestart()
	}
}

func (p testParent) APIUpgradeCheck() (*defs.APIUpgrade, error) {
	if p.onUpgradeCheck != nil {
		return p.onUpgradeCheck()
	}
	return &defs.APIUpgrade{}, nil
}

func (p testParent) APIUpgrade() (*defs.APIUpgrade, error) {
	if p.onUpgrade != nil {
		return p.onUpgrade()
	}
	return &defs.APIUpgrade{}, nil
}

func tempConf(t *testing.T, cnt string) *conf.Conf {
	fi := test.CreateTempFile(t, []byte(cnt))

	cnf, _, err := conf.Load(fi, nil, nil)
	require.NoError(t, err)

	return cnf
}

func httpRequest(t *testing.T, hc *http.Client, method string, ur string, in any, out any) {
	buf := func() io.Reader {
		if in == nil {
			return nil
		}

		byts, err := json.Marshal(in)
		require.NoError(t, err)

		return bytes.NewBuffer(byts)
	}()

	req, err := http.NewRequest(method, ur, buf)
	require.NoError(t, err)

	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("bad status code: %d", res.StatusCode)
	}

	if out == nil {
		checkOK(t, res.Body)
		return
	}

	err = json.NewDecoder(res.Body).Decode(out)
	require.NoError(t, err)
}

func checkError(t *testing.T, body io.Reader, msg string) {
	var raw map[string]any
	err := json.NewDecoder(body).Decode(&raw)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"status": "error", "error": msg}, raw)
}

func checkOK(t *testing.T, body io.Reader) {
	var raw map[string]any
	err := json.NewDecoder(body).Decode(&raw)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"status": "ok"}, raw)
}

func TestPreflightRequest(t *testing.T) {
	api := API{
		Address:      "localhost:9997",
		AllowOrigins: []string{"*"},
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

	req, err := http.NewRequest(http.MethodOptions, "http://localhost:9997", nil)
	require.NoError(t, err)

	req.Header.Add("Access-Control-Request-Method", "GET")

	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusNoContent, res.StatusCode)

	byts, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "OPTIONS, GET, POST, PATCH, DELETE", res.Header.Get("Access-Control-Allow-Methods"))
	require.Equal(t, "Authorization, Content-Type", res.Header.Get("Access-Control-Allow-Headers"))
	require.Equal(t, byts, []byte{})
}

func TestInfo(t *testing.T) {
	cnf := tempConf(t, "api: yes\n")

	api := API{
		Version:      "v1.2.3",
		Started:      time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local),
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		Conf:         cnf,
		AuthManager:  test.NilAuthManager,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out map[string]any
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/info", nil, &out)
	require.Equal(t, map[string]any{
		"started": time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local).Format(time.RFC3339),
		"version": "v1.2.3",
	}, out)
}

type fakeSystemMetrics struct {
	snap defs.APISystemMetrics
}

func (f fakeSystemMetrics) Snapshot() defs.APISystemMetrics {
	return f.snap
}

func TestSystemMetrics(t *testing.T) {
	collected := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		SystemMetrics: fakeSystemMetrics{
			snap: defs.APISystemMetrics{
				CollectedAt: collected,
				CPU:         defs.APISystemMetricsCPU{Percent: 12.5, ProcessPercent: 4, Cores: 8},
				Memory: defs.APISystemMetricsMemory{
					TotalBytes:      1000,
					UsedBytes:       400,
					AvailableBytes:  600,
					UsedPercent:     40,
					ProcessRssBytes: 50,
				},
				Disks: []defs.APISystemMetricsDisk{{
					Path:             "/data",
					TotalBytes:       2000,
					UsedBytes:        500,
					FreeBytes:        1500,
					UsedPercent:      25,
					ReadBytesPerSec:  10,
					WriteBytesPerSec: 20,
				}},
				Network: []defs.APISystemMetricsNIC{{
					Name:            "eth0",
					RecvBytesPerSec: 100,
					SentBytesPerSec: 200,
				}},
				History: []defs.APISystemMetricsPoint{{
					CollectedAt:          collected,
					CPUPercent:           12.5,
					ProcessPercent:       4,
					MemUsedBytes:         400,
					MemUsedPercent:       40,
					ProcessRssBytes:      50,
					DiskUsedBytes:        500,
					DiskUsedPercent:      25,
					DiskReadBytesPerSec:  10,
					DiskWriteBytesPerSec: 20,
					NetRecvBytesPerSec:   100,
					NetSentBytesPerSec:   200,
				}},
			},
		},
		Parent: &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out map[string]any
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/metrics/system", nil, &out)
	require.Equal(t, map[string]any{
		"collectedAt": collected.Format(time.RFC3339),
		"cpu": map[string]any{
			"percent":        12.5,
			"processPercent": float64(4),
			"cores":          float64(8),
		},
		"memory": map[string]any{
			"totalBytes":      float64(1000),
			"usedBytes":       float64(400),
			"availableBytes":  float64(600),
			"usedPercent":     float64(40),
			"processRssBytes": float64(50),
		},
		"disks": []any{
			map[string]any{
				"path":             "/data",
				"totalBytes":       float64(2000),
				"usedBytes":        float64(500),
				"freeBytes":        float64(1500),
				"usedPercent":      float64(25),
				"readBytesPerSec":  float64(10),
				"writeBytesPerSec": float64(20),
			},
		},
		"network": []any{
			map[string]any{
				"name":            "eth0",
				"recvBytesPerSec": float64(100),
				"sentBytesPerSec": float64(200),
			},
		},
		"history": []any{
			map[string]any{
				"collectedAt":          collected.Format(time.RFC3339),
				"cpuPercent":           12.5,
				"processPercent":       float64(4),
				"memUsedBytes":         float64(400),
				"memUsedPercent":       float64(40),
				"processRssBytes":      float64(50),
				"diskUsedBytes":        float64(500),
				"diskUsedPercent":      float64(25),
				"diskReadBytesPerSec":  float64(10),
				"diskWriteBytesPerSec": float64(20),
				"netRecvBytesPerSec":   float64(100),
				"netSentBytesPerSec":   float64(200),
			},
		},
	}, out)
}

func TestAuthJWKSRefresh(t *testing.T) {
	ok := false

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager: &test.AuthManager{
			AuthenticateImpl: func(_ *auth.Request) (string, *auth.Error) {
				return "", nil
			},
			RefreshJWTJWKSImpl: func() {
				ok = true
			},
		},
		Parent: &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	u, err := url.Parse("http://localhost:9997/v3/auth/jwks/refresh")
	require.NoError(t, err)

	httpRequest(t, hc, http.MethodPost, u.String(), nil, nil)

	require.True(t, ok)
}

func TestAuthError(t *testing.T) {
	cnf := tempConf(t, "api: yes\n")

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		Conf:         cnf,
		AuthManager: &test.AuthManager{
			AuthenticateImpl: func(req *auth.Request) (string, *auth.Error) {
				if req.Credentials.User == "" {
					return "", &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}
				}
				return "", &auth.Error{Wrapped: fmt.Errorf("auth error")}
			},
		},
		Parent: &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	res, err := hc.Get("http://localhost:9997/v3/config/global/get")
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Equal(t, `Basic realm="mediamtx"`, res.Header.Get("WWW-Authenticate"))
	checkError(t, res.Body, "authentication error")

	res, err = hc.Get("http://myuser:mypass@localhost:9997/v3/config/global/get")
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Equal(t, ``, res.Header.Get("WWW-Authenticate"))
	checkError(t, res.Body, "authentication error")
}
