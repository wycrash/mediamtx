package compatapi

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/webui"
)

var (
	archivePlaylistRegexp = regexp.MustCompile(`^(.*)/(?:index|archive)-(\d+)-(\d+)(?:\.fmp4)?\.m3u8$`)
	archiveDownloadRegexp = regexp.MustCompile(`^(.*)/archive-(\d+)-(\d+)\.mp4$`)
	timeshiftAbsRegexp    = regexp.MustCompile(`^(.*)/timeshift_abs-(\d+)(?:\.fmp4)?\.m3u8$`)
	previewUnixRegexp     = regexp.MustCompile(`^(.*)/(\d{10,13})-preview\.mp4$`)
	previewRegexp         = regexp.MustCompile(
		`^(.+)/(\d{4})/(\d{2})/(\d{2})/(\d{2})/(\d{2})/(\d{2})(?:-preview)?\.mp4$`)
)

func requestPathName(rawPath string) string {
	pa := strings.TrimPrefix(rawPath, "/")
	if pa == "" {
		return ""
	}
	switch {
	case strings.HasSuffix(pa, "/info.json"):
		return strings.TrimSuffix(pa, "/info.json")
	case strings.HasSuffix(pa, "/recording_status.json"):
		return strings.TrimSuffix(pa, "/recording_status.json")
	case strings.HasSuffix(strings.TrimSuffix(pa, "/"), "/ranges.json"):
		return strings.TrimSuffix(strings.TrimSuffix(pa, "/"), "/ranges.json")
	case strings.HasSuffix(pa, "/preview.mp4"):
		return strings.TrimSuffix(pa, "/preview.mp4")
	case strings.HasSuffix(pa, "/preview.jpeg"):
		return strings.TrimSuffix(pa, "/preview.jpeg")
	case strings.HasSuffix(pa, "/preview.jpg"):
		return strings.TrimSuffix(pa, "/preview.jpg")
	case strings.HasSuffix(pa, "/embed.html"):
		return strings.TrimSuffix(pa, "/embed.html")
	case strings.HasSuffix(pa, "/authmirror"):
		return strings.TrimSuffix(pa, "/authmirror")
	}
	if m := archivePlaylistRegexp.FindStringSubmatch(pa); m != nil {
		return m[1]
	}
	if m := timeshiftAbsRegexp.FindStringSubmatch(pa); m != nil {
		return m[1]
	}
	if m := archiveDownloadRegexp.FindStringSubmatch(pa); m != nil {
		return m[1]
	}
	if m := previewRegexp.FindStringSubmatch(pa); m != nil {
		return m[1]
	}
	if m := previewUnixRegexp.FindStringSubmatch(pa); m != nil {
		return m[1]
	}
	dir := path.Dir(pa)
	if dir == "." {
		return pa
	}
	return dir
}

func requestIsHead(ctx *gin.Context) bool {
	return ctx.Request.Method == http.MethodHead
}

func (s *Server) onRequest(ctx *gin.Context) {
	if ctx.Request.Method != http.MethodGet && ctx.Request.Method != http.MethodHead {
		ctx.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}

	pa := strings.TrimPrefix(ctx.Request.URL.Path, "/")
	if pa == "" {
		ctx.String(http.StatusOK, "MediaMTX compat API")
		return
	}

	switch {
	case pa == "lib/dvrplayer" || strings.HasPrefix(pa, "lib/dvrplayer/"):
		s.onDvrPlayerAsset(ctx, strings.TrimPrefix(pa, "lib/dvrplayer/"))

	case strings.HasSuffix(pa, "/embed.html"):
		s.onEmbedHTML(ctx, strings.TrimSuffix(pa, "/embed.html"))

	case strings.HasSuffix(pa, "/authmirror"):
		s.onAuthMirror(ctx)

	case strings.HasSuffix(pa, "/info.json"):
		pathName := strings.TrimSuffix(pa, "/info.json")
		s.onInfoJSON(ctx, pathName)

	case strings.HasSuffix(pa, "/recording_status.json"):
		pathName := strings.TrimSuffix(pa, "/recording_status.json")
		s.onRecordingStatus(ctx, pathName)

	case strings.HasSuffix(strings.TrimSuffix(pa, "/"), "/ranges.json"):
		pathName := strings.TrimSuffix(strings.TrimSuffix(pa, "/"), "/ranges.json")
		s.onRangesJSON(ctx, pathName)

	case strings.HasSuffix(pa, "/preview.mp4"):
		pathName := strings.TrimSuffix(pa, "/preview.mp4")
		s.onLatestPreview(ctx, pathName, "video/mp4", "snapshot.mp4")

	case strings.HasSuffix(pa, "/preview.jpeg"):
		pathName := strings.TrimSuffix(pa, "/preview.jpeg")
		s.onLatestPreview(ctx, pathName, "video/mp4", "snapshot.mp4")

	case strings.HasSuffix(pa, "/preview.jpg"):
		pathName := strings.TrimSuffix(pa, "/preview.jpg")
		s.onLatestPreview(ctx, pathName, "video/mp4", "snapshot.mp4")

	case archivePlaylistRegexp.MatchString(pa):
		m := archivePlaylistRegexp.FindStringSubmatch(pa)
		startUnix, _ := strconv.ParseInt(m[2], 10, 64)
		durSec, _ := strconv.ParseInt(m[3], 10, 64)
		s.onArchivePlaylist(ctx, m[1], time.Unix(unixFromURL(startUnix), 0).UTC(), time.Duration(durSec)*time.Second)

	case timeshiftAbsRegexp.MatchString(pa):
		m := timeshiftAbsRegexp.FindStringSubmatch(pa)
		startUnix, _ := strconv.ParseInt(m[2], 10, 64)
		start := time.Unix(unixFromURL(startUnix), 0).UTC()
		dur := time.Now().UTC().Sub(start)
		if dur < time.Second {
			dur = time.Second
		}
		s.onArchivePlaylist(ctx, m[1], start, dur)

	case archiveDownloadRegexp.MatchString(pa):
		m := archiveDownloadRegexp.FindStringSubmatch(pa)
		startUnix, _ := strconv.ParseInt(m[2], 10, 64)
		durSec, _ := strconv.ParseInt(m[3], 10, 64)
		s.onArchiveMP4(ctx, m[1], time.Unix(unixFromURL(startUnix), 0).UTC(), time.Duration(durSec)*time.Second)

	case previewRegexp.MatchString(pa):
		m := previewRegexp.FindStringSubmatch(pa)
		y, _ := strconv.Atoi(m[2])
		mo, _ := strconv.Atoi(m[3])
		d, _ := strconv.Atoi(m[4])
		h, _ := strconv.Atoi(m[5])
		mi, _ := strconv.Atoi(m[6])
		sec, _ := strconv.Atoi(m[7])
		s.onPreview(ctx, m[1], y, mo, d, h, mi, sec)

	case previewUnixRegexp.MatchString(pa):
		m := previewUnixRegexp.FindStringSubmatch(pa)
		startUnix, _ := strconv.ParseInt(m[2], 10, 64)
		if startUnix >= 1e12 {
			startUnix /= 1000
		}
		s.onPreviewAt(ctx, m[1], time.Unix(startUnix, 0).UTC())

	case strings.HasSuffix(pa, ".ts") || strings.HasSuffix(pa, ".mp4"):
		dir, fname := path.Dir(pa), path.Base(pa)
		if dir == "." {
			s.serveLive(ctx)
			return
		}
		served, err := s.tryServeArchiveSegment(ctx, dir, fname)
		if err != nil {
			s.writeError(ctx, http.StatusInternalServerError, err)
			return
		}
		if !served {
			s.serveLive(ctx)
		}

	default:
		s.serveLive(ctx)
	}
}

func unixFromURL(v int64) int64 {
	if v >= 1e12 {
		return v / 1000
	}
	return v
}

func (s *Server) dvrPlayerFS() fs.FS {
	if s.DvrPlayer != nil {
		return s.DvrPlayer
	}
	return webui.DvrPlayer()
}

func (s *Server) onDvrPlayerAsset(ctx *gin.Context, name string) {
	webui.ServeFile(ctx, s.dvrPlayerFS(), name, "max-age=3600")
}

func (s *Server) onEmbedHTML(ctx *gin.Context, pathName string) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if _, err := s.safeFindPathConf(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	body, err := webui.ReadFile(s.dvrPlayerFS(), "embed.html")
	if err != nil {
		s.writeError(ctx, http.StatusNotFound, fmt.Errorf("dvr player is not embedded; run go generate"))
		return
	}

	ctx.Header("Cache-Control", "no-cache")
	ctx.Header("Content-Type", "text/html; charset=utf-8")
	if requestIsHead(ctx) {
		ctx.Status(http.StatusOK)
		return
	}
	ctx.Data(http.StatusOK, "text/html; charset=utf-8", body)
}

// onAuthMirror echoes HTTP Basic / Bearer from the request, like the MoQ
// read page. The DVR player is cross-origin from :8892, so it fetches this
// same-origin endpoint instead and passes the result into MediaMTXMoQReader.
func (s *Server) onAuthMirror(ctx *gin.Context) {
	authz := ctx.Request.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Basic ") {
		creds, err := base64.StdEncoding.DecodeString(authz[len("Basic "):])
		if err != nil {
			s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid basic auth header: %w", err))
			return
		}
		parts := strings.SplitN(string(creds), ":", 2)
		if len(parts) != 2 {
			s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid basic auth header: missing colon"))
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"user": parts[0], "pass": parts[1]})
		return
	}
	if strings.HasPrefix(authz, "Bearer ") {
		ctx.JSON(http.StatusOK, gin.H{"token": strings.TrimPrefix(authz, "Bearer ")})
		return
	}
	ctx.Status(http.StatusNoContent)
}

func (s *Server) onInfoJSON(ctx *gin.Context, pathName string) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	if _, err := s.safeFindPathConf(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	var path *defs.APIPath
	if s.PathManager != nil {
		if p, err := s.PathManager.APIPathsGet(pathName); err == nil {
			path = p
		}
	}

	var recTracks []*fmp4.InitTrack
	if s.Index != nil {
		recTracks = s.Index.LatestFMP4Tracks(pathName)
	}

	ranges := []RecordingRange{}
	if s.Index != nil {
		ranges = s.Index.Ranges(pathName)
	}

	ctx.Header("Cache-Control", "no-cache")
	ctx.JSON(http.StatusOK, buildInfo(pathName, path, ranges, recTracks))
}

func (s *Server) onRecordingStatus(ctx *gin.Context, pathName string) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	if _, err := s.safeFindPathConf(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	ranges := s.Index.Ranges(pathName)

	ctx.Header("Cache-Control", "no-cache")
	ctx.JSON(http.StatusOK, []RecordingStatus{{
		Stream: pathName,
		Ranges: ranges,
	}})
}

func (s *Server) onRangesJSON(ctx *gin.Context, pathName string) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	if _, err := s.safeFindPathConf(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	q := parseRangesQuery(ctx.Query)
	out := buildRangesJSON(s.Index.Ranges(pathName), q)

	ctx.Header("Cache-Control", "no-cache")
	ctx.JSON(http.StatusOK, out)
}

func (s *Server) onArchivePlaylist(
	ctx *gin.Context,
	pathName string,
	start time.Time,
	duration time.Duration,
) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	pathConf, err := s.safeFindPathConf(pathName)
	if err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	windowed := s.Index.SegmentsInWindow(pathName, start, duration)
	segDur := time.Duration(pathConf.RecordSegmentDuration)
	body := appendQueryToPlaylistURIs(
		GenerateArchiveM3U8Indexed(pathConf.RecordFormat, windowed, segDur, s.TimeOffsetMinutes, start),
		playlistAuthQuery(ctx),
	)

	ctx.Header("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
	ctx.Header("Cache-Control", "no-cache")
	ctx.String(http.StatusOK, body)
}

func (s *Server) onPreview(
	ctx *gin.Context,
	pathName string,
	year, month, day, hour, minute, second int,
) {
	ts := previewCivilTime(year, month, day, hour, minute, second, s.TimeOffsetMinutes)
	s.onPreviewAt(ctx, pathName, ts)
}

func (s *Server) onPreviewAt(ctx *gin.Context, pathName string, ts time.Time) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	if _, err := s.safeFindPathConf(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	seg, ok := s.Index.FindNearest(pathName, ts)
	if !ok {
		s.writeError(ctx, http.StatusNotFound, fmt.Errorf("no recording segment found"))
		return
	}

	s.servePreviewMP4(ctx, seg.Fpath())
}

func (s *Server) onLatestPreview(ctx *gin.Context, pathName, contentType, filename string) {
	if err := conf.IsValidPathName(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	if !s.doAuth(ctx, pathName) {
		return
	}

	if _, err := s.safeFindPathConf(pathName); err != nil {
		s.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	seg, ok := s.Index.FindLatest(pathName)
	if !ok {
		s.writeError(ctx, http.StatusNotFound, fmt.Errorf("no recording segment found"))
		return
	}

	s.writePreview(ctx, contentType, filename, seg.Fpath())
}

func (s *Server) servePreviewMP4(ctx *gin.Context, segPath string) {
	s.writePreview(ctx, "video/mp4", "snapshot.mp4", segPath)
}

func (s *Server) writePreview(ctx *gin.Context, contentType, filename, segPath string) {
	ctx.Header("Content-Type", contentType)
	ctx.Header("Content-Disposition", `inline; filename="`+filename+`"`)
	ctx.Header("Cache-Control", "no-cache")
	if requestIsHead(ctx) {
		ctx.Status(http.StatusOK)
		return
	}

	mp4, err := ExtractPreviewMP4(segPath)
	if err != nil {
		s.writeError(ctx, http.StatusInternalServerError, err)
		return
	}
	ctx.Data(http.StatusOK, contentType, mp4)
}

func previewCivilTime(year, month, day, hour, minute, second, offsetMinutes int) time.Time {
	loc := time.FixedZone("compat", offsetMinutes*60)
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, loc)
}

func (s *Server) tryServeArchiveSegment(ctx *gin.Context, pathName string, fileName string) (bool, error) {
	if err := conf.IsValidPathName(pathName); err != nil {
		return false, nil
	}
	if strings.Contains(fileName, "..") || strings.ContainsAny(fileName, `/\`) {
		return false, nil
	}

	pathConf, err := s.safeFindPathConf(pathName)
	if err != nil {
		return false, nil
	}

	switch pathConf.RecordFormat {
	case conf.RecordFormatMPEGTS:
		if !strings.HasSuffix(fileName, ".ts") {
			return false, nil
		}
	case conf.RecordFormatFMP4:
		if !strings.HasSuffix(fileName, ".mp4") {
			return false, nil
		}
	default:
		return false, nil
	}

	if s.Index == nil {
		return false, nil
	}

	fpath, ok := s.Index.FindByName(pathName, fileName)
	if !ok {
		return false, nil
	}

	if !s.doAuth(ctx, pathName) {
		return true, nil // auth already wrote response
	}

	switch pathConf.RecordFormat {
	case conf.RecordFormatMPEGTS:
		ctx.Header("Content-Type", "video/mp2t")
		ctx.Header("Cache-Control", "no-cache")
		ctx.File(fpath)
		return true, nil
	case conf.RecordFormatFMP4:
		ctx.Header("Content-Type", "video/mp4")
		ctx.Header("Cache-Control", "no-cache")
		return true, serveFMP4ArchivePart(ctx, fpath)
	default:
		return false, nil
	}
}

// serveFMP4ArchivePart serves a recording fMP4 as a whole resource.
// hls=init / hls=media split the file so VLC does not need EXT-X-BYTERANGE.
// hls=media&sn=&td= rewrites mfhd/tfdt into a continuous timeline for VLC seek.
// Tracks Chrome MSE cannot play (LPCM/ipcm) are stripped from HLS parts.
func serveFMP4ArchivePart(ctx *gin.Context, fpath string) error {
	part := ctx.Query("hls")
	switch part {
	case "", "full":
		ctx.File(fpath)
		return nil
	case "init", "media":
	default:
		ctx.AbortWithStatus(http.StatusBadRequest)
		return nil
	}

	initSize, err := fmp4InitSize(fpath)
	if err != nil {
		return err
	}

	f, err := os.Open(fpath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if size <= initSize {
		ctx.AbortWithStatus(http.StatusNotFound)
		return nil
	}

	name := path.Base(fpath)
	_, _, init, initErr := readFMP4Init(fpath)
	var drop map[uint32]struct{}
	if initErr == nil {
		drop = hlsDropTrackIDs(init)
	}

	if part == "init" {
		name = strings.TrimSuffix(name, ".mp4") + "_init.mp4"
		initBytes, err2 := marshalFMP4InitForHLS(init, drop)
		if err2 != nil {
			return err2
		}
		if initBytes != nil {
			http.ServeContent(ctx.Writer, ctx.Request, name, fi.ModTime(), bytes.NewReader(initBytes))
			return nil
		}
		section := io.NewSectionReader(f, 0, initSize)
		http.ServeContent(ctx.Writer, ctx.Request, name, fi.ModTime(), section)
		return nil
	}

	mediaLen := size - initSize
	snStr := ctx.Query("sn")
	tdStr := ctx.Query("td")
	var startSeq uint64
	var tdMs int64
	if snStr != "" {
		startSeq, err = strconv.ParseUint(snStr, 10, 32)
		if err != nil {
			ctx.AbortWithStatus(http.StatusBadRequest)
			return nil
		}
	}
	if tdStr != "" {
		tdMs, err = strconv.ParseInt(tdStr, 10, 64)
		if err != nil || tdMs < 0 {
			ctx.AbortWithStatus(http.StatusBadRequest)
			return nil
		}
	}

	name = strings.TrimSuffix(name, ".mp4") + "_media.mp4"
	needRewrite := len(drop) > 0 || startSeq != 0 || tdMs != 0
	if !needRewrite {
		section := io.NewSectionReader(f, initSize, mediaLen)
		http.ServeContent(ctx.Writer, ctx.Request, name, fi.ModTime(), section)
		return nil
	}

	media := make([]byte, mediaLen)
	if _, err = f.ReadAt(media, initSize); err != nil {
		return err
	}

	var trackOffsets map[uint32]uint64
	if tdMs > 0 && init != nil {
		trackOffsets = trackDecodeOffsets(init.Tracks, time.Duration(tdMs)*time.Millisecond)
	}
	if len(drop) > 0 {
		media, err = rewriteFMP4MediaForHLS(media, drop, uint32(startSeq), trackOffsets)
	} else {
		err = patchFMP4MediaTimeline(media, uint32(startSeq), trackOffsets)
	}
	if err != nil {
		return err
	}

	http.ServeContent(ctx.Writer, ctx.Request, name, fi.ModTime(), bytes.NewReader(media))
	return nil
}

func rewriteLivePlaylistPath(p string) string {
	for _, alias := range []string{"/index.fmp4.m3u8", "/video.fmp4.m3u8", "/video.m3u8"} {
		if strings.HasSuffix(p, alias) {
			return strings.TrimSuffix(p, alias) + "/index.m3u8"
		}
	}
	return p
}

func (s *Server) serveLive(ctx *gin.Context) {
	req := ctx.Request.Clone(ctx.Request.Context())

	if rewritten := rewriteLivePlaylistPath(req.URL.Path); rewritten != req.URL.Path {
		req.URL.Path = rewritten
		req.URL.RawPath = ""
	}

	if s.HLSHandler == nil {
		http.Error(ctx.Writer, "HLS backend unavailable", http.StatusBadGateway)
		return
	}

	s.Log(logger.Debug, "[conn %v] serve live %s", httpp.RemoteAddr(ctx), req.URL.Path)
	s.HLSHandler.ServeHTTP(&relativeLocationWriter{
		ResponseWriter: ctx.Writer,
		reqPath:        ctx.Request.URL.Path,
	}, req)
}
