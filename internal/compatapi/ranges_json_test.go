package compatapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func TestRangesJSONHTTP(t *testing.T) {
	idx := NewIndex()
	idx.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:                  "cam1",
			RecordSegmentDuration: conf.Duration(10 * time.Second),
		},
	})
	base := time.Unix(1000, 0).UTC()
	idx.Add("cam1", "/rec/cam1/a.ts", base)
	idx.Add("cam1", "/rec/cam1/b.ts", base.Add(10*time.Second))
	idx.Add("cam1", "/rec/cam1/c.ts", base.Add(60*time.Second))

	s := &Server{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:                  "cam1",
				RecordSegmentDuration: conf.Duration(10 * time.Second),
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
		Index:       idx,
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(s.onRequest)
	ts := httptest.NewServer(r)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/cam1/ranges.json/?closed_at_gte=1025&opened_at_lte=2000&limit=1000&resolution=0")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var out RangesJSON
	require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
	require.Equal(t, 1, out.EstimatedCount)
	require.Equal(t, []DVRRange{
		{Duration: 10, From: 1060, OpenedAt: 1060, ClosedAt: 1070},
	}, out.Ranges)

	res2, err := http.Get(ts.URL + "/cam1/ranges.json")
	require.NoError(t, err)
	defer res2.Body.Close()
	require.Equal(t, http.StatusOK, res2.StatusCode)

	var all RangesJSON
	require.NoError(t, json.NewDecoder(res2.Body).Decode(&all))
	require.Equal(t, 2, all.EstimatedCount)
	require.Equal(t, int64(1000), all.Ranges[0].From)
	require.Equal(t, int64(1020), all.Ranges[0].ClosedAt)
}
