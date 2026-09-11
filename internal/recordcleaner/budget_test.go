package recordcleaner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

func TestStoragePathStatsAverages(t *testing.T) {
	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"cam1": {
				Name:              "cam1",
				Storage:           "dvr",
				Record:            true,
				RecordMaxPartSize: 40 * 1024 * 1024,
				RecordPartDuration: conf.Duration(4 * time.Second),
			},
			"cam2": {
				Name:              "cam2",
				Storage:           "dvr",
				Record:            true,
				RecordMaxPartSize: 60 * 1024 * 1024,
				RecordPartDuration: conf.Duration(6 * time.Second),
			},
			"other": {
				Name:              "other",
				Storage:           "other",
				Record:            true,
				RecordMaxPartSize: 10 * 1024 * 1024,
				RecordPartDuration: conf.Duration(time.Second),
			},
			"off": {
				Name:              "off",
				Storage:           "dvr",
				Record:            false,
				RecordMaxPartSize: 10 * 1024 * 1024,
			},
		},
	}

	st := c.storagePathStats("dvr")
	require.Equal(t, 2, st.cameras)
	require.Equal(t, uint64(50*1024*1024), st.segSize) // (40+60)/2
	require.Equal(t, 5*time.Second, st.partDur)         // (4+6)/2
}

func TestDeleteBudgetForDisk(t *testing.T) {
	// Need 7.6% of 1TiB ≈ 77.8GiB; avg part 50MiB → ~1594 deletes.
	total := uint64(1024) * 1024 * 1024 * 1024
	avg := uint64(50) * 1024 * 1024
	n := deleteBudgetForDisk(72.6, 65, total, avg, 27, 5*time.Second)
	require.Equal(t, 1594, n)

	// Without disk total, still keep ingest floor for cameras / min bound.
	n = deleteBudgetForDisk(90, 80, 0, avg, 27, 5*time.Second)
	require.GreaterOrEqual(t, n, minDeletesPerDiskPerTick)
}
