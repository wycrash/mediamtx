package codecs

// MPEG1Audio is a MPEG-1/2 Audio codec (MP1/MP2/MP3 in MPEG-TS).
type MPEG1Audio struct{}

// IsVideo returns whether the codec is a video one.
func (*MPEG1Audio) IsVideo() bool {
	return false
}

func (*MPEG1Audio) isCodec() {
}
