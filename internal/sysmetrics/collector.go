// Package sysmetrics collects host CPU, memory, disk and network stats.
package sysmetrics

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/defs"
)

const (
	defaultInterval = time.Second
	historyDuration = 15 * time.Minute
)

// Collector periodically samples host metrics.
type Collector struct {
	Interval    time.Duration
	RecordPaths []string

	sample sampler
	now    func() time.Time

	ctx       context.Context
	ctxCancel func()
	done      chan struct{}

	mu          sync.RWMutex
	recordPaths []string
	snapshot    defs.APISystemMetrics
	prevNet     map[string]netSample
	prevDisk    map[string]ioSample
	prevTime    time.Time
	histBuf     []defs.APISystemMetricsPoint
	histPos     int
	histLen     int
}

// Initialize starts background sampling.
func (c *Collector) Initialize() error {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.sample == nil {
		c.sample = newHostSampler()
	}
	if c.now == nil {
		c.now = time.Now
	}

	c.recordPaths = slices.Clone(c.RecordPaths)
	c.snapshot.Disks = []defs.APISystemMetricsDisk{}
	c.snapshot.Network = []defs.APISystemMetricsNIC{}
	c.snapshot.History = []defs.APISystemMetricsPoint{}
	c.histBuf = make([]defs.APISystemMetricsPoint, historyCap(c.Interval))
	c.ctx, c.ctxCancel = context.WithCancel(context.Background())
	c.done = make(chan struct{})

	c.collect()

	go c.run()
	return nil
}

// Close stops background sampling.
func (c *Collector) Close() {
	c.ctxCancel()
	<-c.done
}

// SetRecordPaths updates the recording directories whose volumes are sampled.
func (c *Collector) SetRecordPaths(paths []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordPaths = slices.Clone(paths)
}

// Snapshot returns the last collected sample.
func (c *Collector) Snapshot() defs.APISystemMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snapshot
	s.Disks = slices.Clone(c.snapshot.Disks)
	s.Network = slices.Clone(c.snapshot.Network)
	s.History = c.historyChrono()
	return s
}

func (c *Collector) run() {
	defer close(c.done)
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			c.collect()
		}
	}
}

func (c *Collector) collect() {
	now := c.now()
	dt := 0.0
	c.mu.RLock()
	if !c.prevTime.IsZero() {
		dt = now.Sub(c.prevTime).Seconds()
	}
	prevNet := c.prevNet
	prevDisk := c.prevDisk
	recordPaths := slices.Clone(c.recordPaths)
	c.mu.RUnlock()

	snap := defs.APISystemMetrics{
		CollectedAt: now,
		Disks:       []defs.APISystemMetricsDisk{},
		Network:     []defs.APISystemMetricsNIC{},
	}

	if v, err := c.sample.cpuPercent(); err == nil {
		snap.CPU.Percent = v
	}
	if v, err := c.sample.processCPUPercent(); err == nil {
		snap.CPU.ProcessPercent = v
	}
	if v, err := c.sample.cpuCores(); err == nil {
		snap.CPU.Cores = v
	}
	if snap.CPU.Cores > 0 {
		snap.CPU.ProcessPercent /= float64(snap.CPU.Cores)
	}
	if total, used, avail, pct, err := c.sample.memory(); err == nil {
		snap.Memory.TotalBytes = total
		snap.Memory.UsedBytes = used
		snap.Memory.AvailableBytes = avail
		snap.Memory.UsedPercent = pct
	}
	if v, err := c.sample.processRSS(); err == nil {
		snap.Memory.ProcessRssBytes = v
	}

	parts, _ := c.sample.partitions()
	diskIO, _ := c.sample.diskIO()
	if diskIO == nil {
		diskIO = map[string]ioSample{}
	}

	nextDisk := make(map[string]ioSample, len(recordPaths))
	for _, dir := range recordPaths {
		d := defs.APISystemMetricsDisk{Path: dir}
		usagePath := existingAncestor(dir)
		if u, err := c.sample.diskUsage(usagePath); err == nil {
			d.TotalBytes = u.total
			d.UsedBytes = u.used
			d.FreeBytes = u.free
			d.UsedPercent = u.usedPercent
		}

		io, ok := matchDiskIO(dir, parts, diskIO)
		if ok {
			nextDisk[d.Path] = io
			if prev, found := prevDisk[d.Path]; found {
				d.ReadBytesPerSec = perSec(prev.readBytes, io.readBytes, dt)
				d.WriteBytesPerSec = perSec(prev.writeBytes, io.writeBytes, dt)
			}
		}

		snap.Disks = append(snap.Disks, d)
	}

	netIO, _ := c.sample.netIO()
	nextNet := make(map[string]netSample, len(netIO))
	for _, n := range netIO {
		nextNet[n.name] = n
		nic := defs.APISystemMetricsNIC{Name: n.name}
		if prev, found := prevNet[n.name]; found {
			nic.RecvBytesPerSec = perSec(prev.recv, n.recv, dt)
			nic.SentBytesPerSec = perSec(prev.sent, n.sent, dt)
		}
		snap.Network = append(snap.Network, nic)
	}
	sortNICs(snap.Network)

	c.mu.Lock()
	c.snapshot = snap
	c.prevNet = nextNet
	c.prevDisk = nextDisk
	c.prevTime = now
	c.pushHistory(compactPoint(snap))
	c.mu.Unlock()
}

func perSec(prev, cur uint64, dt float64) float64 {
	if dt <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / dt
}

func matchDiskIO(path string, parts []partition, ios map[string]ioSample) (ioSample, bool) {
	bestLen := -1
	var best partition
	for _, p := range parts {
		if !hasPathPrefix(path, p.mountpoint) {
			continue
		}
		n := len(filepath.Clean(p.mountpoint))
		if n > bestLen {
			best = p
			bestLen = n
		}
	}
	if bestLen < 0 {
		return ioSample{}, false
	}
	for _, key := range ioDeviceKeys(best.device) {
		if s, ok := ios[key]; ok {
			return s, true
		}
	}
	return ioSample{}, false
}

func hasPathPrefix(path, prefix string) bool {
	path = filepath.Clean(path)
	prefix = filepath.Clean(prefix)
	if path == prefix {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(path, prefix)
}

func ioDeviceKeys(device string) []string {
	device = strings.TrimPrefix(device, `\\.\`)
	base := filepath.Base(device)
	keys := []string{device, base}
	trimmed := strings.TrimRight(base, "0123456789")
	trimmed = strings.TrimSuffix(trimmed, "p")
	if trimmed != "" && trimmed != base {
		keys = append(keys, trimmed)
	}
	return keys
}

func sortNICs(nics []defs.APISystemMetricsNIC) {
	slices.SortFunc(nics, func(a, b defs.APISystemMetricsNIC) int {
		return strings.Compare(a.Name, b.Name)
	})
}

func historyCap(interval time.Duration) int {
	n := int(historyDuration / interval)
	if n < 1 {
		return 1
	}
	return n
}

func compactPoint(snap defs.APISystemMetrics) defs.APISystemMetricsPoint {
	p := defs.APISystemMetricsPoint{
		CollectedAt:     snap.CollectedAt,
		CPUPercent:      snap.CPU.Percent,
		ProcessPercent:  snap.CPU.ProcessPercent,
		MemUsedBytes:    snap.Memory.UsedBytes,
		MemUsedPercent:  snap.Memory.UsedPercent,
		ProcessRssBytes: snap.Memory.ProcessRssBytes,
	}
	if len(snap.Disks) > 0 {
		p.DiskUsedBytes = snap.Disks[0].UsedBytes
		p.DiskUsedPercent = snap.Disks[0].UsedPercent
		for _, d := range snap.Disks {
			p.DiskReadBytesPerSec += d.ReadBytesPerSec
			p.DiskWriteBytesPerSec += d.WriteBytesPerSec
		}
	}
	for _, n := range snap.Network {
		p.NetRecvBytesPerSec += n.RecvBytesPerSec
		p.NetSentBytesPerSec += n.SentBytesPerSec
	}
	return p
}

func (c *Collector) pushHistory(p defs.APISystemMetricsPoint) {
	if len(c.histBuf) == 0 {
		return
	}
	c.histBuf[c.histPos] = p
	c.histPos = (c.histPos + 1) % len(c.histBuf)
	if c.histLen < len(c.histBuf) {
		c.histLen++
	}
}

func (c *Collector) historyChrono() []defs.APISystemMetricsPoint {
	if c.histLen == 0 {
		return []defs.APISystemMetricsPoint{}
	}
	out := make([]defs.APISystemMetricsPoint, c.histLen)
	start := c.histPos - c.histLen
	if start < 0 {
		start += len(c.histBuf)
	}
	for i := 0; i < c.histLen; i++ {
		out[i] = c.histBuf[(start+i)%len(c.histBuf)]
	}
	return out
}
