package recordcleaner

import (
	"math"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// Safety bounds for one pressure pass (derived budget is preferred).
const (
	minDeletesPerDiskPerTick = 64
	maxDeletesPerDiskPerTick = 200_000
	segSizeSampleFallback    = 50 * 1024 * 1024 // matches path default recordMaxPartSize
)

type diskUsageInfo struct {
	usedPercent float64
	totalBytes  uint64
}

func defaultDiskUsage(path string) (diskUsageInfo, error) {
	// Filesystem Statfs/GetDiskFreeSpaceEx — not a directory walk / du.
	u, err := disk.Usage(path)
	if err != nil {
		return diskUsageInfo{}, err
	}
	return diskUsageInfo{usedPercent: u.UsedPercent, totalBytes: u.Total}, nil
}

func defaultDiskUsedPercent(path string) (float64, error) {
	u, err := defaultDiskUsage(path)
	if err != nil {
		return 0, err
	}
	return u.usedPercent, nil
}

// storagePathStats summarizes recording paths that write into a storage pool.
type storagePathStats struct {
	cameras    int
	segSize    uint64        // average recordMaxPartSize across cameras
	partDur    time.Duration // average recordPartDuration across cameras
}

func (c *Cleaner) storagePathStats(storageName string) storagePathStats {
	var (
		cameras     int
		sumSegSize  uint64
		sumPartDur  time.Duration
		partDurN    int
	)
	for _, pathConf := range c.PathConfs {
		if pathConf == nil || pathConf.Storage != storageName || !pathConf.Record {
			continue
		}
		cameras++
		sumSegSize += uint64(pathConf.RecordMaxPartSize)
		if pd := time.Duration(pathConf.RecordPartDuration); pd > 0 {
			sumPartDur += pd
			partDurN++
		}
	}

	st := storagePathStats{cameras: cameras}
	if cameras > 0 && sumSegSize > 0 {
		st.segSize = sumSegSize / uint64(cameras)
	} else {
		st.segSize = segSizeSampleFallback
	}
	if partDurN > 0 {
		st.partDur = sumPartDur / time.Duration(partDurN)
	} else {
		st.partDur = time.Second
	}
	if st.cameras < 1 {
		st.cameras = 1
	}
	if st.segSize == 0 {
		st.segSize = segSizeSampleFallback
	}
	return st
}

// deleteBudgetForDisk is how many oldest segments one pressure pass may remove.
//
//	bytesToFree ≈ diskTotal * (usedPercent - untilPercent) / 100
//	budget      ≈ ceil(bytesToFree / avgRecordMaxPartSize)
//
// Floored by ~one spacePressureInterval of ingest across all cameras
// (cameras * interval / avgRecordPartDuration) so cleanup stays ahead of writers.
func deleteBudgetForDisk(usedPercent, untilPercent float64, diskTotal, avgSegSize uint64, cameras int, avgPartDur time.Duration) int {
	if avgSegSize == 0 {
		avgSegSize = segSizeSampleFallback
	}
	if cameras < 1 {
		cameras = 1
	}
	if avgPartDur <= 0 {
		avgPartDur = time.Second
	}

	budget := 0
	if diskTotal > 0 && usedPercent > untilPercent {
		frac := (usedPercent - untilPercent) / 100
		bytesToFree := uint64(math.Ceil(float64(diskTotal) * frac))
		budget = int((bytesToFree + avgSegSize - 1) / avgSegSize)
	}

	ingestFloor := cameras * int((spacePressureInterval+avgPartDur-1)/avgPartDur)
	if ingestFloor < cameras {
		ingestFloor = cameras
	}
	if budget < ingestFloor {
		budget = ingestFloor
	}
	if budget < minDeletesPerDiskPerTick {
		budget = minDeletesPerDiskPerTick
	}
	if budget > maxDeletesPerDiskPerTick {
		budget = maxDeletesPerDiskPerTick
	}
	return budget
}
