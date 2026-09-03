package api //nolint:revive

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func TestAdminUI(t *testing.T) {
	api := API{
		Address:      "127.0.0.1:39997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager: &test.AuthManager{
			AuthenticateImpl: func(_ *auth.Request) (string, *auth.Error) {
				return "", &auth.Error{AskCredentials: true, Wrapped: fmt.Errorf("auth error")}
			},
		},
		Admin: fstest.MapFS{
			"index.html":    &fstest.MapFile{Data: []byte("<html>admin</html>")},
			"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
			"settings.json": &fstest.MapFile{Data: []byte(`{"http":""}`)},
		},
		Parent: &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{
		Transport: tr,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	res, err := hc.Get("http://127.0.0.1:39997/")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusFound, res.StatusCode)
	require.Equal(t, "/admin/", res.Header.Get("Location"))

	res, err = hc.Get("http://127.0.0.1:39997/admin")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusFound, res.StatusCode)
	require.Equal(t, "/admin/", res.Header.Get("Location"))

	res, err = hc.Get("http://127.0.0.1:39997/admin/")
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(body), "admin")
	require.Empty(t, res.Header.Get("WWW-Authenticate"))

	res, err = hc.Get("http://127.0.0.1:39997/admin/assets/app.js")
	require.NoError(t, err)
	body, err = io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(body), "console.log")

	res, err = hc.Get("http://127.0.0.1:39997/admin/settings/api")
	require.NoError(t, err)
	body, err = io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(body), "admin")

	res, err = hc.Get("http://127.0.0.1:39997/admin/missing.js")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	res, err = hc.Get("http://127.0.0.1:39997/v3/info")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
}
