package compatapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

func TestServerReloadPathConfsLoadsNewPathInBackground(t *testing.T) {
	dir := t.TempDir()
	cam := filepath.Join(dir, "cam1")
	writeNamedFMP4(t, filepath.Join(cam, "2020-01-01_00-00-00-000000.mp4"), 2)
	writeNamedFMP4(t, filepath.Join(cam, "2020-01-01_00-00-05-000000.mp4"), 3)

	pathConf := &conf.Path{
		Name:                  "cam1",
		Enabled:               true,
		RecordPath:            filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		RecordFormat:          conf.RecordFormatFMP4,
		RecordSegmentDuration: conf.Duration(5 * time.Second),
	}
	confs := map[string]*conf.Path{"cam1": pathConf}

	idx1 := NewIndex()
	require.Equal(t, 0, idx1.LoadFromDisk(confs).Segments)
	require.Equal(t, 2, idx1.ReconcileAll(nil, false).Segments)
	idx1.ClosePersist()

	s := &Server{Parent: test.NilLogger, Index: NewIndex()}
	s.reconcileStop = make(chan struct{})
	s.reconcileKick = make(chan struct{}, 1)
	s.indexLoadKick = make(chan struct{}, 1)
	s.indexLoadDone = make(chan struct{})
	go s.runIndexLoads()
	defer func() {
		close(s.reconcileStop)
		<-s.indexLoadDone
		s.Index.ClosePersist()
	}()

	s.ReloadPathConfs(confs)
	require.Equal(t, confs, s.PathConfs)

	require.Eventually(t, func() bool {
		return len(s.Index.SegmentsInWindow("cam1", time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local), time.Minute)) == 2
	}, 2*time.Second, 10*time.Millisecond)
}
