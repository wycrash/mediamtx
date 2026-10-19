package onvif

import (
	"context"
	"strings"
	"time"
)

// Found is one camera from WS-Discovery.
type Found struct {
	Address  string   `json:"address"`
	Name     string   `json:"name,omitempty"`
	Hardware string   `json:"hardware,omitempty"`
	XAddrs   []string `json:"xaddrs,omitempty"`
	// Type is camera, nvr, display, or analytics when the discovery types say so.
	Type string `json:"type,omitempty"`
	// Owner is the path with this camera's address. Empty means it is not configured.
	// Paths is that path plus streams linked with onvifPath.
	// RTSP is true when one of those paths has an RTSP source.
	// All three are filled when the result is served, not while the scan is stored.
	Owner string   `json:"owner"`
	Paths []string `json:"paths"`
	RTSP  bool     `json:"rtsp"`
}

// deviceType maps ONVIF discovery types to a short kind.
// A recorder wins over a transmitter when a device advertises both.
func deviceType(types []string) string {
	var camera, nvr, display, analytics bool
	for _, typ := range types {
		local := typ
		if i := strings.LastIndex(typ, ":"); i >= 0 {
			local = typ[i+1:]
		}
		switch local {
		case "NetworkVideoTransmitter":
			camera = true
		case "NetworkVideoStorage", "NetworkVideoRecorder":
			nvr = true
		case "NetworkVideoDisplay":
			display = true
		case "NetworkVideoAnalytics":
			analytics = true
		}
	}
	switch {
	case nvr:
		return "nvr"
	case camera:
		return "camera"
	case display:
		return "display"
	case analytics:
		return "analytics"
	default:
		return ""
	}
}

// SubscribeOpts is the managed PullPoint tuning.
type SubscribeOpts struct {
	Duration     time.Duration
	RenewMargin  time.Duration
	PullTimeout  time.Duration
	MessageLimit int
}

// Subscription is a live PullPoint.
type Subscription interface {
	Done() <-chan struct{}
	Unsubscribe(ctx context.Context) error
}

// Camera talks to devices. Tests substitute a fake.
type Camera interface {
	Discover(ctx context.Context, timeout time.Duration, iface string) ([]Found, error)
	Probe(ctx context.Context, address, user, pass string) (ProbeResult, error)
	Subscribe(ctx context.Context, address, user, pass, filter string, opts SubscribeOpts, handler func(Message)) (Subscription, error)
}
