// Package storage implements named recording disk pools.
package storage

import (
	"errors"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"golang.org/x/sync/singleflight"

	"github.com/bluenviron/mediamtx/internal/conf"
)

// Errors returned by Registry.Pick.
var (
	ErrNotFound       = errors.New("storage not found")
	ErrNoWritableDisk = errors.New("no writable storage disk")
)

// How long a successful (under-limit) usage reading is reused.
// Avoids blocking recorders on disk.Usage for every new segment.
const usageCacheTTL = 5 * time.Second

// Full disks are rechecked sooner so cleanup can reopen them quickly.
const usageCacheFullTTL = 500 * time.Millisecond

// After a disk I/O failure, skip it this long then retry a write.
const unavailableFirstTTL = 3 * time.Minute

// If the retry still fails, skip the disk this long before trying again.
const unavailableRepeatTTL = 10 * time.Minute

type usageFunc func(path string) (usedPercent float64, err error)

func statUsedPercent(path string) (float64, error) {
	// gopsutil disk.Usage is a filesystem Statfs/GetDiskFreeSpaceEx probe —
	// it does not walk directories or sum file sizes.
	u, err := disk.Usage(path)
	if err != nil {
		return 0, err
	}
	return u.UsedPercent, nil
}

type usageCacheEntry struct {
	percent float64
	err     error
	at      time.Time
	full    bool
}

type pool struct {
	conf     *conf.Storage
	next     atomic.Uint64
	pathNext *sync.Map // pathName -> *atomic.Uint64
}

// Registry holds runtime state for named storage pools.
type Registry struct {
	mu    sync.Mutex
	pools map[string]*pool
	usage usageFunc

	usageCacheMu sync.Mutex
	usageCache   map[string]usageCacheEntry
	usageGroup   singleflight.Group
	healthMu     sync.Mutex
	health       map[string]*diskHealth
	// OnPressure is invoked when Pick sees a disk at/above maxUsedPercent while
	// space reclaim (deleteUntilPercent) is enabled — kick the record cleaner.
	OnPressure func()
	// usageTTL overrides usageCacheTTL (tests). Zero means default.
	usageTTL time.Duration
	// usageFullTTL overrides usageCacheFullTTL (tests). Zero means default.
	usageFullTTL time.Duration
	// firstUnavailableTTL overrides unavailableFirstTTL (tests). Zero means default.
	firstUnavailableTTL time.Duration
	// repeatUnavailableTTL overrides unavailableRepeatTTL (tests). Zero means default.
	repeatUnavailableTTL time.Duration
	// now overrides time.Now (tests).
	now func() time.Time
}

type diskHealth struct {
	until   time.Time
	strikes int
	err     string
}

// NewRegistry builds a registry from conf.Storages.
func NewRegistry(storages map[string]*conf.Storage) *Registry {
	r := &Registry{
		usage:      statUsedPercent,
		usageCache: make(map[string]usageCacheEntry),
		health:     make(map[string]*diskHealth),
		now:        time.Now,
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
		p := &pool{conf: s, pathNext: &sync.Map{}}
		if old, ok := r.pools[name]; ok {
			p.next.Store(old.next.Load())
			if old.pathNext != nil {
				p.pathNext = old.pathNext
			}
		}
		next[name] = p
	}
	r.pools = next

	r.usageCacheMu.Lock()
	r.usageCache = make(map[string]usageCacheEntry)
	r.usageCacheMu.Unlock()

	r.healthMu.Lock()
	r.health = make(map[string]*diskHealth)
	r.healthMu.Unlock()
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

// InvalidateUsage drops cached disk usage (e.g. after space cleanup).
func (r *Registry) InvalidateUsage() {
	if r == nil {
		return
	}
	r.usageCacheMu.Lock()
	r.usageCache = make(map[string]usageCacheEntry)
	r.usageCacheMu.Unlock()
}

func (r *Registry) cachedUsage(path string, maxPct float64) (float64, error) {
	now := r.now()
	ttl := r.usageTTL
	if ttl == 0 {
		ttl = usageCacheTTL
	}
	fullTTL := r.usageFullTTL
	if fullTTL == 0 {
		fullTTL = usageCacheFullTTL
	}

	if pct, err, ok := r.usageCacheGet(path, now, ttl, fullTTL); ok {
		return pct, err
	}

	type result struct {
		pct float64
		err error
	}
	v, err, _ := r.usageGroup.Do(path, func() (any, error) {
		// Another waiter may have filled the cache while we queued.
		if pct, err, ok := r.usageCacheGet(path, r.now(), ttl, fullTTL); ok {
			return result{pct: pct, err: err}, nil
		}
		pct, err := r.usage(path)
		entry := usageCacheEntry{
			percent: pct,
			err:     err,
			at:      r.now(),
			full:    err == nil && maxPct > 0 && pct >= maxPct,
		}
		r.usageCacheMu.Lock()
		r.usageCache[path] = entry
		r.usageCacheMu.Unlock()
		return result{pct: pct, err: err}, nil
	})
	if err != nil {
		return 0, err
	}
	res := v.(result)
	return res.pct, res.err
}

func (r *Registry) usageCacheGet(path string, now time.Time, ttl, fullTTL time.Duration) (float64, error, bool) {
	r.usageCacheMu.Lock()
	defer r.usageCacheMu.Unlock()
	e, ok := r.usageCache[path]
	if !ok {
		return 0, nil, false
	}
	limit := ttl
	if e.full {
		limit = fullTTL
	}
	if now.Sub(e.at) >= limit {
		return 0, nil, false
	}
	return e.percent, e.err, true
}

func pathRROffset(pathName string, n int) uint64 {
	if n <= 0 || pathName == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(pathName))
	return uint64(h.Sum32()) % uint64(n)
}

func (p *pool) pathCounter(pathName string) *atomic.Uint64 {
	if pathName == "" || p.pathNext == nil {
		return &p.next
	}
	if v, ok := p.pathNext.Load(pathName); ok {
		return v.(*atomic.Uint64)
	}
	c := new(atomic.Uint64)
	if actual, loaded := p.pathNext.LoadOrStore(pathName, c); loaded {
		return actual.(*atomic.Uint64)
	}
	return c
}

// Pick returns a disk to create a new segment on.
// pathName scopes roundRobin: each path alternates disks on its own cursor so
// one camera is not pinned to a single disk by other paths sharing the pool.
// Empty pathName uses the pool-wide counter.
// skip lists disk roots already tried in this attempt (ENOSPC / dead disk).
// Disks in cooldown after MarkFailed are skipped until the backoff expires.
//
// When deleteUntilPercent is set, disks are not refused at maxUsedPercent:
// that threshold only triggers background reclaim so recording can continue.
// Without deleteUntilPercent, disks at/above maxUsedPercent are skipped
// (refuse-only / ENOSPC-style behavior).
func (r *Registry) Pick(name, pathName string, skip []string) (string, error) {
	if r == nil || name == "" {
		return "", ErrNotFound
	}

	r.mu.Lock()
	p := r.pools[name]
	r.mu.Unlock()
	if p == nil || p.conf == nil || len(p.conf.Disks) == 0 {
		return "", ErrNotFound
	}

	skipped := make(map[string]struct{}, len(skip))
	for _, s := range skip {
		skipped[s] = struct{}{}
	}

	reclaim := p.conf.HasSpaceReclaim()
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
		if r.isUnavailable(d) {
			return false
		}
		if maxPct <= 0 {
			return true
		}
		pct, err := r.cachedUsage(d, maxPct)
		if err != nil {
			// Path may not exist yet; allow Create to mkdir.
			return true
		}
		if reclaim {
			// Keep writing; ask cleaner to free space down to deleteUntilPercent.
			if pct >= maxPct && r.OnPressure != nil {
				r.OnPressure()
			}
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
		c := p.pathCounter(pathName)
		start := int(c.Add(1)-1+pathRROffset(pathName, n)) % n
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

func (r *Registry) firstTTL() time.Duration {
	if r != nil && r.firstUnavailableTTL > 0 {
		return r.firstUnavailableTTL
	}
	return unavailableFirstTTL
}

func (r *Registry) repeatTTL() time.Duration {
	if r != nil && r.repeatUnavailableTTL > 0 {
		return r.repeatUnavailableTTL
	}
	return unavailableRepeatTTL
}

// MarkFailed cools down a disk that failed mkdir/create.
// First failure: skip for 3 minutes, then retry. A failure after that retry:
// skip for 10 minutes. Concurrent failures while already cooling down do not
// escalate the backoff.
func (r *Registry) MarkFailed(root string, cause error) time.Duration {
	if r == nil || root == "" {
		return 0
	}
	now := r.now()
	reason := "not writable"
	if cause != nil && cause.Error() != "" {
		reason = cause.Error()
	}
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	if r.health == nil {
		r.health = make(map[string]*diskHealth)
	}
	h := r.health[root]
	if h == nil {
		h = &diskHealth{}
		r.health[root] = h
	}
	h.err = reason
	if now.Before(h.until) {
		return h.until.Sub(now)
	}
	h.strikes++
	ttl := r.firstTTL()
	if h.strikes >= 2 {
		ttl = r.repeatTTL()
	}
	h.until = now.Add(ttl)
	return ttl
}

// Unusable reports whether path is a storage disk currently in write cooldown.
func (r *Registry) Unusable(path string) (reason string, unusable bool) {
	if r == nil || path == "" {
		return "", false
	}
	now := r.now()
	want := diskKey(path)
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	for root, h := range r.health {
		if h == nil || !now.Before(h.until) {
			continue
		}
		got := diskKey(root)
		if got == want || hasDiskPrefix(want, got) || hasDiskPrefix(got, want) {
			reason := h.err
			if reason == "" {
				reason = "not writable"
			}
			return reason, true
		}
	}
	return "", false
}

func diskKey(p string) string {
	return strings.ToLower(filepath.Clean(p))
}

func hasDiskPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(path, prefix)
}

// MarkOK clears cooldown after a successful write to root.
func (r *Registry) MarkOK(root string) {
	if r == nil || root == "" {
		return
	}
	r.healthMu.Lock()
	delete(r.health, root)
	r.healthMu.Unlock()
}

func (r *Registry) isUnavailable(root string) bool {
	if r == nil || root == "" {
		return false
	}
	now := r.now()
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	h := r.health[root]
	if h == nil {
		return false
	}
	return now.Before(h.until)
}
