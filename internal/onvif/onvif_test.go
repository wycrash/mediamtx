package onvif

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/eventstore"
	"github.com/bluenviron/mediamtx/internal/logger"
)

func TestEdgeMotionInterval(t *testing.T) {
	var edge Edge
	recv := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	msg := Message{
		Topic:     "tns1:VideoSource/MotionAlarm",
		Operation: "Changed",
		Data:      []Pair{{Name: "State", Value: "true"}},
	}
	rec, ok := edge.Apply(msg, recv)
	require.True(t, ok)
	require.Equal(t, eventstore.OpStart, rec.Op)
	require.Equal(t, eventstore.ClassMotion, rec.Class)

	_, ok = edge.Apply(msg, recv.Add(time.Second))
	require.False(t, ok)

	msg.Data[0].Value = "false"
	rec, ok = edge.Apply(msg, recv.Add(2*time.Second))
	require.True(t, ok)
	require.Equal(t, eventstore.OpEnd, rec.Op)
	require.Equal(t, recv, rec.Match)
}

func TestEdgeInitializedDoesNotWrite(t *testing.T) {
	var edge Edge
	_, ok := edge.Apply(Message{
		Topic:     "tns1:VideoSource/MotionAlarm",
		Operation: "Initialized",
		Data:      []Pair{{Name: "State", Value: "true"}},
	}, time.Now())
	require.False(t, ok)

	_, ok = edge.Apply(Message{
		Topic:     "tns1:VideoSource/MotionAlarm",
		Operation: "Changed",
		Data:      []Pair{{Name: "State", Value: "true"}},
	}, time.Now())
	require.False(t, ok)
}

func TestEdgePoint(t *testing.T) {
	var edge Edge
	rec, ok := edge.Apply(Message{
		Topic:     "tns1:RuleEngine/LineDetector/Crossed",
		Operation: "Changed",
		Data:      []Pair{{Name: "ObjectId", Value: "4"}},
	}, time.Now())
	require.True(t, ok)
	require.Equal(t, eventstore.OpPoint, rec.Op)
	require.Equal(t, eventstore.ClassLine, rec.Class)
}

func TestPlanAddMainAndSub(t *testing.T) {
	probe := ProbeResult{
		Endpoint: "http://192.168.1.100:8000/onvif/device_service",
		Profiles: []Profile{
			{Token: "p1", Role: "main", StreamURI: "rtsp://192.168.1.100:554/main"},
			{Token: "p2", Role: "sub", StreamURI: "rtsp://192.168.1.100:554/sub"},
		},
	}
	record := true
	events := true
	got, err := PlanAdd(AddRequest{
		Name:    "gate",
		Address: "http://192.168.1.100:8000",
		User:    "admin",
		Pass:    "secret",
		Streams: []StreamRequest{
			{Profile: "main", Record: &record, Events: &events},
			{Profile: "sub"},
		},
	}, probe, nil, "")
	require.NoError(t, err)
	require.Equal(t, "gate", got[0].Name)
	require.True(t, got[0].OnvifEvents)
	require.Contains(t, got[0].Source, "admin:")
	require.Equal(t, "gate-sub", got[1].Name)
	require.Equal(t, "gate", got[1].OnvifPath)
	require.Empty(t, got[1].OnvifAddress)
}

func TestPlanAddExplicitNameAndExistingOwner(t *testing.T) {
	probe := ProbeResult{
		Endpoint: "http://192.168.1.100:8000/onvif/device_service",
		Profiles: []Profile{
			{Token: "p2", Role: "sub", StreamURI: "rtsp://192.168.1.100/sub"},
		},
	}
	paths := map[string]*conf.Path{
		"gate": {Name: "gate", OnvifAddress: "http://192.168.1.100:8000/onvif/device_service"},
	}
	got, err := PlanAdd(AddRequest{
		Name: "preview",
		Streams: []StreamRequest{
			{Profile: "sub", Name: "gate-sub"},
		},
	}, probe, paths, "gate")
	require.NoError(t, err)
	require.Equal(t, "gate-sub", got[0].Name)
	require.Equal(t, "gate", got[0].OnvifPath)
}

func TestDeviceType(t *testing.T) {
	require.Equal(t, "camera", deviceType([]string{"tds:Device", "dn:NetworkVideoTransmitter"}))
	require.Equal(t, "camera", deviceType([]string{"dp0:NetworkVideoTransmitter"}))
	require.Equal(t, "nvr", deviceType([]string{"dn:NetworkVideoTransmitter", "dn:NetworkVideoStorage"}))
	require.Equal(t, "nvr", deviceType([]string{"NetworkVideoRecorder"}))
	require.Equal(t, "display", deviceType([]string{"dn:NetworkVideoDisplay"}))
	require.Equal(t, "analytics", deviceType([]string{"dn:NetworkVideoAnalytics"}))
	require.Empty(t, deviceType([]string{"tds:Device"}))
	require.Empty(t, deviceType(nil))
}

func TestParseOnvifAddress(t *testing.T) {
	endpoint, key, err := conf.ParseOnvifAddress("http://192.168.1.100:8000")
	require.NoError(t, err)
	require.Equal(t, "http://192.168.1.100:8000/onvif/device_service", endpoint)
	require.Equal(t, "http://192.168.1.100:8000", key)

	_, key2, err := conf.ParseOnvifAddress("http://192.168.1.100:8000/onvif/device_service")
	require.NoError(t, err)
	require.Equal(t, key, key2)
}

func TestDiscoveryStartStopResult(t *testing.T) {
	prev := discoveryPoll
	discoveryPoll = 15 * time.Millisecond
	t.Cleanup(func() { discoveryPoll = prev })

	cam := &scriptCamera{items: []Found{
		{Address: "http://10.0.0.5:80/onvif/device_service", Name: "gate"},
		{Address: "http://10.0.0.6:80/onvif/device_service", Name: "yard"},
	}}
	svc := &Service{Camera: cam}
	svc.Start(&conf.Conf{OnvifDiscoveryTimeout: conf.Duration(40 * time.Millisecond)})
	defer svc.Close()

	started, err := svc.DiscoverStart()
	require.NoError(t, err)
	require.True(t, started.Running)
	require.Empty(t, started.Devices)

	_, err = svc.DiscoverStart()
	require.ErrorIs(t, err, ErrDiscoveryBusy)

	require.Eventually(t, func() bool {
		res := svc.DiscoverResult()
		return len(res.Devices) >= 1
	}, time.Second, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		return !svc.DiscoverResult().Running
	}, time.Second, 5*time.Millisecond)

	done := svc.DiscoverResult()
	require.Equal(t, cam.items, stripAdded(done.Devices))
	require.Empty(t, done.Error)

	blocked := &scriptCamera{
		block:   true,
		entered: make(chan struct{}),
		items:   []Found{{Address: "http://10.0.0.7:80/onvif/device_service"}},
	}
	svc.Camera = blocked
	_, err = svc.DiscoverStart()
	require.NoError(t, err)
	<-blocked.entered
	stopped := svc.DiscoverStop()
	require.False(t, stopped.Running)
	require.Equal(t, blocked.items, stripAdded(stopped.Devices))
}

func TestDiscoverResultMarksAddedPaths(t *testing.T) {
	svc := &Service{
		paths: map[string]*conf.Path{
			"gate": {
				OnvifAddress: "http://10.0.0.5/onvif/device_service",
				Source:       "rtsp://user:pass@10.0.0.5/main",
			},
			"gate-sub": {
				OnvifPath: "gate",
				Source:    "publisher",
			},
			"yard": {
				OnvifAddress: "http://10.0.0.8:8000/onvif/device_service",
				Source:       "publisher",
			},
			"yard-sub": {
				OnvifPath: "yard",
				Source:    "rtsps://10.0.0.8/sub",
			},
		},
		discovery: discoveryState{devices: []Found{
			{Address: "http://10.0.0.5:80/onvif/device_service", Name: "gate cam"},
			{Address: "http://10.0.0.6:80/onvif/device_service", Name: "other"},
			{Address: "http://10.0.0.8:8000/onvif/device_service", Name: "yard cam"},
		}},
	}

	res := svc.DiscoverResult()
	require.Equal(t, "gate", res.Devices[0].Owner)
	require.Equal(t, []string{"gate", "gate-sub"}, res.Devices[0].Paths)
	require.True(t, res.Devices[0].RTSP)

	require.Empty(t, res.Devices[1].Owner)
	require.Empty(t, res.Devices[1].Paths)
	require.False(t, res.Devices[1].RTSP)

	require.Equal(t, "yard", res.Devices[2].Owner)
	require.Equal(t, []string{"yard", "yard-sub"}, res.Devices[2].Paths)
	require.True(t, res.Devices[2].RTSP)

	require.Empty(t, svc.discovery.devices[0].Owner)
	require.Nil(t, svc.discovery.devices[0].Paths)
}

func stripAdded(devices []Found) []Found {
	out := append([]Found(nil), devices...)
	for i := range out {
		out[i].Owner = ""
		out[i].Paths = nil
		out[i].RTSP = false
	}
	return out
}

type scriptCamera struct {
	mu      sync.Mutex
	items   []Found
	n       int
	block   bool
	entered chan struct{}
	once    sync.Once
}

func (c *scriptCamera) Discover(ctx context.Context, timeout time.Duration, _ string) ([]Found, error) {
	if c.entered != nil {
		c.once.Do(func() { close(c.entered) })
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if c.block {
			return append([]Found(nil), c.items...), ctx.Err()
		}
		return nil, ctx.Err()
	case <-timer.C:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n >= len(c.items) {
		return nil, nil
	}
	item := c.items[c.n]
	c.n++
	return []Found{item}, nil
}

func (c *scriptCamera) Probe(context.Context, string, string, string) (ProbeResult, error) {
	return ProbeResult{}, errors.New("not used")
}

func (c *scriptCamera) Subscribe(context.Context, string, string, string, string, SubscribeOpts, func(Message)) (Subscription, error) {
	return nil, errors.New("not used")
}

type memLog struct {
	mu    sync.Mutex
	lines []string
}

func (m *memLog) Log(_ logger.Level, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, fmt.Sprintf(format, args...))
}

func (m *memLog) joined() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.lines, "\n")
}

type holdSub struct {
	done chan struct{}
	once sync.Once
}

func (s *holdSub) Done() <-chan struct{} { return s.done }

func (s *holdSub) Unsubscribe(context.Context) error {
	s.once.Do(func() { close(s.done) })
	return nil
}

type packetCamera struct {
	msg     Message
	handled chan struct{}
}

func (c *packetCamera) Discover(context.Context, time.Duration, string) ([]Found, error) {
	return nil, nil
}

func (c *packetCamera) Probe(context.Context, string, string, string) (ProbeResult, error) {
	return ProbeResult{}, errors.New("not used")
}

func (c *packetCamera) Subscribe(ctx context.Context, _, _, _, _ string, _ SubscribeOpts, handler func(Message)) (Subscription, error) {
	handler(c.msg)
	close(c.handled)
	sub := &holdSub{done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe(context.Background())
	}()
	return sub, nil
}

func TestDebugLogsFullPacket(t *testing.T) {
	when := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	msg := Message{
		Topic:     "tns1:RuleEngine/MyDetector/Custom",
		Operation: "Changed",
		Camera:    when,
		Producer:  "http://10.0.0.5/onvif/events",
		Source:    []Pair{{Name: "VideoSource", Value: "vs1"}},
		Key:       []Pair{{Name: "ObjectId", Value: "7"}},
		Data:      []Pair{{Name: "State", Value: "true"}},
	}
	cam := &packetCamera{msg: msg, handled: make(chan struct{})}
	log := &memLog{}
	svc := &Service{Parent: log, Camera: cam}
	svc.Start(&conf.Conf{
		OnvifDebug: true,
		Paths: map[string]*conf.Path{
			"cam": {
				Enabled:      true,
				OnvifAddress: "http://10.0.0.5/onvif/device_service",
				OnvifEvents:  true,
			},
		},
	})
	defer svc.Close()

	select {
	case <-cam.handled:
	case <-time.After(2 * time.Second):
		t.Fatal("notification was not delivered")
	}

	require.Eventually(t, func() bool {
		return strings.Contains(log.joined(), "packet ")
	}, time.Second, 10*time.Millisecond)

	got := log.joined()
	require.Contains(t, got, `[path cam] [onvif] packet {"topic":"tns1:RuleEngine/MyDetector/Custom","operation":"Changed","camera":"2026-10-14T12:00:00Z","producer":"http://10.0.0.5/onvif/events","source":[{"name":"VideoSource","value":"vs1"}],"key":[{"name":"ObjectId","value":"7"}],"data":[{"name":"State","value":"true"}]}`)
}
