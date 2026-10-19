package eventstore

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const syncEveryDefault = 32

var syncEveryN = syncEveryDefault

// SetSyncEvery sets how many appends pass between fsyncs.
func SetSyncEvery(n int) {
	if n < 1 {
		n = 1
	}
	storeMu.Lock()
	syncEveryN = n
	storeMu.Unlock()
}

// Query selects events in [From, To).
// Desc orders by start time from newest to oldest. The zero value is oldest first.
// Limit is applied after that order, so Desc keeps the newest events.
type Query struct {
	From  time.Time
	To    time.Time
	Class string
	Limit int
	Desc  bool
}

type openFile struct {
	layout Layout
	f      *os.File
	day    string
	since  int
	last   time.Time
}

var (
	storeMu sync.Mutex
	opens   = map[string]*openFile{}
)

// Append writes one record into the day's journal.
func Append(layout Layout, rec Record) error {
	if layout.Common == "" || rec.Recv.IsZero() {
		return nil
	}
	day := dayString(rec.Recv)
	frame, err := encodeRecord(rec)
	if err != nil {
		return err
	}

	storeMu.Lock()
	defer storeMu.Unlock()

	of, err := openDay(layout, day)
	if err != nil {
		return err
	}
	if _, err = of.f.Write(frame); err != nil {
		return err
	}
	of.since++
	now := time.Now()
	if of.since >= syncEveryN || now.Sub(of.last) >= time.Second {
		if err = of.f.Sync(); err != nil {
			return err
		}
		of.since = 0
		of.last = now
	}
	return nil
}

// Query reads sealed packs and the open journal for the days that can overlap q.
func QuerySpans(layout Layout, q Query) ([]Span, error) {
	if layout.Common == "" {
		return nil, nil
	}
	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-24 * time.Hour)
	}
	if q.Limit <= 0 {
		q.Limit = 1000
	}
	if q.Limit > 10000 {
		q.Limit = 10000
	}

	fromDay := q.From.Add(-24 * time.Hour)
	var recs []Record
	for day := dayStart(fromDay); !day.After(q.To); day = day.Add(24 * time.Hour) {
		part, err := readDay(layout, dayString(day))
		if err != nil {
			return nil, err
		}
		recs = append(recs, part...)
	}
	spans := pairSpans(recs, q)
	if len(spans) > q.Limit {
		spans = spans[:q.Limit]
	}
	return spans, nil
}

// DeleteBefore removes day journals whose whole day ends at or before cutoff.
func DeleteBefore(layout Layout, cutoff time.Time) error {
	if layout.Common == "" || cutoff.IsZero() {
		return nil
	}
	storeMu.Lock()
	defer storeMu.Unlock()

	entries, err := os.ReadDir(layout.Common)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, ent := range entries {
		day := ""
		var paths []string
		if layout.DateDir && ent.IsDir() && isDay(ent.Name()) {
			day = ent.Name()
			paths = []string{layout.walPath(day), layout.packPath(day)}
		}
		if !layout.DateDir && !ent.IsDir() {
			day = dayFromFile(layout, ent.Name())
			if day != "" {
				paths = []string{filepath.Join(layout.Common, ent.Name())}
			}
		}
		if day == "" {
			continue
		}
		end, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			continue
		}
		end = end.Add(24 * time.Hour)
		if end.After(cutoff) {
			continue
		}
		for _, p := range paths {
			if of := opens[p]; of != nil {
				_ = of.f.Close()
				delete(opens, p)
			}
			_ = os.Remove(p)
		}
	}
	return nil
}

func openDay(layout Layout, day string) (*openFile, error) {
	wal := layout.walPath(day)
	if of := opens[wal]; of != nil {
		return of, nil
	}
	for path, of := range opens {
		if of.layout == layout && of.day != day {
			_ = of.f.Sync()
			_ = of.f.Close()
			delete(opens, path)
			_ = seal(layout, of.day)
		}
	}
	if err := os.MkdirAll(filepath.Dir(wal), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(wal, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Size() == 0 {
		t, _ := time.ParseInLocation("2006-01-02", day, time.Local)
		if err = writeHeader(f, t); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	of := &openFile{layout: layout, f: f, day: day, last: time.Now()}
	opens[wal] = of
	return of, nil
}

func seal(layout Layout, day string) error {
	wal := layout.walPath(day)
	pack := layout.packPath(day)
	if _, err := os.Stat(wal); err != nil {
		return nil
	}
	if _, err := os.Stat(pack); err == nil {
		return os.Remove(wal)
	}
	return os.Rename(wal, pack)
}

func readDay(layout Layout, day string) ([]Record, error) {
	var out []Record
	for _, path := range []string{layout.packPath(day), layout.walPath(day)} {
		part, err := readFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func readFile(path string) ([]Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(b) < headerSize || string(b[:4]) != fileMagic {
		return nil, nil
	}
	if binary.LittleEndian.Uint16(b[4:6]) != fileVersion {
		return nil, nil
	}
	rest := b[headerSize:]
	var out []Record
	for len(rest) >= 8 {
		n := binary.LittleEndian.Uint32(rest[4:8])
		total := 8 + int(n)
		if total < 8 || len(rest) < total {
			break
		}
		rec, err := decodeRecord(rest[:total])
		if err != nil {
			break
		}
		out = append(out, rec)
		rest = rest[total:]
	}
	return out, nil
}

type openStart struct {
	rec Record
}

func pairSpans(recs []Record, q Query) []Span {
	type key struct {
		topic  string
		source string
		match  int64
	}
	open := map[key]openStart{}
	var spans []Span
	now := time.Now()
	for _, rec := range recs {
		if q.Class != "" && className(rec.Class) != q.Class {
			continue
		}
		switch rec.Op {
		case OpPoint:
			if !inWindow(rec.Recv, q) {
				continue
			}
			spans = append(spans, Span{
				From: rec.Recv, To: rec.Recv, Point: true,
				Class: className(rec.Class), Topic: rec.Topic, Source: rec.Source,
			})
		case OpStart:
			open[key{rec.Topic, rec.Source, rec.Recv.UnixMicro()}] = openStart{rec: rec}
		case OpEnd:
			k := key{rec.Topic, rec.Source, rec.Match.UnixMicro()}
			st, ok := open[k]
			if !ok {
				continue
			}
			delete(open, k)
			spans = append(spans, clipSpan(st.rec, rec.Recv, q))
		}
	}
	for _, st := range open {
		end := now
		if !q.To.IsZero() && q.To.Before(end) {
			end = q.To
		}
		spans = append(spans, clipSpan(st.rec, end, q))
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].From.Equal(spans[j].From) {
			if spans[i].Topic == spans[j].Topic {
				return spans[i].Source < spans[j].Source
			}
			return spans[i].Topic < spans[j].Topic
		}
		if q.Desc {
			return spans[i].From.After(spans[j].From)
		}
		return spans[i].From.Before(spans[j].From)
	})
	out := spans[:0]
	for _, sp := range spans {
		if sp.From.IsZero() {
			continue
		}
		out = append(out, sp)
	}
	return out
}

func clipSpan(start Record, end time.Time, q Query) Span {
	from := start.Recv
	to := end
	if to.Before(from) {
		to = from
	}
	if !q.From.IsZero() && to.Before(q.From) {
		return Span{}
	}
	if !q.To.IsZero() && !from.Before(q.To) {
		return Span{}
	}
	if !q.From.IsZero() && from.Before(q.From) {
		from = q.From
	}
	if !q.To.IsZero() && to.After(q.To) {
		to = q.To
	}
	return Span{
		From: from, To: to,
		Class: className(start.Class), Topic: start.Topic, Source: start.Source,
	}
}

func inWindow(t time.Time, q Query) bool {
	if !q.From.IsZero() && t.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !t.Before(q.To) {
		return false
	}
	return true
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.In(time.Local).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func isDay(name string) bool {
	_, err := time.ParseInLocation("2006-01-02", name, time.Local)
	return err == nil && len(name) == 10
}

func dayFromFile(layout Layout, name string) string {
	prefix := ".mtx-events" + layout.FileTag + "."
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, prefix)
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, ".wal"), ".pack")
	if !isDay(rest) {
		return ""
	}
	return rest
}
