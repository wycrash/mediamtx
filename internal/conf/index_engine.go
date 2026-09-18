package conf

import (
	"fmt"

	"github.com/bluenviron/mediamtx/internal/conf/jsonwrapper"
)

// IndexEngine selects how the Compat DVR index is held in RAM.
type IndexEngine string

const (
	// IndexEngineOld keeps every day journal pinned in RAM (current behavior).
	IndexEngineOld IndexEngine = "old"
	// IndexEngineNew keeps meta, ranges, and status in RAM and loads day
	// journals on demand, pinning the last CompatAPIIndexEngineNewDay days.
	IndexEngineNew IndexEngine = "new"
)

// UnmarshalJSON implements json.Unmarshaler.
func (d *IndexEngine) UnmarshalJSON(b []byte) error {
	type alias IndexEngine
	if err := jsonwrapper.Unmarshal(b, (*alias)(d)); err != nil {
		return err
	}

	switch *d {
	case "", IndexEngineOld:
		*d = IndexEngineOld
	case IndexEngineNew:
	default:
		return fmt.Errorf("invalid 'compatAPIIndexEngine': '%s'", *d)
	}

	return nil
}

// UnmarshalEnv implements env.Unmarshaler.
func (d *IndexEngine) UnmarshalEnv(_ string, v string) error {
	return d.UnmarshalJSON([]byte(`"` + v + `"`))
}
