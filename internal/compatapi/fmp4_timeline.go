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

const fmp4InspectWorkers = 8

// inspectFMP4Segment reads playlist metadata from an fMP4 file in a single open.
func inspectFMP4Segment(fpath string) (fmp4SegMeta, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return fmp4SegMeta{}, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmp4SegMeta{}, err
	}

	initSize, duration, err := readFMP4InitHeader(f)
	if err != nil {
		return fmp4SegMeta{}, err
	}
	fmp4InitSizeCache.Store(fpath, fmp4InitCacheEntry{size: initSize, duration: duration})

	if fi.Size() <= initSize {
		return fmp4SegMeta{}, fmt.Errorf("no media")
	}

	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return fmp4SegMeta{}, err
	}
	initBuf := make([]byte, initSize)
	if _, err = io.ReadFull(f, initBuf); err != nil {
		return fmp4SegMeta{}, err
	}

	meta := fmp4SegMeta{
		Duration: duration,
		Ready:    true,
	}
	var init fmp4.Init
	if err = init.Unmarshal(bytes.NewReader(initBuf)); err == nil {
		meta.Tracks = init.Tracks
	}

	moofs, mediaDur, err := inspectFMP4Media(f, initSize, meta.Tracks)
	if err != nil {
		return fmp4SegMeta{}, err
	}
	if moofs == 0 {
		return fmp4SegMeta{}, fmt.Errorf("no moof")
	}
	meta.MoofCount = moofs
	if mediaDur > 0 {
		meta.Duration = mediaDur
	}
	return meta, nil
}

func inspectFMP4Segments(fpaths []string) []fmp4SegMeta {
	out := make([]fmp4SegMeta, len(fpaths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fmp4InspectWorkers)
	for i, fpath := range fpaths {
		if fpath == "" {
			continue
		}
		wg.Add(1)
		go func(i int, fpath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			meta, err := inspectFMP4Segment(fpath)
			if err == nil {
				out[i] = meta
			}
		}(i, fpath)
	}
	wg.Wait()
	return out
}

func loadFMP4Tracks(fpath string) []*fmp4.InitTrack {
	_, _, init, err := readFMP4Init(fpath)
	if err != nil || init == nil {
		return nil
	}
	return init.Tracks
}

func inspectFMP4Media(r io.ReadSeeker, initSize int64, tracks []*fmp4.InitTrack) (uint32, time.Duration, error) {
	if _, err := r.Seek(initSize, io.SeekStart); err != nil {
		return 0, 0, err
	}

	var (
		count        uint32
		lastMoofPos  int64
		lastMoofSize int64
		hdr          = make([]byte, 8)
	)
	for {
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, 0, err
		}
		_, err = io.ReadFull(r, hdr)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		if size < 8 {
			return 0, 0, fmt.Errorf("invalid box size %d", size)
		}
		if string(hdr[4:8]) == "moof" {
			count++
			lastMoofPos = pos
			lastMoofSize = size
		}
		if _, err = r.Seek(size-8, io.SeekCurrent); err != nil {
			return 0, 0, err
		}
	}
	if count == 0 {
		return 0, 0, nil
	}

	var dur time.Duration
	if lastMoofSize > 0 && len(tracks) > 0 {
		if _, err := r.Seek(lastMoofPos, io.SeekStart); err == nil {
			buf := make([]byte, lastMoofSize)
			if _, err = io.ReadFull(r, buf); err == nil {
				dur = fmp4DurationFromMoof(buf, tracks)
			}
		}
	}
	return count, dur, nil
}

func fmp4DurationFromMoof(moof []byte, tracks []*fmp4.InitTrack) time.Duration {
	tsByID := make(map[uint32]uint32, len(tracks))
	for _, tr := range tracks {
		if tr != nil && tr.TimeScale != 0 {
			tsByID[uint32(tr.ID)] = tr.TimeScale
		}
	}

	var (
		trackID    uint32
		base       uint64
		defDur     uint32
		maxElapsed time.Duration
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
			if len(trun.Entries) == 0 {
				sum = uint64(defDur)
			} else {
				for _, e := range trun.Entries {
					d := e.SampleDuration
					if d == 0 {
						d = defDur
					}
					sum += uint64(d)
				}
			}
			if ts := tsByID[trackID]; ts != 0 {
				elapsed := time.Duration(base+sum) * time.Second / time.Duration(ts)
				if elapsed > maxElapsed {
					maxElapsed = elapsed
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return 0
	}
	return maxElapsed
}

func trackDecodeOffsets(tracks []*fmp4.InitTrack, offset time.Duration) map[uint32]uint64 {
	out := make(map[uint32]uint64, len(tracks))
	if offset <= 0 {
		return out
	}
	for _, tr := range tracks {
		if tr == nil || tr.TimeScale == 0 {
			continue
		}
		out[uint32(tr.ID)] = uint64(offset) * uint64(tr.TimeScale) / uint64(time.Second)
	}
	return out
}

// patchFMP4MediaTimeline rewrites mfhd.sequence_number and tfdt base times in place
// so adjacent archive segments form one continuous fMP4 timeline (needed for VLC seek).
func patchFMP4MediaTimeline(media []byte, startSeq uint32, trackOffsets map[uint32]uint64) error {
	seq := startSeq
	i := 0
	for i+8 <= len(media) {
		size := int(binary.BigEndian.Uint32(media[i : i+4]))
		if size < 8 || i+size > len(media) {
			return fmt.Errorf("invalid top-level box at %d", i)
		}
		if string(media[i+4:i+8]) == "moof" {
			if err := patchMoof(media[i+8:i+size], seq, trackOffsets); err != nil {
				return err
			}
			seq++
		}
		i += size
	}
	if i != len(media) {
		return fmt.Errorf("trailing bytes in media section")
	}
	return nil
}

func patchMoof(moof []byte, seq uint32, trackOffsets map[uint32]uint64) error {
	i := 0
	for i+8 <= len(moof) {
		size := int(binary.BigEndian.Uint32(moof[i : i+4]))
		if size < 8 || i+size > len(moof) {
			return fmt.Errorf("invalid moof child box")
		}
		typ := string(moof[i+4 : i+8])
		switch typ {
		case "mfhd":
			if size < 16 {
				return fmt.Errorf("mfhd too short")
			}
			binary.BigEndian.PutUint32(moof[i+12:i+16], seq)
		case "traf":
			if err := patchTraf(moof[i+8:i+size], trackOffsets); err != nil {
				return err
			}
		}
		i += size
	}
	return nil
}

func patchTraf(traf []byte, trackOffsets map[uint32]uint64) error {
	if len(trackOffsets) == 0 {
		return nil
	}

	var trackID uint32
	i := 0
	for i+8 <= len(traf) {
		size := int(binary.BigEndian.Uint32(traf[i : i+4]))
		if size < 8 || i+size > len(traf) {
			return fmt.Errorf("invalid traf child box")
		}
		typ := string(traf[i+4 : i+8])
		switch typ {
		case "tfhd":
			if size < 16 {
				return fmt.Errorf("tfhd too short")
			}
			trackID = binary.BigEndian.Uint32(traf[i+12 : i+16])
		case "tfdt":
			off := trackOffsets[trackID]
			if off == 0 {
				break
			}
			if size < 16 {
				return fmt.Errorf("tfdt too short")
			}
			ver := traf[i+8]
			if ver == 1 {
				if size < 20 {
					return fmt.Errorf("tfdt v1 too short")
				}
				old := binary.BigEndian.Uint64(traf[i+12 : i+20])
				binary.BigEndian.PutUint64(traf[i+12:i+20], old+off)
			} else {
				old := binary.BigEndian.Uint32(traf[i+12 : i+16])
				binary.BigEndian.PutUint32(traf[i+12:i+16], old+uint32(off))
			}
		}
		i += size
	}
	return nil
}
