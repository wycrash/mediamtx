package conf

import (
	"fmt"

	"github.com/bluenviron/mediamtx/internal/conf/jsonwrapper"
)

// IndexEngine selects how the Compat DVR index is held in RAM.
type IndexEngine string

const (
	// IndexEngineInRAM keeps every day journal pinned in RAM.
	IndexEngineInRAM IndexEngine = "inram"
	// IndexEngineHybrid keeps meta, ranges, and status in RAM and loads day
	// journals on demand, pinning the last CompatAPIIndexEngineHybridDay days.
	IndexEngineHybrid IndexEngine = "hybrid"
)

// UnmarshalJSON implements json.Unmarshaler.
func (d *IndexEngine) UnmarshalJSON(b []byte) error {
	type alias IndexEngine
	if err := jsonwrapper.Unmarshal(b, (*alias)(d)); err != nil {
		return err
	}

	switch *d {
	case "", IndexEngineHybrid:
		*d = IndexEngineHybrid
	case IndexEngineInRAM:
	default:
		return fmt.Errorf("invalid 'compatAPIIndexEngine': '%s'", *d)
	}

	return nil
}

// UnmarshalEnv implements env.Unmarshaler.
func (d *IndexEngine) UnmarshalEnv(_ string, v string) error {
	return d.UnmarshalJSON([]byte(`"` + v + `"`))
}
