package compatapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

const (
	hlsChunkWholeFileFloor = 15 * time.Second
	maxHLSSliceParts       = 8192
	// hlsChunkMaxOvershoot caps how far past the target a chunk may grow while
	// waiting for an IDR-carrying part. The recorder cuts parts by
	// partDuration only, so a stream whose keyframes never land on a part
	// boundary offers no split candidate and would collapse into one chunk
	// spanning the whole file.
	hlsChunkMaxOvershoot   = 3
	trunSampleFlagsPresent = 0x000400
	trunFirstSampleFlags   = 0x000004
)

type fmp4MediaPart struct {
	Off      int64
	Len      int64
	Duration time.Duration
	PTSStart time.Duration
	HasIDR   bool
}

func (p fmp4MediaPart) PTSEnd() time.Duration {
	return p.PTSStart + p.Duration
}

type fmp4PartIndex struct {
	size  int64
	parts []fmp4MediaPart
}

type hlsMediaChunk struct {
	Off       int64
	N         int
	Duration  time.Duration
	PTSStart  time.Duration
	PTSEnd    time.Duration
	MoofCount uint32
}

var fmp4PartIndexCache sync.Map // path -> fmp4PartIndex

func forgetFMP4FileCaches(fpath string) {
	fmp4InitSizeCache.Delete(fpath)
	fmp4PartIndexCache.Delete(fpath)
}

func shouldSliceFMP4(fileDuration, chunkDuration time.Duration) bool {
	if chunkDuration <= 0 || fileDuration <= 0 {
		return false
	}
	limit := 2 * chunkDuration
	if limit < hlsChunkWholeFileFloor {
		limit = hlsChunkWholeFileFloor
	}
	return fileDuration > limit
}

func hlsPlaylistStep(segmentDuration, chunkDuration time.Duration) time.Duration {
	if chunkDuration > 0 {
		return chunkDuration
	}
	return segmentDuration
}

func loadFMP4MediaParts(fpath string) ([]fmp4MediaPart, error) {
	fi, err := os.Stat(fpath)
	if err != nil {
		return nil, err
	}
	if v, ok := fmp4PartIndexCache.Load(fpath); ok {
		cached := v.(fmp4PartIndex)
		if cached.size == fi.Size() {
			return cached.parts, nil
		}
	}

	parts, err := inspectFMP4MediaPartsFile(fpath)
	if err != nil {
		return nil, err
	}
	fmp4PartIndexCache.Store(fpath, fmp4PartIndex{size: fi.Size(), parts: parts})
	return parts, nil
}

func inspectFMP4MediaPartsFile(fpath string) ([]fmp4MediaPart, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	initSize, err := fmp4InitSize(fpath)
	if err != nil {
		return nil, err
	}
	tracks := loadFMP4Tracks(fpath)
	return inspectFMP4MediaParts(f, initSize, tracks)
}

func inspectFMP4MediaParts(r io.ReadSeeker, initSize int64, tracks []*fmp4.InitTrack) ([]fmp4MediaPart, error) {
	if _, err := r.Seek(initSize, io.SeekStart); err != nil {
		return nil, err
	}

	tsByID := make(map[uint32]uint32, len(tracks))
	videoIDs := make(map[uint32]struct{})
	for _, tr := range tracks {
		if tr == nil || tr.TimeScale == 0 {
			continue
		}
		tsByID[uint32(tr.ID)] = tr.TimeScale
		if tr.Codec != nil && tr.Codec.IsVideo() {
			videoIDs[uint32(tr.ID)] = struct{}{}
		}
	}

	var (
		parts   []fmp4MediaPart
		pending *fmp4MediaPart
		hdr     = make([]byte, 8)
	)
	closePending := func() {
		if pending == nil {
			return
		}
		parts = append(parts, *pending)
		pending = nil
	}

	for {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		_, err = io.ReadFull(r, hdr)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		if size < 8 {
			return nil, fmt.Errorf("invalid box size %d", size)
		}
		typ := string(hdr[4:8])
		switch typ {
		case "moof":
			closePending()
			buf := make([]byte, size)
			copy(buf[:8], hdr)
			if size > 8 {
				if _, err = io.ReadFull(r, buf[8:]); err != nil {
					return nil, err
				}
			}
			dur, ptsStart, hasIDR := inspectMoofPart(buf, videoIDs, tsByID)
			pending = &fmp4MediaPart{
				Off:      pos,
				Len:      size,
				Duration: dur,
				PTSStart: ptsStart,
				HasIDR:   hasIDR,
			}
			continue
		case "mdat":
			if pending != nil {
				pending.Len = pos + size - pending.Off
			}
		}
		if _, err = r.Seek(size-8, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
	closePending()
	if len(parts) == 0 {
		return nil, fmt.Errorf("no moof")
	}
	return parts, nil
}

func inspectMoofPart(moof []byte, videoIDs map[uint32]struct{}, tsByID map[uint32]uint32) (time.Duration, time.Duration, bool) {
	var (
		trackID    uint32
		base       uint64
		defDur     uint32
		defFlags   uint32
		hasDefFlag bool
		maxDur     time.Duration
		maxPTS     time.Duration
		videoDur   time.Duration
		videoPTS   time.Duration
		haveVideo  bool
		hasIDR     bool
	)
	_, err := amp4.ReadBoxStructure(bytes.NewReader(moof), func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof", "traf":
			return h.Expand()
		case "tfhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfhd := box.(*amp4.Tfhd)
			trackID = tfhd.TrackID
			defDur = tfhd.DefaultSampleDuration
			base = 0
			if tfhd.GetFlags()&amp4.TfhdDefaultSampleFlagsPresent != 0 {
				defFlags = tfhd.DefaultSampleFlags
				hasDefFlag = true
			}
		case "tfdt":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			base = box.(*amp4.Tfdt).GetBaseMediaDecodeTime()
		case "trun":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*amp4.Trun)
			sum := uint64(0)
			_, isVideo := videoIDs[trackID]
			flags := trun.GetFlags()
			sampleFlagsPresent := flags&trunSampleFlagsPresent != 0
			firstSampleFlagsPresent := flags&trunFirstSampleFlags != 0
			if len(trun.Entries) == 0 {
				sum = uint64(defDur)
				if isVideo && !sampleIsNonSync(defFlags, hasDefFlag) {
					hasIDR = true
				}
			} else {
				for i, e := range trun.Entries {
					d := e.SampleDuration
					if d == 0 {
						d = defDur
					}
					sum += uint64(d)
					if !isVideo {
						continue
					}
					sflags := e.SampleFlags
					haveFlags := sampleFlagsPresent
					if i == 0 && firstSampleFlagsPresent {
						sflags = trun.FirstSampleFlags
						haveFlags = true
					}
					if !haveFlags {
						sflags = defFlags
						haveFlags = hasDefFlag
					}
					if haveFlags && !sampleIsNonSync(sflags, true) {
						hasIDR = true
					}
				}
			}
			if ts := tsByID[trackID]; ts != 0 {
				dur := time.Duration(sum) * time.Second / time.Duration(ts)
				pts := time.Duration(base) * time.Second / time.Duration(ts)
				if isVideo && (!haveVideo || dur > videoDur) {
					haveVideo = true
					videoDur = dur
					videoPTS = pts
				}
				if dur > maxDur {
					maxDur = dur
					maxPTS = pts
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return 0, 0, false
	}
	if haveVideo {
		return videoDur, videoPTS, hasIDR
	}
	return maxDur, maxPTS, hasIDR
}

func sampleIsNonSync(flags uint32, present bool) bool {
	if !present {
		return true
	}
	return flags&sampleFlagIsNonSyncSample != 0
}

func groupHLSChunks(parts []fmp4MediaPart, target time.Duration) []hlsMediaChunk {
	if len(parts) == 0 {
		return nil
	}
	if target <= 0 {
		return []hlsMediaChunk{makeHLSChunk(parts)}
	}

	anyIDR := false
	for _, p := range parts {
		if p.HasIDR {
			anyIDR = true
			break
		}
	}

	hardLimit := hlsChunkMaxOvershoot * target

	var out []hlsMediaChunk
	start := 0
	var dur time.Duration
	for i, p := range parts {
		if i == start {
			dur = p.Duration
			continue
		}
		split := dur >= target
		if split && anyIDR && !p.HasIDR && dur < hardLimit {
			split = false
		}
		if split {
			out = append(out, makeHLSChunk(parts[start:i]))
			start = i
			dur = p.Duration
			continue
		}
		dur += p.Duration
	}
	out = append(out, makeHLSChunk(parts[start:]))
	return out
}

func makeHLSChunk(parts []fmp4MediaPart) hlsMediaChunk {
	var dur time.Duration
	for _, p := range parts {
		dur += p.Duration
	}
	ptsEnd := parts[len(parts)-1].PTSEnd()
	return hlsMediaChunk{
		Off:       parts[0].Off,
		N:         len(parts),
		Duration:  dur,
		PTSStart:  parts[0].PTSStart,
		PTSEnd:    ptsEnd,
		MoofCount: uint32(len(parts)),
	}
}

func fmp4HLSSliceRange(fpath string, off int64, n int) (int64, int64, error) {
	if n < 1 || n > maxHLSSliceParts {
		return 0, 0, fmt.Errorf("invalid HLS slice part count")
	}
	parts, err := loadFMP4MediaParts(fpath)
	if err != nil {
		return 0, 0, err
	}
	i := -1
	for j, p := range parts {
		if p.Off == off {
			i = j
			break
		}
	}
	if i < 0 || i+n > len(parts) {
		return 0, 0, fmt.Errorf("HLS slice not found")
	}
	start := parts[i].Off
	end := parts[i+n-1].Off + parts[i+n-1].Len
	if end <= start {
		return 0, 0, fmt.Errorf("invalid HLS slice range")
	}
	return start, end - start, nil
}
