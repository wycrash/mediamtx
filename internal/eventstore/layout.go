package eventstore

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

// Layout is the directory of one path's event journal, next to its DVR index.
type Layout struct {
	Common  string
	DateDir bool
	FileTag string
}

// LayoutFor places events beside the path's recording index.
func LayoutFor(pathConf *conf.Path) Layout {
	if pathConf == nil {
		return Layout{}
	}
	tpl := pathConf.RecordPath
	if formats := pathConf.RecordPathFormats(); len(formats) > 0 {
		tpl = formats[0]
	}
	expanded := strings.ReplaceAll(tpl, "%path", pathConf.Name)
	if abs, err := filepath.Abs(expanded); err == nil {
		expanded = abs
	}
	l := Layout{
		Common:  recordstore.CommonPath(expanded),
		DateDir: recordPathHasDateDir(tpl),
	}
	base := filepath.Base(l.Common)
	if base != pathConf.Name && base != filepath.Base(pathConf.Name) {
		l.FileTag = "." + sanitizeName(pathConf.Name)
	}
	return l
}

func recordPathHasDateDir(recordPath string) bool {
	for _, part := range strings.Split(filepath.ToSlash(recordPath), "/") {
		if part == "%Y-%m-%d" {
			return true
		}
	}
	return false
}

func sanitizeName(pathName string) string {
	var b strings.Builder
	for _, r := range pathName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "path"
	}
	return b.String()
}

func (l Layout) dayPrefix(day string) string {
	name := ".mtx-events" + l.FileTag
	if l.DateDir {
		return filepath.Join(l.Common, day, name)
	}
	return filepath.Join(l.Common, name+"."+day)
}

func (l Layout) walPath(day string) string {
	return l.dayPrefix(day) + ".wal"
}

func (l Layout) packPath(day string) string {
	return l.dayPrefix(day) + ".pack"
}

func dayString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format("2006-01-02")
}
