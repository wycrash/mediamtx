package compatapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

type fmp4InitCacheEntry struct {
	size     int64
	duration time.Duration
}

var fmp4InitSizeCache sync.Map // path -> fmp4InitCacheEntry

const sampleFlagIsNonSyncSample = 1 << 16

func fmp4InitSize(fpath string) (int64, error) {
	size, _, err := fmp4InitMeta(fpath)
	return size, err
}

func fmp4InitMeta(fpath string) (int64, time.Duration, error) {
	if v, ok := fmp4InitSizeCache.Load(fpath); ok {
		e := v.(fmp4InitCacheEntry)
		return e.size, e.duration, nil
	}

	f, err := os.Open(fpath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	size, duration, err := readFMP4InitHeader(f)
	if err != nil {
		return 0, 0, err
	}
	fmp4InitSizeCache.Store(fpath, fmp4InitCacheEntry{size: size, duration: duration})
	return size, duration, nil
}

func readFMP4InitHeader(r io.ReadSeeker) (initSize int64, duration time.Duration, err error) {
	buf := make([]byte, 8)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return 0, 0, err
	}
	if !bytes.Equal(buf[4:], []byte{'f', 't', 'y', 'p'}) {
		return 0, 0, fmt.Errorf("ftyp box not found")
	}
	ftypSize := int64(uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]))

	_, err = r.Seek(ftypSize, io.SeekStart)
	if err != nil {
		return 0, 0, err
	}

	_, err = io.ReadFull(r, buf)
	if err != nil {
		return 0, 0, err
	}
	if !bytes.Equal(buf[4:], []byte{'m', 'o', 'o', 'v'}) {
		return 0, 0, fmt.Errorf("moov box not found")
	}
	moovSize := int64(uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]))

	_, err = r.Seek(ftypSize+8, io.SeekStart)
	if err != nil {
		return 0, 0, err
	}

	var mvhd amp4.Mvhd
	_, err = amp4.Unmarshal(r, uint64(moovSize-8), &mvhd, amp4.Context{})
	if err == nil && mvhd.Timescale != 0 {
		duration = time.Duration(mvhd.GetDuration()) * time.Second / time.Duration(mvhd.Timescale)
	}

	return ftypSize + moovSize, duration, nil
}

func readFMP4Init(fpath string) (initSize int64, duration time.Duration, init *fmp4.Init, err error) {
	f, err := os.Open(fpath)
	if err != nil {
		return 0, 0, nil, err
	}
	defer f.Close()

	initSize, duration, err = readFMP4InitHeader(f)
	if err != nil {
		return 0, 0, nil, err
	}
	fmp4InitSizeCache.Store(fpath, fmp4InitCacheEntry{size: initSize, duration: duration})

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return 0, 0, nil, err
	}
	initBuf := make([]byte, initSize)
	_, err = io.ReadFull(f, initBuf)
	if err != nil {
		return 0, 0, nil, err
	}

	var parsed fmp4.Init
	err = parsed.Unmarshal(bytes.NewReader(initBuf))
	if err != nil {
		return 0, 0, nil, err
	}
	return initSize, duration, &parsed, nil
}

func fmp4TracksCompatible(a, b []*fmp4.InitTrack) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	if &a[0] == &b[0] {
		return true
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].TimeScale != b[i].TimeScale {
			return false
		}
		if reflect.TypeOf(a[i].Codec) != reflect.TypeOf(b[i].Codec) {
			return false
		}
		if !reflect.DeepEqual(a[i].Codec, b[i].Codec) {
			return false
		}
	}
	return true
}

// ExtractPreviewFMP4 extracts the first video keyframe from a recorded fMP4 segment.
func ExtractPreviewFMP4(fpath string) ([]byte, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}

	initSize, _, err := readFMP4InitHeader(f)
	if err != nil {
		return nil, err
	}

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}
	initBuf := make([]byte, initSize)
	_, err = io.ReadFull(f, initBuf)
	if err != nil {
		return nil, err
	}

	var init fmp4.Init
	err = init.Unmarshal(bytes.NewReader(initBuf))
	if err != nil {
		return nil, err
	}

	var videoTrack *fmp4.InitTrack
	for _, tr := range init.Tracks {
		if tr.Codec != nil && tr.Codec.IsVideo() {
			videoTrack = tr
			break
		}
	}
	if videoTrack == nil {
		return nil, errNoVideoTrack
	}

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}

	var (
		payload    []byte
		gotSample  bool
		moofOffset uint64
		tfhd       *amp4.Tfhd
		tfdt       *amp4.Tfdt
	)

	_, err = amp4.ReadBoxStructure(f, func(h *amp4.ReadHandle) (any, error) {
		if gotSample {
			return nil, errPreviewDone
		}

		switch h.BoxInfo.Type.String() {
		case "moof":
			moofOffset = h.BoxInfo.Offset
			return h.Expand()

		case "traf":
			return h.Expand()

		case "tfhd":
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			tfhd = box.(*amp4.Tfhd)

		case "tfdt":
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			tfdt = box.(*amp4.Tfdt)
			_ = tfdt

		case "trun":
			if tfhd == nil || int(tfhd.TrackID) != videoTrack.ID {
				return nil, nil
			}
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			trun := box.(*amp4.Trun)
			dataOffset := moofOffset + uint64(trun.DataOffset)

			for _, e := range trun.Entries {
				isNonSync := (e.SampleFlags & sampleFlagIsNonSyncSample) != 0
				if isNonSync {
					dataOffset += uint64(e.SampleSize)
					continue
				}
				buf := make([]byte, e.SampleSize)
				n, err2 := f.ReadAt(buf, int64(dataOffset))
				if err2 != nil {
					return nil, err2
				}
				if n != int(e.SampleSize) {
					return nil, fmt.Errorf("partial read")
				}
				payload = buf
				gotSample = true
				return nil, errPreviewDone
			}
		}
		return nil, nil
	})
	if err != nil && !errors.Is(err, errPreviewDone) {
		return nil, err
	}
	if !gotSample || len(payload) == 0 {
		return nil, errNoVideoKeyframe
	}
	return marshalSingleFrameMP4(videoTrack.Codec, payload)
}
