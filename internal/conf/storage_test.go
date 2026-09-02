package conf

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecordPathRel(t *testing.T) {
	require.Equal(t, "%path/%Y-%m-%d_%H-%M-%S-%f", RecordPathRel("%path/%Y-%m-%d_%H-%M-%S-%f"))
	require.Equal(t, "%path/%Y-%m-%d_%H-%M-%S-%f", RecordPathRel("./recordings/%path/%Y-%m-%d_%H-%M-%S-%f"))
	require.Equal(t, "%path/%Y-%m-%d_%H-%M-%S-%f", RecordPathRel("/mnt/data/%path/%Y-%m-%d_%H-%M-%S-%f"))
}

func TestRecordPathFormats(t *testing.T) {
	p := &Path{RecordPath: "./recordings/%path/%Y-%m-%d_%H-%M-%S-%f"}
	require.Equal(t, []string{p.RecordPath}, p.RecordPathFormats())

	p.StorageDisks = []string{"/mnt/s1", "/mnt/s2"}
	require.Equal(t, []string{
		filepath.Join("/mnt/s1", "%path/%Y-%m-%d_%H-%M-%S-%f"),
		filepath.Join("/mnt/s2", "%path/%Y-%m-%d_%H-%M-%S-%f"),
	}, p.RecordPathFormats())
}

func TestStorageDefaults(t *testing.T) {
	tmpf := createTempFile(t, []byte(
		"storages:\n"+
			"  dvr:\n"+
			"    disks:\n"+
			"      - /mnt/a\n"+
			"      - /mnt/b\n"+
			"pathDefaults:\n"+
			"  storage: dvr\n"+
			"  recordPath: '%path/%Y-%m-%d_%H-%M-%S-%f'\n"+
			"paths:\n"+
			"  cam1:\n"))

	conf, _, err := Load(tmpf, nil, nil)
	require.NoError(t, err)
	s := conf.Storages["dvr"]
	require.NotNil(t, s)
	require.Equal(t, StorageStrategyRoundRobin, s.Strategy)
	require.NotNil(t, s.MaxUsedPercent)
	require.Equal(t, float64(90), *s.MaxUsedPercent)
	require.Equal(t, "dvr", conf.Paths["cam1"].Storage)
	require.Equal(t, []string{"/mnt/a", "/mnt/b"}, conf.Paths["cam1"].StorageDisks)
}
