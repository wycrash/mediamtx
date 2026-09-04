package api //nolint:revive

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/upgrade"
)

func TestSystemRestart(t *testing.T) {
	restarted := make(chan struct{})

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onRestart: func() { close(restarted) },
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	httpRequest(t, hc, http.MethodPost, "http://localhost:9997/v3/system/restart", nil, nil)

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart was not requested")
	}
}

func TestSystemUpgradeGet(t *testing.T) {
	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onUpgradeCheck: func() (*defs.APIUpgrade, error) {
				return &defs.APIUpgrade{
					Current:   "v1.8.0",
					Latest:    "v1.9.0",
					Available: true,
				}, nil
			},
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out defs.APIUpgrade
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/system/upgrade", nil, &out)
	require.Equal(t, defs.APIUpgrade{
		Current:   "v1.8.0",
		Latest:    "v1.9.0",
		Available: true,
	}, out)
}

func TestSystemUpgradeGetNoVersions(t *testing.T) {
	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onUpgradeCheck: func() (*defs.APIUpgrade, error) {
				return nil, upgrade.ErrNoVersions
			},
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	req, err := http.NewRequest(http.MethodGet, "http://localhost:9997/v3/system/upgrade", nil)
	require.NoError(t, err)
	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	checkError(t, res.Body, "no official releases found")
}

func TestSystemUpgradeGetUnofficial(t *testing.T) {
	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onUpgradeCheck: func() (*defs.APIUpgrade, error) {
				return nil, &upgrade.ErrUnofficial{Version: "v1.8.0-dirty"}
			},
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	req, err := http.NewRequest(http.MethodGet, "http://localhost:9997/v3/system/upgrade", nil)
	require.NoError(t, err)
	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	checkError(t, res.Body, "current version (v1.8.0-dirty) is not official and cannot be upgraded")
}

func TestSystemUpgradePostAppliesAndRestarts(t *testing.T) {
	restarted := make(chan struct{})
	var upgraded atomic.Bool

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onUpgrade: func() (*defs.APIUpgrade, error) {
				upgraded.Store(true)
				return &defs.APIUpgrade{
					Current:   "v1.8.0",
					Latest:    "v1.9.0",
					Available: true,
					Upgraded:  true,
				}, nil
			},
			onRestart: func() { close(restarted) },
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out defs.APIUpgrade
	httpRequest(t, hc, http.MethodPost, "http://localhost:9997/v3/system/upgrade", nil, &out)
	require.Equal(t, defs.APIUpgrade{
		Current:   "v1.8.0",
		Latest:    "v1.9.0",
		Available: true,
		Upgraded:  true,
	}, out)
	require.True(t, upgraded.Load())

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart was not requested after upgrade")
	}
}

func TestSystemUpgradePostUpToDateDoesNotRestart(t *testing.T) {
	restarted := make(chan struct{})

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onUpgrade: func() (*defs.APIUpgrade, error) {
				return &defs.APIUpgrade{
					Current:   "v1.9.0",
					Latest:    "v1.9.0",
					Available: false,
					Upgraded:  false,
				}, nil
			},
			onRestart: func() { close(restarted) },
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out defs.APIUpgrade
	httpRequest(t, hc, http.MethodPost, "http://localhost:9997/v3/system/upgrade", nil, &out)
	require.False(t, out.Upgraded)

	select {
	case <-restarted:
		t.Fatal("restart must not be requested when already up to date")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSystemUpgradePostError(t *testing.T) {
	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent: &testParent{
			onUpgrade: func() (*defs.APIUpgrade, error) {
				return nil, errors.New("download failed")
			},
		},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	req, err := http.NewRequest(http.MethodPost, "http://localhost:9997/v3/system/upgrade", nil)
	require.NoError(t, err)
	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusInternalServerError, res.StatusCode)
	checkError(t, res.Body, "download failed")
}
