package onvif

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/mickeyzzc/onvif-go/v2/media"
	ov "github.com/mickeyzzc/onvif-go/v2/onvif"
)

// LibraryCamera is the mickeyzzc/onvif-go client.
// Log receives one line per discovery probe: interface, bound address, UDP replies.
type LibraryCamera struct {
	Log         logger.Writer
	ifaceOnce   sync.Once
	windowsHint sync.Once
}

type libSub struct {
	stream *ov.EventStream
}

func (s libSub) Done() <-chan struct{} { return s.stream.Done() }

func (s libSub) Unsubscribe(ctx context.Context) error {
	return s.stream.Unsubscribe(ctx)
}

// Discover probes the local network and logs the socket it actually used.
func (c *LibraryCamera) Discover(ctx context.Context, timeout time.Duration, iface string) ([]Found, error) {
	return c.probeDiscover(ctx, timeout, iface)
}

// Probe reads identity, profiles and stream URIs.
func (LibraryCamera) Probe(ctx context.Context, address, user, pass string) (ProbeResult, error) {
	client, err := ov.NewClient(address,
		ov.WithCredentials(user, pass),
		ov.WithAutoClockSkew(),
		ov.WithHTTPClient(onvifHTTPClient()),
	)
	if err != nil {
		return ProbeResult{}, err
	}
	if err = client.Initialize(ctx); err != nil {
		return ProbeResult{}, err
	}

	out := ProbeResult{Endpoint: address}
	if info, err := client.Device().GetDeviceInformation(ctx); err == nil && info != nil {
		out.Manufacturer = info.Manufacturer
		out.Model = info.Model
		out.Firmware = info.FirmwareVersion
		out.Serial = info.SerialNumber
	}

	profiles, err := client.Media().GetProfiles(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	mainToken := ov.SelectMainProfile(profiles)
	subToken := ov.SelectSubProfile(profiles, mainToken)
	out.Profiles = make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		if p == nil {
			continue
		}
		item := profileFrom(p, mainToken, subToken)
		if uri, err := client.Media().GetStreamURI(ctx, p.Token); err == nil && uri != nil {
			item.StreamURI = uri.URI
		}
		out.Profiles = append(out.Profiles, item)
	}

	_, err = client.Events().GetEventServiceCapabilities(ctx)
	out.Events = err == nil
	out.Topics = []string{}
	if topics, err := eventTopics(ctx, client); err == nil {
		out.Topics = topics
	}
	return out, nil
}

// Subscribe starts a managed PullPoint. A camera without an event service
// returns an error matching ov.ErrEventsNotSupported.
func (LibraryCamera) Subscribe(
	ctx context.Context,
	address, user, pass, filter string,
	opts SubscribeOpts,
	handler func(Message),
) (Subscription, error) {
	client, err := ov.NewClient(address,
		ov.WithCredentials(user, pass),
		ov.WithAutoClockSkew(),
		ov.WithHTTPClient(onvifHTTPClient()),
	)
	if err != nil {
		return nil, err
	}
	if err = client.Initialize(ctx); err != nil {
		return nil, err
	}
	stream, err := client.Events().SubscribeEvents(ctx, func(msg ov.NotificationMessage) {
		handler(messageFrom(msg))
	}, &ov.SubscribeEventsOptions{
		Filter:               filter,
		SubscriptionDuration: opts.Duration,
		RenewMargin:          opts.RenewMargin,
		PullTimeout:          opts.PullTimeout,
		MessageLimit:         opts.MessageLimit,
	})
	if err != nil {
		return nil, &callError{Endpoint: clientEventEndpoint(client), Err: err}
	}
	return libSub{stream: stream}, nil
}

func profileFrom(p *media.Profile, mainToken, subToken string) Profile {
	item := Profile{Token: p.Token, Name: p.Name}
	switch p.Token {
	case mainToken:
		item.Role = "main"
	case subToken:
		item.Role = "sub"
	}
	if p.VideoEncoderConfiguration != nil && p.VideoEncoderConfiguration.Resolution != nil {
		item.Width = p.VideoEncoderConfiguration.Resolution.Width
		item.Height = p.VideoEncoderConfiguration.Resolution.Height
	}
	return item
}

func messageFrom(msg ov.NotificationMessage) Message {
	out := Message{
		Topic:     msg.Topic,
		Operation: msg.Message.PropertyOperation,
		Camera:    msg.Message.UtcTime,
		Producer:  msg.ProducerAddress,
		Source:    pairs(msg.Message.Source),
		Key:       pairs(msg.Message.Key),
		Data:      pairs(msg.Message.Data),
	}
	return out
}

func pairs(in []ov.SimpleItem) []Pair {
	if len(in) == 0 {
		return nil
	}
	out := make([]Pair, len(in))
	for i, it := range in {
		out[i] = Pair{Name: it.Name, Value: it.Value}
	}
	return out
}

// eventsServiceMissing is the library sentinel for a device with no events service.
func eventsServiceMissing(err error) bool {
	return errors.Is(err, ov.ErrEventsNotSupported)
}

// EventsUnsupported reports a camera that will not serve a pull-point.
// HTTP 500 faults are not classified by the library, which only inspects a
// parsed SOAP fault on status 200.
func EventsUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ov.ErrEventsNotSupported) {
		return true
	}
	text := strings.ToLower(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(soapDetail(err)))
	return strings.Contains(text, "actionnotsupported") ||
		strings.Contains(text, "notimplemented")
}

// clientEventEndpoint reads the events XAddr the library resolved. An empty
// XAddr means CreatePullPointSubscription is posted to the device service.
func clientEventEndpoint(client *ov.Client) string {
	if client == nil {
		return ""
	}
	v := reflect.ValueOf(client)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ""
	}
	v = v.Elem()
	events := unexportedString(v, "eventEndpoint")
	if events != "" {
		return events
	}
	device := unexportedString(v, "endpoint")
	if device == "" {
		return ""
	}
	return device + " (no events XAddr)"
}

func unexportedString(v reflect.Value, name string) string {
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}
