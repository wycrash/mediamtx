package compatapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
)

func writeFMP4Parts(t *testing.T, fpath string, n int, idrEvery int) {
	writeFMP4PartsAt(t, fpath, n, idrEvery, 0)
}

func mustLoadParts(t *testing.T, fpath string) []fmp4MediaPart {
	t.Helper()
	parts, err := loadFMP4MediaParts(fpath)
	require.NoError(t, err)
	return parts
}

func writeFMP4PartsAt(t *testing.T, fpath string, n int, idrEvery int, ptsOffset uint64) {
	t.Helper()
	f, err := os.Create(fpath)
	require.NoError(t, err)
	defer f.Close()

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 1000,
				Codec: &mcodecs.H264{
					SPS: test.FormatH264.SPS,
					PPS: test.FormatH264.PPS,
				},
			},
		},
	}
	require.NoError(t, init.Marshal(f))
	for i := 0; i < n; i++ {
		part := fmp4.Part{
			SequenceNumber: uint32(i),
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: ptsOffset + uint64(i*1000),
				Samples: []*fmp4.Sample{{
					Duration:        1000,
					IsNonSyncSample: idrEvery > 0 && i%idrEvery != 0,
					Payload:         []byte{0, 0, 0, 1, 9, 0xf0},
				}},
			}},
		}
		require.NoError(t, part.Marshal(f))
	}
}

func TestShouldSliceFMP4(t *testing.T) {
	require.False(t, shouldSliceFMP4(5*time.Second, 5*time.Second))
	require.False(t, shouldSliceFMP4(10*time.Second, 5*time.Second))
	require.False(t, shouldSliceFMP4(15*time.Second, 5*time.Second))
	require.True(t, shouldSliceFMP4(16*time.Second, 5*time.Second))
	require.False(t, shouldSliceFMP4(time.Hour, 0))
	require.True(t, shouldSliceFMP4(20*time.Second, 5*time.Second))
}

func TestGroupHLSChunksIDRAligned(t *testing.T) {
	parts := make([]fmp4MediaPart, 20)
	for i := range parts {
		parts[i] = fmp4MediaPart{
			Off:      int64(1000 + i*100),
			Len:      100,
			Duration: time.Second,
			HasIDR:   i%2 == 0,
		}
	}
	chunks := groupHLSChunks(parts, 5*time.Second)
	require.GreaterOrEqual(t, len(chunks), 3)
	off0 := parts[0].Off
	for i, ch := range chunks {
		idx := int((ch.Off - off0) / 100)
		require.GreaterOrEqual(t, idx, 0)
		require.Less(t, idx, len(parts))
		if i > 0 {
			require.True(t, parts[idx].HasIDR, "chunk %d must start on IDR", i)
		}
		if i < len(chunks)-1 {
			require.GreaterOrEqual(t, ch.Duration, 5*time.Second, "chunk %d", i)
		}
	}
}

// Parts are cut by partDuration only, so a long GOP can leave a whole file
// without a single IDR-aligned split candidate. Chunk duration must stay
// bounded anyway: one chunk spanning the file makes players stall.
func TestGroupHLSChunksBoundsDurationWithoutIDRCandidates(t *testing.T) {
	parts := make([]fmp4MediaPart, 60)
	for i := range parts {
		parts[i] = fmp4MediaPart{
			Off:      int64(1000 + i*100),
			Len:      100,
			Duration: time.Second,
			HasIDR:   i == 0,
		}
	}

	chunks := groupHLSChunks(parts, 5*time.Second)
	require.Greater(t, len(chunks), 1)

	var total time.Duration
	for i, ch := range chunks {
		require.LessOrEqual(t, ch.Duration, hlsChunkMaxOvershoot*5*time.Second, "chunk %d", i)
		total += ch.Duration
	}
	require.Equal(t, 60*time.Second, total)
}

func TestGenerateArchiveM3U8SlicesLongFMP4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.mp4")
	writeFMP4Parts(t, path, 20, 2)
	base := time.Unix(1000, 0).UTC()

	whole := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: path, Start: base},
	}, time.Hour, 0, 0)
	require.Contains(t, whole, "#EXTINF:20.000,")
	require.Contains(t, whole, "long.mp4?hls=media&sn=0&td=0")
	require.NotContains(t, whole, "off=")
	require.NotContains(t, whole, "#EXT-X-BYTERANGE")

	sliced := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: path, Start: base},
	}, time.Hour, 5*time.Second, 0)
	require.NotContains(t, sliced, "#EXTINF:20.000,")
	require.Contains(t, sliced, "off=")
	require.Contains(t, sliced, "&n=")
	require.NotContains(t, sliced, "#EXT-X-BYTERANGE")
	require.GreaterOrEqual(t, strings.Count(sliced, "hls=media&off="), 3)
	require.Equal(t, 1, strings.Count(sliced, "#EXT-X-MAP:"))
	require.Contains(t, sliced, "td=0")
	require.Contains(t, sliced, "#EXT-X-TARGETDURATION:")
}

func TestGenerateArchiveM3U8KeepsShortFMP4Whole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.mp4")
	writeFMP4Parts(t, path, 10, 1)
	base := time.Unix(1000, 0).UTC()
	body := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: path, Start: base},
	}, 10*time.Second, 5*time.Second, 0)
	require.Contains(t, body, "#EXTINF:10.000,")
	require.Contains(t, body, "short.mp4?hls=media&sn=0&td=0")
	require.NotContains(t, body, "off=")
}

func TestGenerateTimeshiftM3U8IndexedFiltersSlicedChunks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hour.mp4")
	writeFMP4Parts(t, path, 60, 1)
	base := time.Unix(1_000_000, 0).UTC()
	edge := base.Add(60 * time.Second)
	body := GenerateTimeshiftM3U8Indexed(conf.RecordFormatFMP4, []*IndexedSegment{
		{
			Rel:    filepath.Base(path),
			common: dir,
			Start:  base,
			fmp4: fmp4SegMeta{
				Duration:  60 * time.Second,
				MoofCount: 60,
				Ready:     true,
				Chunks:    groupHLSChunks(mustLoadParts(t, path), 5*time.Second),
			},
		},
	}, 60*time.Second, 5*time.Second, 0, edge)
	require.NotContains(t, body, "#EXTINF:60.000,")
	require.Contains(t, body, "off=")
	require.LessOrEqual(t, strings.Count(body, "hls=media&off="), 6)
	require.GreaterOrEqual(t, strings.Count(body, "hls=media&off="), 3)
}

func TestServeFMP4ArchivePartSlice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.mp4")
	writeFMP4Parts(t, path, 20, 1)
	parts, err := loadFMP4MediaParts(path)
	require.NoError(t, err)
	require.Len(t, parts, 20)

	chunks := groupHLSChunks(parts, 5*time.Second)
	require.GreaterOrEqual(t, len(chunks), 2)

	ch := chunks[0]
	w := serveHLSPart(t, path, "hls=media&sn=0&td=0&off="+strconv.FormatInt(ch.Off, 10)+"&n="+strconv.Itoa(ch.N))
	require.Equal(t, http.StatusOK, w.Code)
	var got fmp4.Parts
	require.NoError(t, got.Unmarshal(w.Body.Bytes()))
	require.Len(t, got, ch.N)

	w2 := serveHLSPart(t, path, "hls=media&sn=0&td=0")
	require.Equal(t, http.StatusOK, w2.Code)
	var all fmp4.Parts
	require.NoError(t, all.Unmarshal(w2.Body.Bytes()))
	require.Len(t, all, 20)

	bad := serveHLSPart(t, path, "hls=media&off=1")
	require.Equal(t, http.StatusBadRequest, bad.Code)
}

func TestGenerateArchiveM3U8FMP4TdFollowsLastPTS(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	writeFMP4PartsAt(t, a, 20, 2, 80)
	writeFMP4PartsAt(t, b, 2, 1, 0)
	base := time.Unix(1000, 0).UTC()

	body := GenerateArchiveM3U8(conf.RecordFormatFMP4, []*recordstore.Segment{
		{Fpath: a, Start: base},
		{Fpath: b, Start: base.Add(20 * time.Second)},
	}, time.Hour, 5*time.Second, 0)

	require.NotContains(t, body, "#EXT-X-DISCONTINUITY")
	require.NotContains(t, body, "#EXT-X-GAP")
	require.Contains(t, body, "b.mp4?hls=media&sn=20&td=20080")
	require.NotContains(t, body, "td=20000")

	mediaW := serveHLSPart(t, b, "hls=media&sn=20&td=20080")
	require.Equal(t, http.StatusOK, mediaW.Code)
	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(mediaW.Body.Bytes()))
	require.NotEmpty(t, parts)
	require.Equal(t, uint64(20080), parts[0].Tracks[0].BaseTime)
}
