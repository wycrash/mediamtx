package compatapi

import (
	"strconv"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/formatlabel"
)

// InfoTrack is a Flussonic-compatible media track.
// Only fields MediaMTX can populate are included.
type InfoTrack struct {
	Profile    string `json:"profile,omitempty"`
	Level      string `json:"level,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Codec      string `json:"codec"`
	TrackID    string `json:"track_id"`
	Content    string `json:"content"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
}

// InfoMediaInfo is Flussonic media_info.
type InfoMediaInfo struct {
	Tracks   []InfoTrack `json:"tracks"`
	FlowType string      `json:"flow_type"`
	StreamID int         `json:"stream_id"`
}

// InfoDVR is Flussonic dvr_info built from recording ranges.
type InfoDVR struct {
	Depth    int64            `json:"depth"`
	Duration int64            `json:"duration"`
	From     int64            `json:"from,omitempty"`
	Ranges   []RecordingRange `json:"ranges"`
}

// InfoStats is the stats object of info.json.
type InfoStats struct {
	Status    string        `json:"status"`
	MediaInfo InfoMediaInfo `json:"media_info"`
	DVRInfo   InfoDVR       `json:"dvr_info"`
}

// InfoJSON is GET /{path}/info.json.
type InfoJSON struct {
	Name  string    `json:"name"`
	Stats InfoStats `json:"stats"`
}

func buildInfo(name string, path *defs.APIPath, ranges []RecordingRange, recTracks []*fmp4.InitTrack) InfoJSON {
	status := "waiting"
	if path != nil && (path.Available || path.Online) {
		status = "running"
	}

	tracks := []InfoTrack{}
	if path != nil && len(path.Tracks2) > 0 {
		tracks = tracksFromAPI(path.Tracks2)
	}
	if len(tracks) == 0 && len(recTracks) > 0 {
		tracks = tracksFromFMP4(recTracks)
	}

	if ranges == nil {
		ranges = []RecordingRange{}
	}

	return InfoJSON{
		Name: name,
		Stats: InfoStats{
			Status: status,
			MediaInfo: InfoMediaInfo{
				Tracks:   tracks,
				FlowType: "stream",
				StreamID: 0,
			},
			DVRInfo: buildDVRInfo(ranges),
		},
	}
}

func buildDVRInfo(ranges []RecordingRange) InfoDVR {
	out := InfoDVR{
		Ranges: ranges,
	}
	if len(ranges) == 0 {
		out.Ranges = []RecordingRange{}
		return out
	}
	out.From = ranges[0].From
	last := ranges[len(ranges)-1]
	out.Depth = last.From + last.Duration - ranges[0].From
	for _, r := range ranges {
		out.Duration += r.Duration
	}
	return out
}

func tracksFromAPI(tracks []defs.APIPathTrack) []InfoTrack {
	out := make([]InfoTrack, 0, len(tracks))
	vN, aN := 0, 0
	for _, t := range tracks {
		it, ok := infoTrackFromAPI(t)
		if !ok {
			continue
		}
		switch it.Content {
		case "video":
			vN++
			it.TrackID = "v" + strconv.Itoa(vN)
		case "audio":
			aN++
			it.TrackID = "a" + strconv.Itoa(aN)
		default:
			continue
		}
		out = append(out, it)
	}
	return out
}

func infoTrackFromAPI(t defs.APIPathTrack) (InfoTrack, bool) {
	codec, content := flussonicCodec(t.Codec, t.CodecProps)
	if codec == "" || content == "" {
		return InfoTrack{}, false
	}
	it := InfoTrack{
		Codec:   codec,
		Content: content,
	}
	switch p := t.CodecProps.(type) {
	case *defs.APIPathTrackCodecPropsH264:
		it.Profile = p.Profile
		it.Level = p.Level
		it.Width = p.Width
		it.Height = p.Height
	case *defs.APIPathTrackCodecPropsH265:
		it.Profile = p.Profile
		it.Level = p.Level
		it.Width = p.Width
		it.Height = p.Height
	case *defs.APIPathTrackCodecPropsAV1:
		if p.Profile != 0 {
			it.Profile = strconv.Itoa(p.Profile)
		}
		it.Width = p.Width
		it.Height = p.Height
	case *defs.APIPathTrackCodecPropsVP9:
		if p.Profile != 0 {
			it.Profile = strconv.Itoa(p.Profile)
		}
	case *defs.APIPathTrackCodecPropsG711:
		it.Channels = p.ChannelCount
		it.SampleRate = p.SampleRate
	case *defs.APIPathTrackCodecPropsMPEG4Audio:
		it.Channels = p.ChannelCount
		it.SampleRate = p.SampleRate
	case *defs.APIPathTrackCodecPropsAC3:
		it.Channels = p.ChannelCount
		it.SampleRate = p.SampleRate
	case *defs.APIPathTrackCodecPropsOpus:
		it.Channels = p.ChannelCount
	case *defs.APIPathTrackCodecPropsLPCM:
		it.Channels = p.ChannelCount
		it.SampleRate = p.SampleRate
	}
	return it, true
}

func flussonicCodec(codec defs.APIPathTrackCodec, props defs.APIPathTrackCodecProps) (string, string) {
	switch codec {
	case formatlabel.H264:
		return "h264", "video"
	case formatlabel.H265:
		return "h265", "video"
	case formatlabel.AV1:
		return "av1", "video"
	case formatlabel.VP9:
		return "vp9", "video"
	case formatlabel.VP8:
		return "vp8", "video"
	case formatlabel.MJPEG:
		return "mjpeg", "video"
	case formatlabel.MPEG4Video:
		return "mp4v", "video"
	case formatlabel.MPEG1Video:
		return "mpeg2video", "video"
	case formatlabel.Opus:
		return "opus", "audio"
	case formatlabel.MPEG4Audio, formatlabel.MPEG4AudioLATM:
		return "aac", "audio"
	case formatlabel.MPEG1Audio:
		return "mp2", "audio"
	case formatlabel.AC3:
		return "ac3", "audio"
	case formatlabel.G711:
		if g711, ok := props.(*defs.APIPathTrackCodecPropsG711); ok && g711.MULaw {
			return "pcmu", "audio"
		}
		return "pcma", "audio"
	case formatlabel.G722:
		return "g722", "audio"
	case formatlabel.LPCM:
		return "pcm", "audio"
	default:
		return "", ""
	}
}

func tracksFromFMP4(initTracks []*fmp4.InitTrack) []InfoTrack {
	formats := make([]format.Format, 0, len(initTracks))
	for _, tr := range initTracks {
		if tr == nil || tr.Codec == nil {
			continue
		}
		if f := fmp4CodecToFormat(tr.Codec); f != nil {
			formats = append(formats, f)
		}
	}
	if len(formats) == 0 {
		return []InfoTrack{}
	}
	return tracksFromAPI(defs.MediasToTracks([]*description.Media{{
		Formats: formats,
	}}))
}

func fmp4CodecToFormat(c mcodecs.Codec) format.Format {
	switch codec := c.(type) {
	case *mcodecs.H264:
		return &format.H264{SPS: codec.SPS, PPS: codec.PPS}
	case *mcodecs.H265:
		return &format.H265{VPS: codec.VPS, SPS: codec.SPS, PPS: codec.PPS}
	case *mcodecs.MPEG4Audio:
		cfg := codec.Config
		return &format.MPEG4Audio{Config: &cfg}
	case *mcodecs.Opus:
		return &format.Opus{ChannelCount: codec.ChannelCount}
	case *mcodecs.AC3:
		return &format.AC3{SampleRate: codec.SampleRate, ChannelCount: codec.ChannelCount}
	case *mcodecs.LPCM:
		return &format.LPCM{
			BitDepth:     codec.BitDepth,
			SampleRate:   codec.SampleRate,
			ChannelCount: codec.ChannelCount,
		}
	default:
		return nil
	}
}
