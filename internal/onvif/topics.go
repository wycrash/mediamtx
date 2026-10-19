package onvif

import (
	"strings"

	"github.com/bluenviron/mediamtx/internal/eventstore"
)

// DefaultTopics is the camera-side filter used when onvifEventTopics is empty.
var DefaultTopics = []string{
	"tns1:VideoSource/MotionAlarm",
	"tns1:RuleEngine/CellMotionDetector/Motion",
	"tns1:RuleEngine/MotionRegionDetector/Motion",
	"tns1:RuleEngine/LineDetector/Crossed",
	"tns1:RuleEngine/FieldDetector/ObjectsInside",
	"tns1:Device/Trigger/DigitalInput",
	"tns1:RuleEngine/TamperDetector/Tamper",
	"tns1:VideoSource/ImageTooBlurry",
	"tns1:VideoSource/GlobalSceneChange",
	"tns1:VideoSource/SignalLoss",
}

// ResolveTopics turns the config list into a PullPoint filter.
// An empty list uses DefaultTopics. "*" subscribes to every topic.
func ResolveTopics(configured []string) (filter string, allow []string, all bool) {
	if len(configured) == 1 && configured[0] == "*" {
		return "", nil, true
	}
	allow = configured
	if len(allow) == 0 {
		allow = DefaultTopics
	}
	return strings.Join(allow, "|"), allow, false
}

// TopicAllowed reports whether a notification survives the local filter.
func TopicAllowed(topic string, allow []string, all bool) bool {
	if all || len(allow) == 0 {
		return true
	}
	for _, f := range allow {
		if f == topic || strings.Contains(topic, f) {
			return true
		}
	}
	return false
}

// Classify maps an ONVIF topic onto a stored class.
func Classify(topic string) uint8 {
	switch {
	case strings.Contains(topic, "LineDetector"):
		return eventstore.ClassLine
	case strings.Contains(topic, "FieldDetector"), strings.Contains(topic, "Loitering"):
		return eventstore.ClassField
	case strings.Contains(topic, "DigitalInput"):
		return eventstore.ClassInput
	case strings.Contains(topic, "Tamper"),
		strings.Contains(topic, "ImageTooBlurry"),
		strings.Contains(topic, "GlobalSceneChange"),
		strings.Contains(topic, "SignalLoss"):
		return eventstore.ClassTamper
	case strings.Contains(topic, "Motion"):
		return eventstore.ClassMotion
	default:
		return eventstore.ClassOther
	}
}
