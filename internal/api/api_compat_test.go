package api //nolint:revive

import (
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/compatapi"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/test"
)

type testCompatServer struct {
	sessions map[string]*defs.APICompatSession
}

func (s *testCompatServer) APISessionsList() (*defs.APICompatSessionList, error) {
	items := make([]defs.APICompatSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		items = append(items, *session)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Created.Before(items[j].Created)
	})
	return &defs.APICompatSessionList{Items: items}, nil
}

func (s *testCompatServer) APISessionsGet(id uuid.UUID) (*defs.APICompatSession, error) {
	session, ok := s.sessions[id.String()]
	if !ok {
		return nil, compatapi.ErrSessionNotFound
	}
	return session, nil
}

func (s *testCompatServer) APISessionsKick(id uuid.UUID) error {
	_, ok := s.sessions[id.String()]
	if !ok {
		return compatapi.ErrSessionNotFound
	}
	delete(s.sessions, id.String())
	return nil
}

func TestCompatSessionsList(t *testing.T) {
	now := testTime
	compatServer := &testCompatServer{
		sessions: map[string]*defs.APICompatSession{
			"session1": {
				ID:            uuid.MustParse("18294761-f9d1-4ea9-9a35-fe265b62eb41"),
				Created:       now,
				RemoteAddr:    "192.168.1.1:5000",
				Path:          "stream1",
				Query:         "key=val1",
				User:          "user1",
				OutboundBytes: 111,
			},
			"session2": {
				ID:            uuid.MustParse("18294761-f9d1-4ea9-9a35-fe265b62eb42"),
				Created:       now.Add(time.Minute),
				RemoteAddr:    "192.168.1.2:5001",
				Path:          "stream2",
				Query:         "key=val2",
				User:          "user2",
				OutboundBytes: 222,
			},
		},
	}

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		CompatServer: compatServer,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out defs.APICompatSessionList
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/compatsessions/list", nil, &out)

	require.Equal(t, 2, out.ItemCount)
	require.Equal(t, 1, out.PageCount)
	require.Len(t, out.Items, 2)
	require.Equal(t, []defs.APICompatSession{
		{
			ID:            uuid.MustParse("18294761-f9d1-4ea9-9a35-fe265b62eb41"),
			Created:       now,
			RemoteAddr:    "192.168.1.1:5000",
			Path:          "stream1",
			Query:         "key=val1",
			User:          "user1",
			OutboundBytes: 111,
		},
		{
			ID:            uuid.MustParse("18294761-f9d1-4ea9-9a35-fe265b62eb42"),
			Created:       now.Add(time.Minute),
			RemoteAddr:    "192.168.1.2:5001",
			Path:          "stream2",
			Query:         "key=val2",
			User:          "user2",
			OutboundBytes: 222,
		},
	}, out.Items)
}

func TestCompatSessionsGet(t *testing.T) {
	now := testTime
	sessionID := "18294761-f9d1-4ea9-9a35-fe265b62eb41"
	compatServer := &testCompatServer{
		sessions: map[string]*defs.APICompatSession{
			sessionID: {
				ID:            uuid.MustParse(sessionID),
				Created:       now,
				RemoteAddr:    "192.168.1.100:5000",
				Path:          "mystream",
				Query:         "key=val",
				User:          "myuser",
				OutboundBytes: 345,
			},
		},
	}

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		CompatServer: compatServer,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	var out defs.APICompatSession
	httpRequest(t, hc, http.MethodGet, fmt.Sprintf("http://localhost:9997/v3/compatsessions/get/%s", sessionID), nil, &out)

	require.Equal(t, uuid.MustParse(sessionID), out.ID)
	require.Equal(t, "192.168.1.100:5000", out.RemoteAddr)
	require.Equal(t, "mystream", out.Path)
	require.Equal(t, "key=val", out.Query)
	require.Equal(t, "myuser", out.User)
	require.Equal(t, uint64(345), out.OutboundBytes)
}

func TestCompatSessionsKick(t *testing.T) {
	now := testTime
	sessionID := uuid.MustParse("18294761-f9d1-4ea9-9a35-fe265b62eb41")
	compatServer := &testCompatServer{
		sessions: map[string]*defs.APICompatSession{
			sessionID.String(): {
				ID:            sessionID,
				Created:       now,
				RemoteAddr:    "192.168.1.100:5000",
				Path:          "mystream",
				OutboundBytes: 345,
			},
		},
	}

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		CompatServer: compatServer,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	httpRequest(t, hc, http.MethodPost, fmt.Sprintf("http://localhost:9997/v3/compatsessions/kick/%s", sessionID), nil, nil)

	_, ok := compatServer.sessions[sessionID.String()]
	require.False(t, ok)
}

func TestCompatSessionsKickNotFound(t *testing.T) {
	compatServer := &testCompatServer{}

	api := API{
		Address:      "localhost:9997",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		CompatServer: compatServer,
		Parent:       &testParent{},
	}
	err := api.Initialize()
	require.NoError(t, err)
	defer api.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://localhost:9997/v3/compatsessions/kick/%s", uuid.New()), nil)
	require.NoError(t, err)

	res, err := hc.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusNotFound, res.StatusCode)
	checkError(t, res.Body, "session not found")
}
