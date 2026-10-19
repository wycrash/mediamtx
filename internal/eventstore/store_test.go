package eventstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
)

func TestAppendQueryAndSeal(t *testing.T) {
	dir := t.TempDir()
	layout := Layout{Common: dir}
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	end := start.Add(2 * time.Second)

	require.NoError(t, Append(layout, Record{
		Recv: start, Op: OpStart, Class: ClassMotion, Topic: "MotionAlarm", Source: "vs1",
	}))
	require.NoError(t, Append(layout, Record{
		Recv: start.Add(time.Second), Op: OpPoint, Class: ClassLine, Topic: "Crossed", Source: "rule1",
		Data: []Item{{Name: "ObjectId", Value: "7"}},
	}))
	require.NoError(t, Append(layout, Record{
		Recv: end, Match: start, Op: OpEnd, Class: ClassMotion, Topic: "MotionAlarm", Source: "vs1",
	}))

	spans, err := QuerySpans(layout, Query{
		From: start.Add(-time.Minute),
		To:   end.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, spans, 2)
	require.Equal(t, "motion", spans[0].Class)
	require.Equal(t, 2*time.Second, spans[0].To.Sub(spans[0].From))
	require.Equal(t, "line", spans[1].Class)
	require.True(t, spans[1].Point)

	storeMu.Lock()
	for path, of := range opens {
		if of.layout == layout {
			_ = of.f.Sync()
			_ = of.f.Close()
			delete(opens, path)
		}
	}
	storeMu.Unlock()
	require.NoError(t, seal(layout, dayString(start)))
	_, err = os.Stat(layout.packPath(dayString(start)))
	require.NoError(t, err)

	spans, err = QuerySpans(layout, Query{From: start.Add(-time.Minute), To: end.Add(time.Minute), Class: "motion"})
	require.NoError(t, err)
	require.Len(t, spans, 1)
	require.Equal(t, "motion", spans[0].Class)
}

func TestQuerySpansDescKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	layout := Layout{Common: dir}
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)

	for i := range 3 {
		at := start.Add(time.Duration(i) * time.Second)
		require.NoError(t, Append(layout, Record{
			Recv: at, Op: OpPoint, Class: ClassLine, Topic: "Crossed", Source: "rule",
		}))
	}

	spans, err := QuerySpans(layout, Query{
		From:  start.Add(-time.Minute),
		To:    start.Add(time.Minute),
		Limit: 2,
		Desc:  true,
	})
	require.NoError(t, err)
	require.Len(t, spans, 2)
	require.True(t, spans[0].From.After(spans[1].From))
	require.Equal(t, start.Add(2*time.Second), spans[0].From)

	asc, err := QuerySpans(layout, Query{
		From: start.Add(-time.Minute),
		To:   start.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, asc, 3)
	require.Equal(t, start, asc[0].From)
}

func TestLayoutFor(t *testing.T) {
	p := &conf.Path{
		Name:       "gate",
		RecordPath: filepath.Join(t.TempDir(), "%path", "%Y-%m-%d_%H-%M-%S-%f"),
	}
	l := LayoutFor(p)
	require.False(t, l.DateDir)
	require.Contains(t, l.Common, "gate")
}
