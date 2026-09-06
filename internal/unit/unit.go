// Package unit contains the unit definition.
package unit

import (
	"reflect"
	"time"

	"github.com/pion/rtp"
)

// Unit is an atomic unit of a stream.
type Unit struct {
	// relative time
	PTS int64

	// Decode timestamp, in the same clock as PTS.
	// Set when the source provided a PES DTS that differs from PTS (B-frames).
	DTS int64

	// HasDTS reports whether DTS is set.
	HasDTS bool

	// absolute time
	NTP time.Time

	// RTP packets
	RTPPackets []*rtp.Packet

	// codec-dependent payload
	Payload Payload
}

// NilPayload checks whether the payload is nil.
func (u Unit) NilPayload() bool {
	return u.Payload == nil || reflect.ValueOf(u.Payload).IsNil()
}

// TimingTS is the timestamp used for NTP estimation and other monotonic clocks.
// PES DTS is preferred when present (B-frames make PTS non-monotonic).
func (u *Unit) TimingTS() int64 {
	if u.HasDTS {
		return u.DTS
	}
	return u.PTS
}

// RemuxDTS returns the DTS to use when remuxing this unit.
// If the source provided a PES DTS, that value is used.
// Otherwise extract() reconstructs DTS from the bitstream.
func (u *Unit) RemuxDTS(extract func() (int64, error)) (int64, error) {
	if u.HasDTS {
		return u.DTS, nil
	}
	return extract()
}
