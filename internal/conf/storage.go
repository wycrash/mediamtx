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
	Strategy StorageStrategy `json:"strategy"`
	// Skip a disk when used space is >= this percent. 0 disables the check (ENOSPC only).
	MaxUsedPercent *float64 `json:"maxUsedPercent"`
	// Recording roots. Order is used by roundRobin / fillFirst.
	Disks []string `json:"disks"`
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
