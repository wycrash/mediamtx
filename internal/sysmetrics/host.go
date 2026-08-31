package sysmetrics

import (
	"os"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type ioSample struct {
	readBytes  uint64
	writeBytes uint64
}

type netSample struct {
	name string
	recv uint64
	sent uint64
}

type partition struct {
	mountpoint string
	device     string
}

type diskUsage struct {
	total       uint64
	used        uint64
	free        uint64
	usedPercent float64
}

type sampler interface {
	cpuPercent() (float64, error)
	processCPUPercent() (float64, error)
	cpuCores() (int, error)
	memory() (total, used, available uint64, usedPercent float64, err error)
	processRSS() (uint64, error)
	diskUsage(path string) (diskUsage, error)
	partitions() ([]partition, error)
	diskIO() (map[string]ioSample, error)
	netIO() ([]netSample, error)
}

type hostSampler struct {
	proc *process.Process
}

func newHostSampler() sampler {
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return &hostSampler{}
	}
	return &hostSampler{proc: proc}
}

func (s *hostSampler) cpuPercent() (float64, error) {
	vs, err := cpu.Percent(0, false)
	if err != nil || len(vs) == 0 {
		return 0, err
	}
	return vs[0], nil
}

func (s *hostSampler) processCPUPercent() (float64, error) {
	if s.proc == nil {
		return 0, nil
	}
	return s.proc.CPUPercent()
}

func (s *hostSampler) cpuCores() (int, error) {
	return cpu.Counts(true)
}

func (s *hostSampler) memory() (uint64, uint64, uint64, float64, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return v.Total, v.Used, v.Available, v.UsedPercent, nil
}

func (s *hostSampler) processRSS() (uint64, error) {
	if s.proc == nil {
		return 0, nil
	}
	info, err := s.proc.MemoryInfo()
	if err != nil {
		return 0, err
	}
	return info.RSS, nil
}

func (s *hostSampler) diskUsage(path string) (diskUsage, error) {
	u, err := disk.Usage(path)
	if err != nil {
		return diskUsage{}, err
	}
	return diskUsage{
		total:       u.Total,
		used:        u.Used,
		free:        u.Free,
		usedPercent: u.UsedPercent,
	}, nil
}

func (s *hostSampler) partitions() ([]partition, error) {
	parts, err := disk.Partitions(false)
	if err != nil {
		return nil, err
	}
	out := make([]partition, 0, len(parts))
	for _, p := range parts {
		out = append(out, partition{mountpoint: p.Mountpoint, device: p.Device})
	}
	return out, nil
}

func (s *hostSampler) diskIO() (map[string]ioSample, error) {
	counters, err := disk.IOCounters()
	if err != nil {
		return nil, err
	}
	out := make(map[string]ioSample, len(counters))
	for name, c := range counters {
		out[name] = ioSample{readBytes: c.ReadBytes, writeBytes: c.WriteBytes}
	}
	return out, nil
}

func (s *hostSampler) netIO() ([]netSample, error) {
	counters, err := net.IOCounters(true)
	if err != nil {
		return nil, err
	}
	out := make([]netSample, 0, len(counters))
	for _, c := range counters {
		if isLoopback(c.Name) {
			continue
		}
		out = append(out, netSample{name: c.Name, recv: c.BytesRecv, sent: c.BytesSent})
	}
	return out, nil
}

func isLoopback(name string) bool {
	n := strings.ToLower(name)
	return n == "lo" || n == "lo0" || strings.HasPrefix(n, "loopback")
}
