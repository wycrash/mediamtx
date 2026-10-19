package onvif

import (
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/eventstore"
)

// Pair is one ONVIF simple item.
type Pair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Message is a notification after the SOAP envelope has been dropped.
type Message struct {
	Topic     string
	Operation string
	Camera    time.Time
	Producer  string
	Source    []Pair
	Key       []Pair
	Data      []Pair
}

type memState struct {
	active  bool
	started bool
	recv    time.Time
}

// Edge keeps property state so only transitions are stored.
type Edge struct {
	state map[string]memState
}

// Apply returns a record when the message is a new edge or a point.
func (e *Edge) Apply(msg Message, recv time.Time) (eventstore.Record, bool) {
	if e.state == nil {
		e.state = map[string]memState{}
	}
	topic := msg.Topic
	source := sourceKey(msg.Source)
	key := topic + "\x00" + source
	op := strings.ToLower(msg.Operation)
	active, isProperty := propertyState(msg.Data)

	rec := eventstore.Record{
		Recv:   recv,
		Camera: msg.Camera,
		Class:  Classify(topic),
		Topic:  topic,
		Source: source,
		Data:   items(msg.Data),
	}

	if !isProperty {
		if op == "initialized" {
			return eventstore.Record{}, false
		}
		rec.Op = eventstore.OpPoint
		return rec, true
	}

	cur := e.state[key]
	if op == "initialized" {
		e.state[key] = memState{active: active}
		return eventstore.Record{}, false
	}

	closing := op == "deleted" || !active
	if closing {
		if !cur.started {
			e.state[key] = memState{}
			return eventstore.Record{}, false
		}
		rec.Op = eventstore.OpEnd
		rec.Match = cur.recv
		e.state[key] = memState{}
		return rec, true
	}

	if cur.active {
		return eventstore.Record{}, false
	}
	rec.Op = eventstore.OpStart
	e.state[key] = memState{active: true, started: true, recv: recv}
	return rec, true
}

func sourceKey(items []Pair) string {
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if it.Value == "" {
			continue
		}
		parts = append(parts, it.Name+"="+it.Value)
	}
	return strings.Join(parts, ",")
}

func items(in []Pair) []eventstore.Item {
	if len(in) == 0 {
		return nil
	}
	out := make([]eventstore.Item, len(in))
	for i, it := range in {
		out[i] = eventstore.Item{Name: it.Name, Value: it.Value}
	}
	return out
}

func propertyState(data []Pair) (active bool, ok bool) {
	for _, it := range data {
		switch strings.ToLower(it.Name) {
		case "state", "ismotion", "logicalstate", "isinside", "active":
			v, parsed := parseBool(it.Value)
			if !parsed {
				return false, false
			}
			return v, true
		}
	}
	return false, false
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n != 0, true
		}
		return false, false
	}
}
