package logger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRingWrap(t *testing.T) {
	r := NewRing(3)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	r.Push(t0, Info, "a")
	r.Push(t0.Add(time.Second), Info, "b")
	r.Push(t0.Add(2*time.Second), Warn, "c")
	r.Push(t0.Add(3*time.Second), Error, "d")

	out := r.List(ListFilter{})
	require.Equal(t, []string{"b", "c", "d"}, messagesOf(out))
	require.Equal(t, []Level{Info, Warn, Error}, levelsOf(out))
}

func TestRingListFilter(t *testing.T) {
	r := NewRing(16)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	r.Push(t0, Debug, "[path cam1] debug")
	r.Push(t0, Info, "[path cam1] ready")
	r.Push(t0, Info, "[path cam10] other")
	r.Push(t0, Info, "[RTSP] [session ab] is publishing to path 'cam1'")
	r.Push(t0, Info, "[HLS] is reading from muxer 'cam1'")
	r.Push(t0, Info, "[MoQ] is reading from path cam1")
	r.Push(t0, Warn, "unrelated")
	r.Push(t0, Error, "[path cam1/sub] nested")

	t.Run("path prefix", func(t *testing.T) {
		out := r.List(ListFilter{Path: "cam1"})
		require.Equal(t, []string{
			"[path cam1] debug",
			"[path cam1] ready",
			"[RTSP] [session ab] is publishing to path 'cam1'",
			"[HLS] is reading from muxer 'cam1'",
			"[MoQ] is reading from path cam1",
		}, messagesOf(out))
	})

	t.Run("nested path", func(t *testing.T) {
		out := r.List(ListFilter{Path: "cam1/sub"})
		require.Equal(t, []string{"[path cam1/sub] nested"}, messagesOf(out))
	})
}

func TestNewRingDefaultCapacity(t *testing.T) {
	r := NewRing(0)
	require.Equal(t, DefaultRingSize, r.capacity)
}

func messagesOf(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

func levelsOf(entries []Entry) []Level {
	out := make([]Level, len(entries))
	for i, e := range entries {
		out[i] = e.Level
	}
	return out
}
