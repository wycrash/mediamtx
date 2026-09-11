package conf

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bluenviron/mediamtx/internal/conf/jsonwrapper"
)

const defaultStorageMaxUsedPercent = 90

// StorageStrategy is how a storage pool picks a disk for a new segment.
type StorageStrategy string

// supported values.
const (
	StorageStrategyRoundRobin StorageStrategy = "roundRobin"
	StorageStrategyFillFirst  StorageStrategy = "fillFirst"
)

// UnmarshalJSON implements json.Unmarshaler.
func (d *StorageStrategy) UnmarshalJSON(b []byte) error {
	type alias StorageStrategy
	if err := jsonwrapper.Unmarshal(b, (*alias)(d)); err != nil {
		return err
	}

	switch *d {
	case StorageStrategyRoundRobin, StorageStrategyFillFirst:

	default:
		return fmt.Errorf("invalid storage strategy '%s'", *d)
	}

	return nil
}

// UnmarshalEnv implements env.Unmarshaler.
func (d *StorageStrategy) UnmarshalEnv(_ string, v string) error {
	return d.UnmarshalJSON([]byte(`"` + v + `"`))
}

// Storage is a named pool of recording disks.
type Storage struct {
	// How to pick a disk for a new recording segment (roundRobin, fillFirst).
	// roundRobin alternates disks independently for each path.
	Strategy StorageStrategy `json:"strategy"`
	// Threshold for disk used space (%).
	// Without deleteUntilPercent: skip the disk when used >= this value (0 = ENOSPC only).
	// With deleteUntilPercent > 0: do not refuse writes; start deleting oldest
	// segments when used >= this value so new recordings can continue.
	MaxUsedPercent *float64 `json:"maxUsedPercent"`
	// When > 0, reclaim disk space by deleting oldest recordings until used space
	// is <= this percent. Must be lower than maxUsedPercent.
	// 0 or unset: refuse-only behavior at maxUsedPercent (no automatic deletes).
	DeleteUntilPercent *float64 `json:"deleteUntilPercent"`
	// Recording roots. Order is used by roundRobin / fillFirst.
	Disks []string `json:"disks"`
}

// HasSpaceReclaim reports whether this pool deletes oldest segments under pressure
// instead of refusing new recordings at maxUsedPercent.
func (s *Storage) HasSpaceReclaim() bool {
	if s == nil || s.MaxUsedPercent == nil || *s.MaxUsedPercent <= 0 {
		return false
	}
	return s.DeleteUntilPercent != nil && *s.DeleteUntilPercent > 0
}

func (s *Storage) validate(name string) error {
	if name == "" {
		return fmt.Errorf("storage name cannot be empty")
	}

	if s.Strategy == "" {
		s.Strategy = StorageStrategyRoundRobin
	}

	switch s.Strategy {
	case StorageStrategyRoundRobin, StorageStrategyFillFirst:
	default:
		return fmt.Errorf("invalid 'strategy' of storage '%s': '%s'", name, s.Strategy)
	}

	if s.MaxUsedPercent == nil {
		v := float64(defaultStorageMaxUsedPercent)
		s.MaxUsedPercent = &v
	}
	if *s.MaxUsedPercent < 0 || *s.MaxUsedPercent > 100 {
		return fmt.Errorf("'maxUsedPercent' of storage '%s' must be between 0 and 100", name)
	}

	if s.DeleteUntilPercent != nil {
		v := *s.DeleteUntilPercent
		if v < 0 || v > 100 {
			return fmt.Errorf("'deleteUntilPercent' of storage '%s' must be between 0 and 100", name)
		}
		if v > 0 {
			if *s.MaxUsedPercent <= 0 {
				return fmt.Errorf("'deleteUntilPercent' of storage '%s' requires 'maxUsedPercent' > 0", name)
			}
			if v >= *s.MaxUsedPercent {
				return fmt.Errorf("'deleteUntilPercent' of storage '%s' must be lower than 'maxUsedPercent'", name)
			}
		}
	}

	if len(s.Disks) == 0 {
		return fmt.Errorf("storage '%s' must contain at least one disk", name)
	}

	seen := make(map[string]struct{}, len(s.Disks))
	for i, d := range s.Disks {
		d = strings.TrimSpace(d)
		if d == "" {
			return fmt.Errorf("storage '%s' disk %d is empty", name, i)
		}
		s.Disks[i] = d
		key := filepath.Clean(d)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("storage '%s' has duplicate disk '%s'", name, d)
		}
		seen[key] = struct{}{}
	}

	return nil
}

// RecordPathRel returns the template suffix after the static directory prefix of recordPath.
func RecordPathRel(recordPath string) string {
	idx := strings.IndexByte(recordPath, '%')
	if idx < 0 {
		return filepath.Base(recordPath)
	}
	prefix := strings.TrimRight(recordPath[:idx], `/\`)
	rel := strings.TrimLeft(recordPath[len(prefix):], `/\`)
	if rel == "" {
		return recordPath
	}
	return rel
}

// RecordPathFormats returns one or more recordPath templates (one per storage disk).
func (pconf *Path) RecordPathFormats() []string {
	if pconf == nil {
		return nil
	}
	if len(pconf.StorageDisks) == 0 {
		return []string{pconf.RecordPath}
	}
	rel := RecordPathRel(pconf.RecordPath)
	out := make([]string, len(pconf.StorageDisks))
	for i, disk := range pconf.StorageDisks {
		out[i] = filepath.Join(disk, rel)
	}
	return out
}
