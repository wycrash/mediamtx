package sysmetrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

type fakeSampler struct {
	cpu     float64
	procCPU float64
	cores   int
	total   uint64
	used    uint64
	avail   uint64
	usedPct float64
	rss     uint64
	usage   diskUsage
	parts   []partition
	io      map[string]ioSample
	net     []netSample
}

func (f *fakeSampler) cpuPercent() (float64, error) {
	return f.cpu, nil
}

func (f *fakeSampler) processCPUPercent() (float64, error) {
	return f.procCPU, nil
}

func (f *fakeSampler) cpuCores() (int, error) {
	return f.cores, nil
}

func (f *fakeSampler) memory() (uint64, uint64, uint64, float64, error) {
	return f.total, f.used, f.avail, f.usedPct, nil
}

func (f *fakeSampler) processRSS() (uint64, error) {
	return f.rss, nil
}

func (f *fakeSampler) diskUsage(_ string) (diskUsage, error) {
	return f.usage, nil
}

func (f *fakeSampler) partitions() ([]partition, error) {
	return f.parts, nil
}

func (f *fakeSampler) diskIO() (map[string]ioSample, error) {
	return f.io, nil
}

func (f *fakeSampler) netIO() ([]netSample, error) {
	return f.net, nil
}

func TestRecordDirs(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	dirs := RecordDirs("./recordings/%path/%Y-%m-%d_%H-%M-%S-%f", map[string]*conf.Path{
		"cam1": {RecordPath: "./recordings/%path/%Y-%m-%d_%H-%M-%S-%f"},
		"cam2": {RecordPath: "./other/%path/%Y-%m-%d_%H-%M-%S-%f"},
	})
	require.Equal(t, []string{
		filepath.Join(cwd, "other"),
		filepath.Join(cwd, "recordings"),
	}, dirs)
}

func TestCollectorSnapshot(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeSampler{
		cpu:     12.5,
		procCPU: 4.5,
		cores:   8,
		total:   8000,
		used:    3000,
		avail:   5000,
		usedPct: 37.5,
		rss:     100,
		usage: diskUsage{
			total:       1000,
			used:        400,
			free:        600,
			usedPercent: 40,
		},
		parts: []partition{{mountpoint: dir, device: "/dev/sda1"}},
		io:    map[string]ioSample{"sda1": {readBytes: 1000, writeBytes: 2000}},
		net: []netSample{
			{name: "eth1", recv: 10, sent: 20},
			{name: "eth0", recv: 100, sent: 200},
		},
	}

	clock := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	c := &Collector{
		Interval:    time.Second,
		RecordPaths: []string{dir},
		sample:      fake,
		now:         func() time.Time { return clock },
	}
	err := c.Initialize()
	require.NoError(t, err)
	defer c.Close()

	fake.io = map[string]ioSample{"sda1": {readBytes: 3000, writeBytes: 6000}}
	fake.net = []netSample{
		{name: "eth1", recv: 110, sent: 220},
		{name: "eth0", recv: 2100, sent: 2200},
	}
	clock = clock.Add(time.Second)
	c.collect()

	snap := c.Snapshot()
	require.Equal(t, 12.5, snap.CPU.Percent)
	require.InDelta(t, 4.5/8, snap.CPU.ProcessPercent, 0.001)
	require.Equal(t, 8, snap.CPU.Cores)
	require.Equal(t, uint64(8000), snap.Memory.TotalBytes)
	require.Equal(t, uint64(100), snap.Memory.ProcessRssBytes)
	require.Equal(t, []string{"eth0", "eth1"}, []string{snap.Network[0].Name, snap.Network[1].Name})
	require.InDelta(t, 2000, snap.Network[0].RecvBytesPerSec, 0.1)
	require.InDelta(t, 2000, snap.Network[0].SentBytesPerSec, 0.1)
	require.InDelta(t, 100, snap.Network[1].RecvBytesPerSec, 0.1)
	require.NotEmpty(t, snap.Disks)
	require.InDelta(t, 2000, snap.Disks[0].ReadBytesPerSec, 0.1)
	require.InDelta(t, 4000, snap.Disks[0].WriteBytesPerSec, 0.1)
	require.Len(t, snap.History, 2)
	require.Equal(t, clock, snap.History[1].CollectedAt)
	require.InDelta(t, 12.5, snap.History[1].CPUPercent, 0.001)
	require.Equal(t, uint64(3000), snap.History[1].MemUsedBytes)
	require.InDelta(t, 40, snap.History[1].DiskUsedPercent, 0.001)
	require.InDelta(t, 2100, snap.History[1].NetRecvBytesPerSec, 0.1)
	require.InDelta(t, 2200, snap.History[1].NetSentBytesPerSec, 0.1)
}

func TestCollectorLive(t *testing.T) {
	c := &Collector{
		Interval:    time.Hour,
		RecordPaths: RecordDirs("./recordings/%path/%Y-%m-%d", nil),
	}
	err := c.Initialize()
	require.NoError(t, err)
	defer c.Close()

	snap := c.Snapshot()
	require.False(t, snap.CollectedAt.IsZero())
	require.Greater(t, snap.CPU.Cores, 0)
	require.Greater(t, snap.Memory.TotalBytes, uint64(0))
	require.NotNil(t, snap.Disks)
	require.NotNil(t, snap.Network)
	require.NotNil(t, snap.History)
}

func TestCollectorHistoryRing(t *testing.T) {
	fake := &fakeSampler{cores: 1, usage: diskUsage{total: 1, free: 1}}
	clock := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	c := &Collector{
		Interval: time.Second,
		sample:   fake,
		now:      func() time.Time { return clock },
	}
	err := c.Initialize()
	require.NoError(t, err)
	defer c.Close()

	cap := historyCap(time.Second)
	require.Equal(t, 15*60, cap)

	for i := 1; i < cap+5; i++ {
		clock = clock.Add(time.Second)
		fake.cpu = float64(i)
		c.collect()
	}

	snap := c.Snapshot()
	require.Len(t, snap.History, cap)
	require.Equal(t, 5.0, snap.History[0].CPUPercent)
	require.Equal(t, float64(cap+4), snap.History[cap-1].CPUPercent)
	require.Equal(t, clock, snap.History[cap-1].CollectedAt)
}

func TestIsLoopback(t *testing.T) {
	require.True(t, isLoopback("lo"))
	require.True(t, isLoopback("lo0"))
	require.True(t, isLoopback("Loopback"))
	require.False(t, isLoopback("eth0"))
}

func TestIODeviceKeys(t *testing.T) {
	require.Contains(t, ioDeviceKeys("/dev/sda1"), "sda1")
	require.Contains(t, ioDeviceKeys("/dev/sda1"), "sda")
	require.Contains(t, ioDeviceKeys("C:"), "C:")
}

func TestHasPathPrefix(t *testing.T) {
	sep := string(filepath.Separator)
	root := string(filepath.Separator)
	require.True(t, hasPathPrefix(root+"data"+sep+"rec", root+"data"))
	require.True(t, hasPathPrefix(root+"data", root+"data"))
	require.False(t, hasPathPrefix(root+"data", root+"data2"))
}
