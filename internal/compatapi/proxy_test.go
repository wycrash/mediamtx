package compatapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocationToRelative(t *testing.T) {
	require.Equal(t,
		"index.m3u8?cookieCheck=1",
		locationToRelative("/cam1/index.m3u8", "/cam1/index.m3u8?cookieCheck=1"),
	)
	require.Equal(t,
		"index.m3u8?cookieCheck=1",
		locationToRelative("/cam1/index.m3u8", "http://127.0.0.1:8888/cam1/index.m3u8?cookieCheck=1"),
	)
	require.Equal(t,
		"index.m3u8?cookieCheck=1",
		locationToRelative("/cam1/index.m3u8", "index.m3u8?cookieCheck=1"),
	)
	require.Equal(t,
		"seg.mp4",
		locationToRelative("/cam1/index.m3u8", "/cam1/seg.mp4"),
	)
}

func TestRewriteHLSProxyResponse(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8877/cam1/index.m3u8", nil)
	require.NoError(t, err)

	resp := &http.Response{
		StatusCode: http.StatusFound,
		Header:     make(http.Header),
		Request:    req,
	}
	resp.Header.Set("Location", "/cam1/index.m3u8?cookieCheck=1")

	require.NoError(t, rewriteHLSProxyResponse(resp))
	require.Equal(t, "index.m3u8?cookieCheck=1", resp.Header.Get("Location"))
}

func TestIsHLSProxyClientGone(t *testing.T) {
	require.True(t, isHLSProxyClientGone(context.Canceled))
	require.True(t, isHLSProxyClientGone(fmt.Errorf("proxy: %w", context.Canceled)))
	require.True(t, isHLSProxyClientGone(http.ErrAbortHandler))
	require.False(t, isHLSProxyClientGone(fmt.Errorf("connection refused")))
}
