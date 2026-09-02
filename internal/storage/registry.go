// Package storage implements named recording disk pools.
package storage

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/bluenviron/mediamtx/internal/conf"
)

// Errors returned by Registry.Pick.
var (
	ErrNotFound       = errors.New("storage not found")
	ErrNoWritableDisk = errors.New("no writable storage disk")
)

type usageFunc func(path string) (usedPercent float64, err error)

func statUsedPercent(path string) (float64, error) {
	u, err := disk.Usage(path)
	if err != nil {
		return 0, err
	}
	return u.UsedPercent, nil
}

type pool struct {
	conf *conf.Storage
	next atomic.Uint64
}

// Registry holds runtime state for named storage pools.
type Registry struct {
	mu    sync.Mutex
	pools map[string]*pool
	usage usageFunc
}

// NewRegistry builds a registry from conf.Storages.
func NewRegistry(storages map[string]*conf.Storage) *Registry {
	r := &Registry{
		usage: statUsedPercent,
	}
	r.Reload(storages)
	return r
}

// Reload replaces pool definitions. Round-robin counters of unchanged names are kept.
func (r *Registry) Reload(storages map[string]*conf.Storage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := make(map[string]*pool, len(storages))
	for name, s := range storages {
		if s == nil {
			continue
		}
		p := &pool{conf: s}
		if old, ok := r.pools[name]; ok {
			p.next.Store(old.next.Load())
		}
		next[name] = p
	}
	r.pools = next
}

// Roots returns every disk of the named pool (read and write).
func (r *Registry) Roots(name string) []string {
	if r == nil || name == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pools[name]
	if p == nil || p.conf == nil {
		return nil
	}
	return append([]string(nil), p.conf.Disks...)
}

// Pick returns a disk to create a new segment on.
// skip lists disk roots already tried in this attempt (ENOSPC).
func (r *Registry) Pick(name string, skip []string) (string, error) {
	if r == nil || name == "" {
		return "", ErrNotFound
	}

	r.mu.Lock()
	p := r.pools[name]
	usage := r.usage
	r.mu.Unlock()
	if p == nil || p.conf == nil || len(p.conf.Disks) == 0 {
		return "", ErrNotFound
	}

	skipped := make(map[string]struct{}, len(skip))
	for _, s := range skip {
		skipped[s] = struct{}{}
	}

	limit := p.conf.MaxUsedPercent
	var maxPct float64
	if limit != nil {
		maxPct = *limit
	}

	disks := p.conf.Disks
	n := len(disks)

	try := func(d string) bool {
		if _, ok := skipped[d]; ok {
			return false
		}
		if maxPct <= 0 {
			return true
		}
		pct, err := usage(d)
		if err != nil {
			// Path may not exist yet; allow Create to mkdir.
			return true
		}
		return pct < maxPct
	}

	switch p.conf.Strategy {
	case conf.StorageStrategyFillFirst:
		for _, d := range disks {
			if try(d) {
				return d, nil
			}
		}

	default: // roundRobin
		start := int(p.next.Add(1)-1) % n
		if start < 0 {
			start = 0
		}
		for i := 0; i < n; i++ {
			d := disks[(start+i)%n]
			if try(d) {
				return d, nil
			}
		}
	}

	return "", fmt.Errorf("%w '%s'", ErrNoWritableDisk, name)
}
