package onvif

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/eventstore"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
)

type settings struct {
	discoverTimeout time.Duration
	iface           string
	opts            SubscribeOpts
	reconnect       time.Duration
	queue           int
	filter          string
	allow           []string
	all             bool
}

type sessionSpec struct {
	name              string
	address           string
	user              string
	pass              string
	layout            eventstore.Layout
	filter            string
	allow             []string
	all               bool
	opts              SubscribeOpts
	reconnect         time.Duration
	queue             int
	runOnEvent        string
	runOnEventRestart bool
	rtspPort          string
}

func (a sessionSpec) same(b sessionSpec) bool {
	return a.name == b.name &&
		a.address == b.address &&
		a.user == b.user &&
		a.pass == b.pass &&
		a.layout == b.layout &&
		a.filter == b.filter &&
		a.all == b.all &&
		a.opts == b.opts &&
		a.reconnect == b.reconnect &&
		a.queue == b.queue &&
		a.runOnEvent == b.runOnEvent &&
		a.runOnEventRestart == b.runOnEventRestart &&
		a.rtspPort == b.rtspPort
}

type session struct {
	spec        sessionSpec
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	dropped     atomic.Uint64
	subscribed  atomic.Bool
	unsupported atomic.Bool
	lastError   atomic.Value
	parent      logger.Writer
	debug       *atomic.Bool
}

// DeviceStatus is one camera derived from the paths that reference it.
type DeviceStatus struct {
	Address         string   `json:"address"`
	Owner           string   `json:"owner"`
	Paths           []string `json:"paths"`
	Subscribed      bool     `json:"subscribed"`
	EventsSupported bool     `json:"eventsSupported"`
	LastError       string   `json:"lastError,omitempty"`
	Dropped         uint64   `json:"dropped"`
}

// Status is the ONVIF service summary.
type Status struct {
	Enabled     bool   `json:"enabled"`
	Sessions    int    `json:"sessions"`
	Dropped     uint64 `json:"dropped"`
	Unsupported int    `json:"unsupported"`
}

// DiscoveryResult is the scan the UI polls.
type DiscoveryResult struct {
	Running bool    `json:"running"`
	Devices []Found `json:"devices"`
	Error   string  `json:"error,omitempty"`
}

// ErrDiscoveryBusy is returned when a scan is already in progress.
var ErrDiscoveryBusy = errors.New("discovery already running")

// discoveryPoll is how often a running scan publishes newly found cameras.
var discoveryPoll = time.Second

type discoveryJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type discoveryState struct {
	running bool
	devices []Found
	err     string
	job     *discoveryJob
}

// Service owns one PullPoint per path that has onvifAddress.
type Service struct {
	Parent   logger.Writer
	Pool     *externalcmd.Pool
	Camera   Camera
	RTSPPort string

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	cfg       settings
	debug     atomic.Bool
	paths     map[string]*conf.Path
	sessions  map[string]*session
	stopWG    sync.WaitGroup
	discovery discoveryState
}

// Start begins subscriptions for the current configuration.
func (s *Service) Start(c *conf.Conf) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Camera == nil {
		s.Camera = &LibraryCamera{Log: s.Parent}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.sessions = map[string]*session{}
	s.applyLocked(c)
	s.startMissingLocked()
}

// Close stops every subscription.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sessions := s.sessions
	s.sessions = nil
	cancel := s.cancel
	job := s.discovery.job
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, sess := range sessions {
		sess.cancel()
	}
	for _, sess := range sessions {
		<-sess.done
	}
	s.stopWG.Wait()
	if job != nil {
		<-job.done
	}
}

// Reload applies a new configuration. Path-only changes keep healthy sessions.
// Sessions that must stop are cancelled here; waiting for their unsubscribe
// runs in the background so a path add or delete is not stuck on the camera.
func (s *Service) Reload(c *conf.Conf) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.applyLocked(c)
	stop := s.takeStopsLocked()
	for _, sess := range stop {
		sess.cancel()
	}
	if len(stop) > 0 {
		s.stopWG.Add(len(stop))
	}
	if !s.closed {
		s.startMissingLocked()
	}
	s.mu.Unlock()

	for _, sess := range stop {
		go func(done <-chan struct{}) {
			defer s.stopWG.Done()
			<-done
		}(sess.done)
	}
}

// Enabled reports whether the service is running.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// DiscoverStart begins a network scan. The UI polls DiscoverResult.
func (s *Service) DiscoverStart() (DiscoveryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx == nil {
		return DiscoveryResult{}, errors.New("onvif is disabled")
	}
	if s.discovery.running {
		return DiscoveryResult{}, ErrDiscoveryBusy
	}
	ctx, cancel := context.WithCancel(s.ctx)
	job := &discoveryJob{cancel: cancel, done: make(chan struct{})}
	s.discovery = discoveryState{running: true, job: job}
	go s.runDiscovery(ctx, job, s.Camera, s.cfg.discoverTimeout, s.cfg.iface)
	return s.discoverySnapshotLocked(), nil
}

// DiscoverStop cancels the scan and returns whatever has been found.
func (s *Service) DiscoverStop() DiscoveryResult {
	s.mu.Lock()
	job := s.discovery.job
	if job != nil {
		job.cancel()
	}
	s.mu.Unlock()
	if job != nil {
		<-job.done
	}
	return s.DiscoverResult()
}

// DiscoverResult is the current scan snapshot.
func (s *Service) DiscoverResult() DiscoveryResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discoverySnapshotLocked()
}

func (s *Service) discoverySnapshotLocked() DiscoveryResult {
	devices := make([]Found, len(s.discovery.devices))
	for i, dev := range s.discovery.devices {
		devices[i] = dev
		devices[i].Owner, devices[i].Paths, devices[i].RTSP = s.addedPathsLocked(dev)
	}
	if devices == nil {
		devices = []Found{}
	}
	return DiscoveryResult{
		Running: s.discovery.running,
		Devices: devices,
		Error:   s.discovery.err,
	}
}

// addedPathsLocked matches a discovered camera to configured paths.
// The stored scan is left untouched.
func (s *Service) addedPathsLocked(dev Found) (owner string, paths []string, rtsp bool) {
	keys := onvifKeys(dev.Address)
	for _, raw := range dev.XAddrs {
		for key := range onvifKeys(raw) {
			keys[key] = struct{}{}
		}
	}
	preferred := ""
	for key := range onvifKeys(dev.Address) {
		preferred = key
	}

	owners := map[string]string{}
	for name, pathConf := range s.paths {
		if pathConf == nil || pathConf.OnvifAddress == "" {
			continue
		}
		for key := range onvifKeys(pathConf.OnvifAddress) {
			if _, ok := keys[key]; ok {
				owners[key] = name
			}
		}
	}
	owner = owners[preferred]
	if owner == "" {
		for _, name := range owners {
			owner = name
			break
		}
	}
	paths = []string{}
	if owner == "" {
		return "", paths, false
	}

	paths = append(paths, owner)
	var linked []string
	for name, pathConf := range s.paths {
		if pathConf != nil && pathConf.OnvifPath == owner {
			linked = append(linked, name)
		}
	}
	sort.Strings(linked)
	paths = append(paths, linked...)
	for _, name := range paths {
		if pathConf := s.paths[name]; pathConf != nil && rtspSource(pathConf.Source) {
			rtsp = true
			break
		}
	}
	return owner, paths, rtsp
}

func onvifKeys(raw string) map[string]struct{} {
	keys := map[string]struct{}{}
	if raw == "" {
		return keys
	}
	if _, key, err := conf.ParseOnvifAddress(raw); err == nil {
		keys[key] = struct{}{}
	}
	return keys
}

func rtspSource(source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(source, "rtsp://") || strings.HasPrefix(source, "rtsps://")
}

func (s *Service) runDiscovery(ctx context.Context, job *discoveryJob, cam Camera, timeout time.Duration, iface string) {
	defer close(job.done)
	if cam == nil {
		s.finishDiscovery(job, nil, errors.New("onvif camera is not configured"))
		return
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	seen := map[string]struct{}{}
	var found []Found
	var scanErr error
	for ctx.Err() == nil {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		slice := left
		if slice > discoveryPoll {
			slice = discoveryPoll
		}
		devs, err := cam.Discover(ctx, slice, iface)
		found = mergeFound(seen, found, devs)
		s.publishDiscovery(job, found)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				scanErr = err
			}
			break
		}
	}
	s.finishDiscovery(job, found, scanErr)
}

func (s *Service) publishDiscovery(job *discoveryJob, found []Found) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discovery.job != job {
		return
	}
	s.discovery.devices = append([]Found(nil), found...)
}

func (s *Service) finishDiscovery(job *discoveryJob, found []Found, scanErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discovery.job != job {
		return
	}
	s.discovery.running = false
	s.discovery.devices = append([]Found(nil), found...)
	if s.discovery.devices == nil {
		s.discovery.devices = []Found{}
	}
	s.discovery.job = nil
	s.discovery.err = ""
	if scanErr != nil {
		s.discovery.err = scanErr.Error()
	}
}

func mergeFound(seen map[string]struct{}, found, devs []Found) []Found {
	for _, dev := range devs {
		if dev.Address == "" {
			continue
		}
		if _, ok := seen[dev.Address]; ok {
			continue
		}
		seen[dev.Address] = struct{}{}
		found = append(found, dev)
	}
	return found
}

// Probe inspects one camera. The password is not stored.
func (s *Service) Probe(ctx context.Context, address, user, pass string) (ProbeResult, error) {
	endpoint, key, err := conf.ParseOnvifAddress(address)
	if err != nil {
		return ProbeResult{}, err
	}
	s.mu.Lock()
	cam := s.Camera
	s.mu.Unlock()
	res, err := cam.Probe(ctx, endpoint, user, pass)
	if err != nil {
		return ProbeResult{}, err
	}
	res.Endpoint = endpoint
	res.Key = key
	return res, nil
}

// Devices lists owners and the paths linked to them.
func (s *Service) Devices() []DeviceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DeviceStatus
	for name, pathConf := range s.paths {
		if pathConf == nil || pathConf.OnvifAddress == "" {
			continue
		}
		item := DeviceStatus{
			Address:         pathConf.OnvifAddress,
			Owner:           name,
			EventsSupported: true,
			Paths:           []string{name},
		}
		for other, p := range s.paths {
			if p != nil && p.OnvifPath == name {
				item.Paths = append(item.Paths, other)
			}
		}
		if sess := s.sessions[name]; sess != nil {
			item.Subscribed = sess.subscribed.Load()
			item.Dropped = sess.dropped.Load()
			item.EventsSupported = !sess.unsupported.Load()
			if v := sess.lastError.Load(); v != nil {
				item.LastError = v.(string)
			}
		}
		out = append(out, item)
	}
	return out
}

// Status summarizes subscriptions.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Enabled: !s.closed, Sessions: len(s.sessions)}
	for _, sess := range s.sessions {
		st.Dropped += sess.dropped.Load()
		if sess.unsupported.Load() {
			st.Unsupported++
		}
	}
	return st
}

func (s *Service) applyLocked(c *conf.Conf) {
	filter, allow, all := ResolveTopics(c.OnvifEventTopics)
	s.cfg = settings{
		discoverTimeout: time.Duration(c.OnvifDiscoveryTimeout),
		iface:           c.OnvifDiscoveryInterface,
		opts: SubscribeOpts{
			Duration:     time.Duration(c.OnvifSubscriptionDuration),
			RenewMargin:  time.Duration(c.OnvifRenewMargin),
			PullTimeout:  time.Duration(c.OnvifPullTimeout),
			MessageLimit: c.OnvifMessageLimit,
		},
		reconnect: time.Duration(c.OnvifReconnectPause),
		queue:     c.OnvifEventQueue,
		filter:    filter,
		allow:     allow,
		all:       all,
	}
	if s.cfg.queue < 1 {
		s.cfg.queue = 1
	}
	eventstore.SetSyncEvery(c.OnvifEventSyncEvery)
	s.debug.Store(c.OnvifDebug)
	s.paths = c.Paths
	_, port, err := net.SplitHostPort(c.RTSPAddress)
	if err == nil {
		s.RTSPPort = port
	}
}

func (s *Service) wantLocked() map[string]sessionSpec {
	want := map[string]sessionSpec{}
	for name, pathConf := range s.paths {
		if pathConf == nil || !pathConf.Enabled || pathConf.OnvifAddress == "" || !pathConf.OnvifEvents {
			continue
		}
		want[name] = sessionSpec{
			name:              name,
			address:           pathConf.OnvifAddress,
			user:              pathConf.OnvifUser,
			pass:              pathConf.OnvifPass,
			layout:            eventstore.LayoutFor(pathConf),
			filter:            s.cfg.filter,
			allow:             s.cfg.allow,
			all:               s.cfg.all,
			opts:              s.cfg.opts,
			reconnect:         s.cfg.reconnect,
			queue:             s.cfg.queue,
			runOnEvent:        pathConf.RunOnEvent,
			runOnEventRestart: pathConf.RunOnEventRestart,
			rtspPort:          s.RTSPPort,
		}
	}
	return want
}

func (s *Service) takeStopsLocked() []*session {
	want := s.wantLocked()
	var stop []*session
	for name, sess := range s.sessions {
		next, ok := want[name]
		if ok && sess.spec.same(next) {
			continue
		}
		stop = append(stop, sess)
		delete(s.sessions, name)
	}
	return stop
}

func (s *Service) startMissingLocked() {
	parent := s.ctx
	if parent == nil {
		return
	}
	for name, spec := range s.wantLocked() {
		if _, ok := s.sessions[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(parent)
		sess := &session{
			spec:   spec,
			ctx:    ctx,
			cancel: cancel,
			done:   make(chan struct{}),
			debug:  &s.debug,
		}
		s.sessions[name] = sess
		go sess.run(s.Camera, s.Parent, s.Pool)
	}
}

func (s *session) run(cam Camera, parent logger.Writer, pool *externalcmd.Pool) {
	defer close(s.done)
	s.parent = parent
	var edge Edge
	for {
		if s.ctx.Err() != nil {
			return
		}
		sub, q, err := s.subscribe(cam, s.spec.filter)
		if err != nil && s.spec.filter != "" && !eventsServiceMissing(err) {
			filtered := err
			sub, q, err = s.subscribe(cam, "")
			if err == nil {
				logOnvif(parent, logger.Warn, s.spec.name, "topic filter rejected, subscribing to all topics: %s", soapDetail(filtered))
			}
		}
		if EventsUnsupported(err) {
			s.unsupported.Store(true)
			logOnvif(parent, logger.Info, s.spec.name, "pull-point subscriptions are not implemented: %s", soapDetail(err))
			return
		}
		if err != nil {
			s.setErr(err)
			logOnvif(parent, logger.Warn, s.spec.name, "%s", soapDetail(err))
			if !sleep(s.ctx, s.spec.reconnect) {
				return
			}
			continue
		}

		s.subscribed.Store(true)
		s.setErr(nil)
		logOnvif(parent, logger.Info, s.spec.name, "subscribed")
		s.pump(sub, q, &edge, parent, pool)
		s.subscribed.Store(false)
		if s.ctx.Err() != nil {
			return
		}
		if !sleep(s.ctx, s.spec.reconnect) {
			return
		}
	}
}

func (s *session) subscribe(cam Camera, wireFilter string) (Subscription, chan Message, error) {
	qSize := s.spec.queue
	if qSize < 1 {
		qSize = 1
	}
	q := make(chan Message, qSize)
	sub, err := cam.Subscribe(s.ctx, s.spec.address, s.spec.user, s.spec.pass, wireFilter, s.spec.opts, func(msg Message) {
		if s.debug != nil && s.debug.Load() {
			logOnvif(s.parent, logger.Info, s.spec.name, "packet %s", formatPacket(msg))
		}
		if !TopicAllowed(msg.Topic, s.spec.allow, s.spec.all) {
			return
		}
		select {
		case q <- msg:
		default:
			s.dropped.Add(1)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return sub, q, nil
}

func (s *session) pump(sub Subscription, q chan Message, edge *Edge, parent logger.Writer, pool *externalcmd.Pool) {
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sub.Unsubscribe(ctx)
		cancel()
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-sub.Done():
			return
		case msg := <-q:
			rec, ok := edge.Apply(msg, time.Now())
			if !ok {
				continue
			}
			if err := eventstore.Append(s.spec.layout, rec); err != nil {
				logOnvif(parent, logger.Warn, s.spec.name, "write event: %v", err)
				continue
			}
			logOnvif(parent, logger.Info, s.spec.name, "%s", eventLine(rec))
			fireHook(s.spec, pool, parent, msg, rec)
		}
	}
}

func (s *session) setErr(err error) {
	if err == nil {
		s.lastError.Store("")
		return
	}
	s.lastError.Store(soapDetail(err))
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func fireHook(spec sessionSpec, pool *externalcmd.Pool, parent logger.Writer, msg Message, rec eventstore.Record) {
	if spec.runOnEvent == "" {
		return
	}
	state := ""
	switch rec.Op {
	case eventstore.OpStart:
		state = "true"
	case eventstore.OpEnd:
		state = "false"
	}
	raw, _ := json.Marshal(rec.Data)
	hooks.OnEvent(hooks.OnEventParams{
		Logger:  parent,
		Pool:    pool,
		Cmd:     spec.runOnEvent,
		Restart: spec.runOnEventRestart,
		Env: externalcmd.Environment{
			"MTX_PATH":            spec.name,
			"RTSP_PATH":           spec.name,
			"RTSP_PORT":           spec.rtspPort,
			"MTX_EVENT_TIME":      strconv.FormatInt(rec.Recv.Unix(), 10),
			"MTX_EVENT_CLASS":     eventstoreClass(rec.Class),
			"MTX_EVENT_TOPIC":     rec.Topic,
			"MTX_EVENT_STATE":     state,
			"MTX_EVENT_OPERATION": msg.Operation,
			"MTX_EVENT_SOURCE":    rec.Source,
			"MTX_EVENT_DATA":      string(raw),
		},
	})
}

func formatPacket(msg Message) string {
	body := struct {
		Topic     string `json:"topic"`
		Operation string `json:"operation,omitempty"`
		Camera    string `json:"camera,omitempty"`
		Producer  string `json:"producer,omitempty"`
		Source    []Pair `json:"source,omitempty"`
		Key       []Pair `json:"key,omitempty"`
		Data      []Pair `json:"data,omitempty"`
	}{
		Topic:     msg.Topic,
		Operation: msg.Operation,
		Producer:  msg.Producer,
		Source:    msg.Source,
		Key:       msg.Key,
		Data:      msg.Data,
	}
	if !msg.Camera.IsZero() {
		body.Camera = msg.Camera.UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return msg.Topic
	}
	return string(raw)
}

func eventLine(rec eventstore.Record) string {
	op := "point"
	switch rec.Op {
	case eventstore.OpStart:
		op = "start"
	case eventstore.OpEnd:
		op = "end"
	}
	line := "event " + eventstoreClass(rec.Class) + " " + op + " " + rec.Topic
	if rec.Source != "" {
		line += " source=" + rec.Source
	}
	return line
}

func eventstoreClass(c uint8) string {
	switch c {
	case eventstore.ClassMotion:
		return "motion"
	case eventstore.ClassLine:
		return "line"
	case eventstore.ClassField:
		return "field"
	case eventstore.ClassInput:
		return "input"
	case eventstore.ClassTamper:
		return "tamper"
	default:
		return "other"
	}
}

func logOnvif(parent logger.Writer, level logger.Level, name, format string, args ...any) {
	if parent != nil {
		parent.Log(level, "[path "+name+"] [onvif] "+format, args...)
	}
}
