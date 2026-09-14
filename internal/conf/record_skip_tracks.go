package conf

import (
	"encoding/json"
	"strings"

	"github.com/bluenviron/mediamtx/internal/conf/jsonwrapper"
	"github.com/bluenviron/mediamtx/internal/formatlabel"
)

// RecordSkipTracks is the recordSkipTracks parameter.
type RecordSkipTracks []formatlabel.Label

// UnmarshalEnv implements env.Unmarshaler.
func (d *RecordSkipTracks) UnmarshalEnv(_ string, v string) error {
	if v == "" {
		*d = RecordSkipTracks{}
		return nil
	}

	byts, _ := json.Marshal(strings.Split(v, ","))
	return jsonwrapper.Unmarshal(byts, d)
}
