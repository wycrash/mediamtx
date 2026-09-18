// Package recordcleaner contains the recording cleaner.
package recordcleaner

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

var timeNow = time.Now

// When a disk is still above deleteUntilPercent while reclaiming, retry sooner.
const spacePressureInterval = 10 * time.Second

// Cleaner removes expired recording segments from disk and, when configured,
// frees storage pools that exceed maxUsedPercent down to deleteUntilPercent.
// Age cleanup and space cleanup run sequentially in the same tick under MaintMu
// so they never race each other or a DVR index rebuild that shares the mutex.
type Cleaner struct {
	PathConfs       map[string]*conf.Path
	Storages        map[string]*conf.Storage
	Parent          logger.Writer
	OnSegmentRemove func(fpath string)
	// OnSpaceFreed is called once after a disk-pressure pass that deleted at least
	// one segment (so storage usage caches can be refreshed without per-file churn).
	OnSpaceFreed func()
	// IsActiveSegment reports segments that are currently being written.
	IsActiveSegment func(fpath string) bool
	// DiskUsedPercent overrides filesystem usage probing (tests).
	DiskUsedPercent func(path string) (float64, error)
	// DiskUsage overrides full usage probe including total bytes (tests).
	DiskUsage func(path string) (diskUsageInfo, error)
	// UsageRecheckEvery overrides how often freeDisk re-probes usage (tests).
	// Zero means default (every 10 deletes).
	UsageRecheckEvery int
	// MaintMu serializes this cleaner with DVR index reconcile/rebuild.
	MaintMu *sync.Mutex
	// segmentLister supplies deletion candidates from the DVR index (optional).
	segmentListerMu sync.RWMutex
	segmentLister   SegmentLister

	ctx       context.Context
	ctxCancel func()

	chReloadPathConfs chan map[string]*conf.Path
	chReloadStorages  chan map[string]*conf.Storage
	chKick            chan struct{}
	done              chan struct{}

	// reclaiming tracks disks that hit maxUsedPercent and are still above
	// deleteUntilPercent (hysteresis). Only touched from the cleaner goroutine.
	reclaiming map[string]bool
}

// SetSegmentLister installs or clears the index-backed segment source.
func (c *Cleaner) SetSegmentLister(l SegmentLister) {
	if c == nil {
		return
	}
	c.segmentListerMu.Lock()
	c.segmentLister = l
	c.segmentListerMu.Unlock()
}

func (c *Cleaner) getSegmentLister() SegmentLister {
	c.segmentListerMu.RLock()
	defer c.segmentListerMu.RUnlock()
	return c.segmentLister
}

// Initialize initializes a Cleaner.
func (c *Cleaner) Initialize() {
	c.ctx, c.ctxCancel = context.WithCancel(context.Background())
	c.chReloadPathConfs = make(chan map[string]*conf.Path)
	c.chReloadStorages = make(chan map[string]*conf.Storage)
	c.chKick = make(chan struct{}, 1)
	c.done = make(chan struct{})

	go c.run()
}

// Close closes the Cleaner.
func (c *Cleaner) Close() {
	c.ctxCancel()
	<-c.done
}

// Kick requests an immediate cleanup pass (e.g. when Pick refuses a full disk).
// Coalesces concurrent kicks into one pending run.
func (c *Cleaner) Kick() {
	if c == nil || c.chKick == nil {
		return
	}
	select {
	case c.chKick <- struct{}{}:
	default:
	}
}

// Log implements logger.Writer.
func (c *Cleaner) Log(level logger.Level, format string, args ...any) {
	c.Parent.Log(level, "[record cleaner]"+format, args...)
}

// ReloadPathConfs is called by core.Core.
func (c *Cleaner) ReloadPathConfs(pathConfs map[string]*conf.Path) {
	select {
	case c.chReloadPathConfs <- pathConfs:
	case <-c.ctx.Done():
	}
}

// ReloadStorages is called by core.Core.
func (c *Cleaner) ReloadStorages(storages map[string]*conf.Storage) {
	select {
	case c.chReloadStorages <- storages:
	case <-c.ctx.Done():
	}
}

func (c *Cleaner) run() {
	defer close(c.done)

	next := c.doRun()

	for {
		timer := time.NewTimer(next)
		select {
		case <-timer.C:
			next = c.doRun()

		case <-c.chKick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			c.Log(logger.Info, "disk pressure kick: running cleanup now")
			next = c.doRun()

		case cnf := <-c.chReloadPathConfs:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			c.PathConfs = cnf
			next = c.cleanInterval(false)

		case st := <-c.chReloadStorages:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			c.Storages = st
			next = c.cleanInterval(false)

		case <-c.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (c *Cleaner) cleanInterval(spacePressure bool) time.Duration {
	if spacePressure {
		return spacePressureInterval
	}

	interval := 30 * 60 * time.Second

	for _, e := range c.PathConfs {
		if e.RecordDeleteAfter != 0 &&
			interval > (time.Duration(e.RecordDeleteAfter)/2) {
			interval = time.Duration(e.RecordDeleteAfter) / 2
		}
	}

	return interval
}

func (c *Cleaner) doRun() time.Duration {
	if c.MaintMu != nil {
		c.MaintMu.Lock()
		defer c.MaintMu.Unlock()
	}

	spacePressure := false

	// Space reclaim first: age walks can take a long time and would delay
	// freeing a disk that is already refusing new segments.
	if c.hasSpaceClean() {
		spacePressure = c.freeStorageSpace()
	}

	now := timeNow()
	for _, pathName := range c.pathNamesForAge() {
		c.processPathAge(now, pathName)
	}

	if lister := c.getSegmentLister(); lister != nil {
		lister.Flush()
	}

	return c.cleanInterval(spacePressure)
}

func (c *Cleaner) pathNamesForAge() []string {
	if lister := c.getSegmentLister(); lister != nil {
		if names, ok := lister.PathNames(); ok {
			return names
		}
	}
	return recordstore.FindAllPathsWithSegments(c.PathConfs)
}

func (c *Cleaner) processPathAge(now time.Time, pathName string) {
	pathConf, _, err := conf.FindPathConf(c.PathConfs, pathName)
	if err != nil {
		return
	}

	if pathConf.RecordDeleteAfter == 0 {
		return
	}

	n, err := c.deleteExpiredSegments(now, pathName, pathConf)
	if err != nil && !errors.Is(err, recordstore.ErrNoSegmentsFound) {
		c.Log(logger.Warn, "path %s: %v", pathName, err)
		return
	}
	if n > 0 {
		c.deleteEmptyDirs(pathConf)
	}
}

func (c *Cleaner) deleteExpiredSegments(now time.Time, pathName string, pathConf *conf.Path) (int, error) {
	end := now.Add(-time.Duration(pathConf.RecordDeleteAfter))
	deleted := 0

	if lister := c.getSegmentLister(); lister != nil {
		refs, ok := lister.SegmentsBefore(pathName, end)
		if ok {
			for _, ref := range refs {
				if c.isActiveSegment(ref.Fpath) {
					continue
				}
				c.Log(logger.Debug, "removing %s", ref.Fpath)
				os.Remove(ref.Fpath) //nolint:errcheck
				if c.OnSegmentRemove != nil {
					c.OnSegmentRemove(ref.Fpath)
				}
				deleted++
			}
			return deleted, nil
		}
	}

	segments, err := recordstore.FindSegments(pathConf, pathName, nil, &end)
	if err != nil {
		return 0, err
	}

	for _, seg := range segments {
		if c.isActiveSegment(seg.Fpath) {
			continue
		}
		c.Log(logger.Debug, "removing %s", seg.Fpath)
		os.Remove(seg.Fpath) //nolint:errcheck
		if c.OnSegmentRemove != nil {
			c.OnSegmentRemove(seg.Fpath)
		}
		deleted++
	}

	return deleted, nil
}

func (c *Cleaner) deleteEmptyDirs(pathConf *conf.Path) {
	for _, raw := range pathConf.RecordPathFormats() {
		recordPath := strings.ReplaceAll(raw, "%path", pathConf.Name)
		commonPath := recordstore.CommonPath(recordPath)

		filepath.WalkDir(commonPath, func(fpath string, info fs.DirEntry, err error) error { //nolint:errcheck
			if err != nil {
				return err
			}

			if info.IsDir() {
				os.Remove(fpath) //nolint:errcheck
			}

			return nil
		})
	}
}
