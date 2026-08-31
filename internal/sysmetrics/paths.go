package sysmetrics

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

// RecordDirs returns unique recording roots from the default record path and path configs.
func RecordDirs(defaultRecordPath string, pathConfs map[string]*conf.Path) []string {
	seen := make(map[string]struct{})
	add := func(recordPath string) {
		if recordPath == "" {
			return
		}
		dir := recordRoot(recordPath)
		if dir == "" {
			return
		}
		seen[dir] = struct{}{}
	}

	add(defaultRecordPath)
	for _, pc := range pathConfs {
		if pc != nil {
			add(pc.RecordPath)
		}
	}

	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func recordRoot(recordPath string) string {
	abs, err := filepath.Abs(recordPath)
	if err != nil {
		abs = recordPath
	}

	common := recordstore.CommonPath(abs)
	if common == "" {
		common = filepath.Dir(abs)
	}
	if common == "" || common == "." {
		common = abs
	}
	return common
}

func existingAncestor(path string) string {
	cur := path
	for {
		if st, err := os.Stat(cur); err == nil && st.IsDir() {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		cur = parent
	}
}
