package core

import (
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recorder"
	"github.com/bluenviron/mediamtx/internal/test"
)

type dummyPublisher struct{}

func (d *dummyPublisher) Close() {}

func (d *dummyPublisher) Log(_ logger.Level, _ string, _ ...any) {}

func (d *dummyPublisher) APISourceDescribe() *defs.APIPathSource {
	return nil
}

type dummyReader struct{}

func (d *dummyReader) Close() {}

func (d *dummyReader) Log(_ logger.Level, _ string, _ ...any) {}

func (d *dummyReader) APIReaderDescribe() *defs.APIPathReader {
	return nil
}

type pathEnableListener struct {
	disabled []string
	enabled  []string
}

func (l *pathEnableListener) OnSegmentCreate(string, string) {}
func (l *pathEnableListener) OnSegmentComplete(string, string, time.Duration, []recorder.SegmentPart) {
}
func (l *pathEnableListener) OnSegmentRemove(string) {}
func (l *pathEnableListener) OnPathDisabled(name string) {
	l.disabled = append(l.disabled, name)
}
func (l *pathEnableListener) OnPathEnabled(name string) {
	l.enabled = append(l.enabled, name)
}

func TestPathManagerDynamicPathAutoDeletion(t *testing.T) {
	for _, ca := range []string{"describe", "add reader"} {
		t.Run(ca, func(t *testing.T) {
			pathConfs := map[string]*conf.Path{
				"all_others": {
					Regexp:  regexp.MustCompile("^.*$"),
					Name:    "all_others",
					Enabled: true,
					Source:  "publisher",
				},
			}

			pm := &pathManager{
				authManager: test.NilAuthManager,
				pathConfs:   pathConfs,
				parent:      test.NilLogger,
			}
			pm.initialize()
			defer pm.close()

			func() {
				if ca == "describe" {
					_, err := pm.Describe(defs.PathDescribeReq{
						AccessRequest: defs.PathAccessRequest{
							Name: "mypath",
						},
					})
					require.EqualError(t, err, "no stream is available on path 'mypath'")
				} else {
					_, err := pm.AddReader(defs.PathAddReaderReq{
						Author: &dummyReader{},
						AccessRequest: defs.PathAccessRequest{
							Name: "mypath",
						},
					})
					require.EqualError(t, err, "no stream is available on path 'mypath'")
				}
			}()

			time.Sleep(100 * time.Millisecond)

			data, err := pm.APIPathsList()
			require.NoError(t, err)

			require.Empty(t, data.Items)
		})
	}
}

func TestPathManagerDynamicPathDescribeAndPublish(t *testing.T) {
	pathConfs := map[string]*conf.Path{
		"all_others": {
			Regexp:  regexp.MustCompile("^.*$"),
			Name:    "all_others",
			Enabled: true,
			Source:  "publisher",
		},
	}

	pm := &pathManager{
		authManager: test.NilAuthManager,
		pathConfs:   pathConfs,
		parent:      test.NilLogger,
	}
	pm.initialize()
	defer pm.close()

	go func() {
		for range 10 {
			pm.Describe(defs.PathDescribeReq{ //nolint:errcheck
				AccessRequest: defs.PathAccessRequest{
					Name: "mypath",
				},
			})
		}
	}()

	_, err := pm.AddPublisher(defs.PathAddPublisherReq{
		Author: &dummyPublisher{},
		Desc:   &description.Session{},
		AccessRequest: defs.PathAccessRequest{
			Name: "mypath",
		},
	})
	require.NoError(t, err)
}

func TestPathManagerDisabledPath(t *testing.T) {
	pathConfs := map[string]*conf.Path{
		"cam1": {
			Name:    "cam1",
			Enabled: false,
			Source:  "publisher",
		},
	}

	pm := &pathManager{
		authManager: test.NilAuthManager,
		pathConfs:   pathConfs,
		parent:      test.NilLogger,
	}
	pm.initialize()
	defer pm.close()

	data, err := pm.APIPathsList()
	require.NoError(t, err)
	require.Empty(t, data.Items)

	_, err = pm.Describe(defs.PathDescribeReq{
		AccessRequest: defs.PathAccessRequest{Name: "cam1"},
	})
	require.EqualError(t, err, "path 'cam1' is disabled")

	_, err = pm.AddPublisher(defs.PathAddPublisherReq{
		Author:        &dummyPublisher{},
		Desc:          &description.Session{},
		AccessRequest: defs.PathAccessRequest{Name: "cam1"},
	})
	require.EqualError(t, err, "path 'cam1' is disabled")

	_, err = pm.AddReader(defs.PathAddReaderReq{
		Author:        &dummyReader{},
		AccessRequest: defs.PathAccessRequest{Name: "cam1"},
	})
	require.EqualError(t, err, "path 'cam1' is disabled")

	_, err = pm.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{Name: "cam1"},
	})
	require.EqualError(t, err, "path 'cam1' is disabled")

	pm.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:    "cam1",
			Enabled: true,
			Source:  "publisher",
		},
	})
	time.Sleep(50 * time.Millisecond)

	data, err = pm.APIPathsList()
	require.NoError(t, err)
	require.Len(t, data.Items, 1)
	require.Equal(t, "cam1", data.Items[0].Name)
}

func TestPathManagerEnableDisableNotifiesIndex(t *testing.T) {
	pathConfs := map[string]*conf.Path{
		"cam1": {
			Name:    "cam1",
			Enabled: true,
			Source:  "publisher",
		},
	}

	listener := &pathEnableListener{}
	pm := &pathManager{
		authManager: test.NilAuthManager,
		pathConfs:   pathConfs,
		parent:      test.NilLogger,
	}
	pm.initialize()
	defer pm.close()
	pm.SetRecordSegmentListener(listener)

	pm.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:    "cam1",
			Enabled: false,
			Source:  "publisher",
		},
	})
	require.Eventually(t, func() bool {
		data, err := pm.APIPathsList()
		return err == nil && len(data.Items) == 0
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"cam1"}, listener.disabled)
	require.Empty(t, listener.enabled)

	pm.ReloadPathConfs(map[string]*conf.Path{
		"cam1": {
			Name:    "cam1",
			Enabled: true,
			Source:  "publisher",
		},
	})
	require.Eventually(t, func() bool {
		data, err := pm.APIPathsList()
		return err == nil && len(data.Items) == 1
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"cam1"}, listener.enabled)
}

func TestPathManagerConfigHotReload(t *testing.T) {
	// Start MediaMTX with basic configuration
	p, ok := newInstance(t, "api: yes\n"+
		"paths:\n"+
		"  all:\n"+
		"    record: no\n")
	require.Equal(t, true, ok)
	defer p.Close()

	// Set up HTTP client for API calls
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	// Create a publisher that will use the "all" configuration
	media0 := test.UniqueMediaH264()
	source := gortsplib.Client{}
	err := source.StartRecording(
		"rtsp://localhost:8554/undefined_stream",
		&description.Session{Medias: []*description.Media{media0}})
	require.NoError(t, err)
	defer source.Close()

	// Send some data to establish the stream
	err = source.WritePacketRTP(media0, &rtp.Packet{
		Header: rtp.Header{
			Version:     2,
			PayloadType: 96,
		},
		Payload: []byte{5, 1, 2, 3, 4},
	})
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	// Verify the path exists and is using the "all" configuration
	pathData, err := p.pathManager.APIPathsGet("undefined_stream")
	require.NoError(t, err)
	require.Equal(t, "undefined_stream", pathData.Name)
	require.Equal(t, "all", pathData.ConfName)

	// Check the current configuration via API
	var allConfig map[string]any
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/config/paths/get/all", nil, &allConfig)
	require.Equal(t, false, allConfig["record"]) // Should be false from "all" config

	// Add a new specific configuration for "undefined_stream" with record enabled
	httpRequest(t, hc, http.MethodPost, "http://localhost:9997/v3/config/paths/add/undefined_stream",
		map[string]any{
			"record": true,
		}, nil)

	// Give the system time to process the configuration change
	time.Sleep(200 * time.Millisecond)

	// Verify the path now uses the new specific configuration
	pathData, err = p.pathManager.APIPathsGet("undefined_stream")
	require.NoError(t, err)
	require.Equal(t, "undefined_stream", pathData.Name)
	require.Equal(t, "undefined_stream", pathData.ConfName) // Should now use the specific config

	// Check the new configuration via API
	var newConfig map[string]any
	httpRequest(t, hc, http.MethodGet, "http://localhost:9997/v3/config/paths/get/undefined_stream", nil, &newConfig)
	require.Equal(t, true, newConfig["record"]) // Should be true from new config

	// Verify the stream is still active and working
	err = source.WritePacketRTP(media0, &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 2,
		},
		Payload: []byte{5, 1, 2, 3, 4},
	})
	require.NoError(t, err)

	// Verify the path is still ready and functional
	require.Equal(t, true, pathData.Ready)

	// revert configuration
	httpRequest(t, hc, http.MethodDelete, "http://localhost:9997/v3/config/paths/delete/undefined_stream",
		nil, nil)

	// Give the system time to process the configuration change
	time.Sleep(200 * time.Millisecond)

	// Verify the path now uses the old configuration
	pathData, err = p.pathManager.APIPathsGet("undefined_stream")
	require.NoError(t, err)
	require.Equal(t, "undefined_stream", pathData.Name)
	require.Equal(t, "all", pathData.ConfName)
}
