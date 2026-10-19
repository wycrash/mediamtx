package onvif

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlattenTopicSet(t *testing.T) {
	raw := []byte(`
<tns1:RuleEngine xmlns:tns1="http://www.onvif.org/ver10/topics" xmlns:wstop="http://docs.oasis-open.org/wsn/t-1" xmlns:tt="http://www.onvif.org/ver10/schema">
  <CellMotionDetector wstop:topic="true">
    <Motion wstop:topic="true">
      <tt:MessageDescription IsProperty="true">
        <tt:Source><tt:SimpleItemDescription Name="VideoSourceConfigurationToken" Type="tt:ReferenceToken"/></tt:Source>
      </tt:MessageDescription>
    </Motion>
  </CellMotionDetector>
</tns1:RuleEngine>
<tns1:VideoSource wstop:topic="false">
  <MotionAlarm wstop:topic="true"/>
</tns1:VideoSource>`)
	require.Equal(t, []string{
		"tns1:RuleEngine/CellMotionDetector",
		"tns1:RuleEngine/CellMotionDetector/Motion",
		"tns1:VideoSource/MotionAlarm",
	}, flattenTopicSet(raw))
}
