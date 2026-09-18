package recordcleaner

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

// Re-probe filesystem usage every N deletes instead of after each remove.
// Avoids hundreds of disk.Usage syscalls thrashing the same volume recorders write to.
const usageRecheckEveryN = 20

func (c *Cleaner) hasSpaceClean() bool {
	for _, st := range c.Storages {
		if st != nil && st.DeleteUntilPercent != nil && *st.DeleteUntilPercent > 0 {
			return true
		}
	}
	return false
}

func (c *Cleaner) diskUsage(path string) (diskUsageInfo, error) {
	if c.DiskUsage != nil {
		return c.DiskUsage(path)
	}
	if c.DiskUsedPercent != nil {
		pct, err := c.DiskUsedPercent(path)
		return diskUsageInfo{usedPercent: pct}, err
	}
	return defaultDiskUsage(path)
}

func (c *Cleaner) diskUsedPercent(path string) (float64, error) {
	u, err := c.diskUsage(path)
	return u.usedPercent, err
}

func (c *Cleaner) isActiveSegment(fpath string) bool {
	if c.IsActiveSegment == nil {
		return false
	}
	return c.IsActiveSegment(fpath)
}

func reclaimDiskKey(diskRoot string) string {
	abs, err := filepath.Abs(diskRoot)
	if err != nil {
		return filepath.Clean(diskRoot)
	}
	return abs
}

func (c *Cleaner) setReclaiming(diskRoot string, on bool) {
	if c.reclaiming == nil {
		if !on {
			return
		}
		c.reclaiming = make(map[string]bool)
	}
	key := reclaimDiskKey(diskRoot)
	if on {
		c.reclaiming[key] = true
		return
	}
	delete(c.reclaiming, key)
}

func (c *Cleaner) isReclaiming(diskRoot string) bool {
	if c.reclaiming == nil {
		return false
	}
	return c.reclaiming[reclaimDiskKey(diskRoot)]
}

// freeStorageSpace deletes oldest segments on disks under reclaim:
// enter when used >= maxUsedPercent, keep deleting until used <= deleteUntilPercent
// (even if usage dips below max mid-way). Returns whether any disk is still
// above deleteUntilPercent while reclaiming.
func (c *Cleaner) freeStorageSpace() (stillOver bool) {
	deletedAny := false
	for name, st := range c.Storages {
		if st == nil || st.DeleteUntilPercent == nil || *st.DeleteUntilPercent <= 0 {
			continue
		}
		if st.MaxUsedPercent == nil || *st.MaxUsedPercent <= 0 {
			continue
		}
		maxPct := *st.MaxUsedPercent
		untilPct := *st.DeleteUntilPercent
		for _, diskRoot := range st.Disks {
			deleted, over := c.freeDisk(name, diskRoot, maxPct, untilPct)
			if deleted {
				deletedAny = true
			}
			if over {
				stillOver = true
			}
		}
	}
	if deletedAny && c.OnSpaceFreed != nil {
		c.OnSpaceFreed()
	}
	return stillOver
}

func (c *Cleaner) usageRecheckEvery() int {
	if c.UsageRecheckEvery > 0 {
		return c.UsageRecheckEvery
	}
	return usageRecheckEveryN
}

func (c *Cleaner) freeDisk(storageName, diskRoot string, maxPct, untilPct float64) (deletedAny bool, stillOver bool) {
	usage, err := c.diskUsage(diskRoot)
	if err != nil {
		return false, false
	}
	used := usage.usedPercent
	if used <= untilPct {
		c.setReclaiming(diskRoot, false)
		return false, false
	}
	if used >= maxPct {
		c.setReclaiming(diskRoot, true)
	}
	if !c.isReclaiming(diskRoot) {
		// Between deleteUntil and maxUsed: idle hysteresis (wait to hit max again).
		return false, false
	}

	stats := c.storagePathStats(storageName)
	budget := deleteBudgetForDisk(used, untilPct, usage.totalBytes, stats.segSize, stats.cameras, stats.partDur)

	c.Log(logger.Info, "storage '%s' disk '%s' is %.1f%% full (max %.1f%%), deleting until %.1f%% (budget=%d cameras=%d avgMaxPartSize=%d avgPartDur=%s diskTotal=%d)",
		storageName, diskRoot, used, maxPct, untilPct, budget, stats.cameras, stats.segSize, stats.partDur, usage.totalBytes)

	recheckEvery := c.usageRecheckEvery()
	absDisk, absErr := filepath.Abs(diskRoot)
	if absErr != nil {
		absDisk = filepath.Clean(diskRoot)
	}
	segments := c.segmentsOnDisk(storageName, diskRoot, budget)
	if len(segments) == 0 {
		c.Log(logger.Warn, "storage '%s' disk '%s' is %.1f%% full but no deletable segments were found (check path storage= name and record paths)",
			storageName, diskRoot, used)
		return false, true
	}

	deleted := 0
	deletedOnDisk := 0
	skippedActive := 0
	for _, seg := range segments {
		onPressure := pathUnderRoot(seg.Fpath, absDisk)
		if onPressure && deletedOnDisk >= budget {
			if used > untilPct {
				c.Log(logger.Warn, "storage '%s' disk '%s': delete budget (%d) reached this tick (still reclaiming next pass)",
					storageName, diskRoot, budget)
			}
			break
		}
		if c.isActiveSegment(seg.Fpath) {
			skippedActive++
			continue
		}

		c.Log(logger.Debug, "removing %s (disk pressure)", seg.Fpath)
		removeErr := os.Remove(seg.Fpath)
		if removeErr != nil && !os.IsNotExist(removeErr) {
			c.Log(logger.Warn, "failed to remove %s: %v", seg.Fpath, removeErr)
			continue
		}
		if c.OnSegmentRemove != nil {
			c.OnSegmentRemove(seg.Fpath)
		}
		deleted++
		if onPressure {
			deletedOnDisk++
		}

		if !onPressure || deletedOnDisk%recheckEvery != 0 {
			continue
		}
		used, err = c.diskUsedPercent(diskRoot)
		if err != nil {
			return deleted > 0, true
		}
		if used <= untilPct {
			break
		}
	}

	usedAfter := used
	if deleted > 0 {
		if u, uerr := c.diskUsedPercent(diskRoot); uerr == nil {
			usedAfter = u
		}
		c.Log(logger.Info, "storage '%s' disk '%s': removed %d segment(s) under disk pressure (now %.1f%%, target %.1f%%, budget=%d, skippedActive=%d)",
			storageName, diskRoot, deleted, usedAfter, untilPct, budget, skippedActive)
	} else if skippedActive > 0 {
		c.Log(logger.Warn, "storage '%s' disk '%s': no segments removed (%d are active writes)",
			storageName, diskRoot, skippedActive)
	}

	stillOver = usedAfter > untilPct
	if !stillOver {
		c.setReclaiming(diskRoot, false)
	}
	return deleted > 0, stillOver
}

func (c *Cleaner) segmentsOnDisk(storageName, diskRoot string, limit int) []*recordstore.Segment {
	if limit <= 0 {
		limit = minDeletesPerDiskPerTick
	}
	// Ask for a few extra so active-write skips do not exhaust the batch early.
	ask := limit + 32
	if lister := c.getSegmentLister(); lister != nil {
		refs, ok := lister.ReclaimCandidates(storageName, diskRoot, ask)
		if ok {
			out := make([]*recordstore.Segment, 0, len(refs))
			for _, ref := range refs {
				out = append(out, &recordstore.Segment{Fpath: ref.Fpath, Start: ref.Start})
			}
			return out
		}
	}

	absDisk, err := filepath.Abs(diskRoot)
	if err != nil {
		absDisk = filepath.Clean(diskRoot)
	}

	var out []*recordstore.Segment
	seen := make(map[string]struct{})

	pathNames := recordstore.FindAllPathsWithSegments(c.PathConfs)
	for _, pathName := range pathNames {
		pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName)
		if err != nil || pathConf.Storage != storageName {
			continue
		}

		segments, err := recordstore.FindSegments(pathConf, pathName, nil, nil)
		if errors.Is(err, recordstore.ErrNoSegmentsFound) {
			continue
		}
		if err != nil {
			c.Log(logger.Warn, "storage '%s': list segments for path %s: %v", storageName, pathName, err)
			continue
		}

		for _, seg := range segments {
			absFile, err := filepath.Abs(seg.Fpath)
			if err != nil {
				absFile = filepath.Clean(seg.Fpath)
			}
			if !pathUnderRoot(absFile, absDisk) {
				continue
			}
			if _, ok := seen[absFile]; ok {
				continue
			}
			seen[absFile] = struct{}{}
			out = append(out, &recordstore.Segment{Fpath: absFile, Start: seg.Start})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Start.Before(out[j].Start)
	})
	if len(out) > ask {
		out = out[:ask]
	}
	return out
}

func pathUnderRoot(fpath, root string) bool {
	if fpath == root {
		return true
	}
	sep := string(os.PathSeparator)
	prefix := root
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(fpath, prefix)
}
