// Package webui embeds private frontend assets copied at go generate.
//
// dvrplayer is served by compatapi at /{path}/embed.html and /lib/dvrplayer/.
// admin is the Vite dist of mediamtx-admin, served by the control API at /admin/.
package webui

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:generate go run ./downloader

//go:embed all:dvrplayer
var dvrPlayer embed.FS

//go:embed all:admin
var admin embed.FS

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

var (
	dvrPlayerFS = mustSub(dvrPlayer, "dvrplayer")
	adminFS     = mustSub(admin, "admin")
)

// DvrPlayer returns the generated DVR player files (embed.html, js/, css/, vendor/).
func DvrPlayer() fs.FS {
	return dvrPlayerFS
}

// Admin returns the generated mediamtx-admin dist (index.html, assets/, lib/).
func Admin() fs.FS {
	return adminFS
}

var extraMIME = map[string]string{
	".css":  "text/css; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".ico":  "image/x-icon",
	".js":   "application/javascript; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".map":  "application/json",
	".svg":  "image/svg+xml",
	".woff": "font/woff",
}

var assetExt = map[string]struct{}{
	".css": {}, ".gif": {}, ".html": {}, ".ico": {}, ".jpeg": {}, ".jpg": {},
	".js": {}, ".json": {}, ".map": {}, ".mjs": {}, ".png": {}, ".svg": {},
	".txt": {}, ".webp": {}, ".woff": {}, ".woff2": {},
}

// SafeRel cleans a URL path relative to an asset root and rejects traversal / dotfiles.
func SafeRel(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	if name == "" {
		return "", false
	}
	cleaned := path.Clean("/root/" + name)
	rel, ok := strings.CutPrefix(cleaned, "/root/")
	if !ok || rel == "" || rel == "." {
		return "", false
	}
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/../") {
		return "", false
	}
	base := path.Base(rel)
	if base == "." || strings.HasPrefix(base, ".") {
		return "", false
	}
	return rel, true
}

// ReadFile reads a file from fsys after SafeRel.
func ReadFile(fsys fs.FS, name string) ([]byte, error) {
	rel, ok := SafeRel(name)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return fs.ReadFile(fsys, rel)
}

// ServeFile writes a generated asset. Returns false when the file is missing.
func ServeFile(ctx *gin.Context, fsys fs.FS, name string, cacheControl string) bool {
	rel, ok := SafeRel(name)
	if !ok {
		ctx.AbortWithStatus(http.StatusNotFound)
		return false
	}

	st, err := fs.Stat(fsys, rel)
	if err != nil || st.IsDir() {
		ctx.AbortWithStatus(http.StatusNotFound)
		return false
	}

	if cacheControl != "" {
		ctx.Header("Cache-Control", cacheControl)
	}
	if ct := contentType(rel); ct != "" {
		ctx.Header("Content-Type", ct)
	}

	f, err := fsys.Open(rel)
	if err != nil {
		ctx.AbortWithStatus(http.StatusNotFound)
		return false
	}
	defer f.Close()

	reader, ok := f.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(f)
		if err != nil {
			ctx.AbortWithStatus(http.StatusInternalServerError)
			return false
		}
		reader = bytes.NewReader(data)
	}

	http.ServeContent(ctx.Writer, ctx.Request, path.Base(rel), modTime(st), reader)
	return true
}

func modTime(st fs.FileInfo) time.Time {
	if st == nil {
		return time.Time{}
	}
	return st.ModTime()
}

func exists(fsys fs.FS, name string) bool {
	rel, ok := SafeRel(name)
	if !ok {
		return false
	}
	st, err := fs.Stat(fsys, rel)
	return err == nil && !st.IsDir()
}

func cacheControl(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".json":
		return "no-cache"
	default:
		return "max-age=3600"
	}
}

func isAssetPath(name string) bool {
	_, ok := assetExt[strings.ToLower(path.Ext(name))]
	return ok
}

// ServeSPA serves a generated SPA. Missing non-asset paths fall back to index.html.
func ServeSPA(ctx *gin.Context, fsys fs.FS, rel string) {
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.HasSuffix(rel, "/") {
		ServeFile(ctx, fsys, "index.html", "no-cache")
		return
	}
	if exists(fsys, rel) {
		ServeFile(ctx, fsys, rel, cacheControl(rel))
		return
	}
	if isAssetPath(rel) {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	ServeFile(ctx, fsys, "index.html", "no-cache")
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := extraMIME[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext)
}
