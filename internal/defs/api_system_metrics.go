package defs

import "time"

// APISystemMetrics is a snapshot of host CPU, memory, disk and network stats.
type APISystemMetrics struct {
	CollectedAt time.Time               `json:"collectedAt"`
	CPU         APISystemMetricsCPU     `json:"cpu"`
	Memory      APISystemMetricsMemory  `json:"memory"`
	Disks       []APISystemMetricsDisk  `json:"disks"`
	Network     []APISystemMetricsNIC   `json:"network"`
	History     []APISystemMetricsPoint `json:"history"`
}

// APISystemMetricsCPU is host and process CPU usage.
type APISystemMetricsCPU struct {
	// Processor name, for example "Intel(R) Core(TM) i7-9700".
	Model          string  `json:"model"`
	Percent        float64 `json:"percent"`
	ProcessPercent float64 `json:"processPercent"`
	Cores          int     `json:"cores"`
}

// APISystemMetricsMemory is host and process memory usage.
type APISystemMetricsMemory struct {
	TotalBytes      uint64  `json:"totalBytes"`
	UsedBytes       uint64  `json:"usedBytes"`
	AvailableBytes  uint64  `json:"availableBytes"`
	UsedPercent     float64 `json:"usedPercent"`
	ProcessRssBytes uint64  `json:"processRssBytes"`
}

// Recording disk health as reported by GET /v3/metrics/system.
const (
	APIDiskStatusOK    = "ok"
	APIDiskStatusError = "error"
)

// APISystemMetricsDisk is usage and IO of a volume used for recordings.
type APISystemMetricsDisk struct {
	Path             string  `json:"path"`
	TotalBytes       uint64  `json:"totalBytes"`
	UsedBytes        uint64  `json:"usedBytes"`
	FreeBytes        uint64  `json:"freeBytes"`
	UsedPercent      float64 `json:"usedPercent"`
	ReadBytesPerSec  float64 `json:"readBytesPerSec"`
	WriteBytesPerSec float64 `json:"writeBytesPerSec"`
	// Status is "ok" when MediaMTX can write to the disk, "error" when the
	// volume is missing, hung, or skipped after a write failure.
	Status string `json:"status"`
	// Error is set when Status is "error".
	Error string `json:"error,omitempty"`
}

// APISystemMetricsNIC is traffic of a network interface.
type APISystemMetricsNIC struct {
	Name            string  `json:"name"`
	RecvBytesPerSec float64 `json:"recvBytesPerSec"`
	SentBytesPerSec float64 `json:"sentBytesPerSec"`
}

// APISystemMetricsPoint is a compact sample for dashboard graphs.
type APISystemMetricsPoint struct {
	CollectedAt          time.Time `json:"collectedAt"`
	CPUPercent           float64   `json:"cpuPercent"`
	ProcessPercent       float64   `json:"processPercent"`
	MemUsedBytes         uint64    `json:"memUsedBytes"`
	MemUsedPercent       float64   `json:"memUsedPercent"`
	ProcessRssBytes      uint64    `json:"processRssBytes"`
	DiskUsedBytes        uint64    `json:"diskUsedBytes"`
	DiskUsedPercent      float64   `json:"diskUsedPercent"`
	DiskReadBytesPerSec  float64   `json:"diskReadBytesPerSec"`
	DiskWriteBytesPerSec float64   `json:"diskWriteBytesPerSec"`
	NetRecvBytesPerSec   float64   `json:"netRecvBytesPerSec"`
	NetSentBytesPerSec   float64   `json:"netSentBytesPerSec"`
}
