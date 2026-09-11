package compatapi

import (
	"os"
	"time"
)

func dvrDayUnix(day string) int64 {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func packChunksFromHLS(cs []hlsMediaChunk) []dvrPackChunk {
	if len(cs) == 0 {
		return nil
	}
	out := make([]dvrPackChunk, len(cs))
	for i, ch := range cs {
		moof := ch.MoofCount
		if moof == 0 && ch.N > 0 {
			moof = uint32(ch.N)
		}
		if moof > 0xFFFF {
			moof = 0xFFFF
		}
		off := ch.Off
		if off < 0 {
			off = 0
		}
		if off > int64(^uint32(0)) {
			off = int64(^uint32(0))
		}
		out[i] = dvrPackChunk{
			Off:      uint32(off),
			Duration: ch.Duration,
			PTSEnd:   ch.PTSEnd,
			Moof:     uint16(moof),
		}
	}
	return out
}

func hlsChunksFromPack(cs []dvrPackChunk) []hlsMediaChunk {
	if len(cs) == 0 {
		return nil
	}
	out := make([]hlsMediaChunk, len(cs))
	var elapsed time.Duration
	for i, ch := range cs {
		ptsStart := elapsed
		if i > 0 && cs[i-1].PTSEnd > 0 {
			ptsStart = cs[i-1].PTSEnd
		}
		n := int(ch.Moof)
		out[i] = hlsMediaChunk{
			Off:       int64(ch.Off),
			N:         n,
			Duration:  ch.Duration,
			PTSStart:  ptsStart,
			PTSEnd:    ch.PTSEnd,
			MoofCount: uint32(ch.Moof),
		}
		elapsed += ch.Duration
	}
	return out
}

func overlayPackChunks(segs []*IndexedSegment, p *dvrPack) {
	if p == nil || len(segs) == 0 || p.chunkCount() == 0 {
		return
	}
	byStart := make(map[int64]*IndexedSegment, len(segs))
	for _, seg := range segs {
		if seg == nil || seg.Start.IsZero() {
			continue
		}
		byStart[seg.Start.UnixMicro()] = seg
	}
	for i := 0; i < p.segCount(); i++ {
		if p.segChunkN[i] == 0 {
			continue
		}
		seg, ok := byStart[p.segStartUs[i]]
		if !ok || seg == nil {
			continue
		}
		rec := p.seg(i)
		if len(rec.Chunks) < 2 {
			continue
		}
		seg.fmp4.Chunks = hlsChunksFromPack(rec.Chunks)
	}
}

func loadDayPack(path string, hash uint64) (*dvrPack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeDvrPack(data, hash)
}

func writeDayPackFile(path string, p *dvrPack) error {
	if path == "" || p == nil {
		return nil
	}
	raw, err := encodeDvrPack(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func packFromSegs(hash uint64, dayUnix int64, segs []*IndexedSegment) *dvrPack {
	p := &dvrPack{hash: hash, dayUnix: dayUnix}
	hasChunks := false
	for _, seg := range segs {
		if seg == nil || !seg.fmp4.Ready {
			continue
		}
		rec := dvrPackSeg{
			Start:    seg.Start,
			Duration: seg.fmp4.Duration,
			Moof:     uint16(minU32(seg.fmp4.MoofCount, 0xFFFF)),
			CodecID:  seg.fmp4.codecID,
			Ready:    true,
			Chunks:   packChunksFromHLS(seg.fmp4.Chunks),
		}
		if n := len(seg.fmp4.Chunks); n > 0 {
			rec.PTSEnd = seg.fmp4.Chunks[n-1].PTSEnd
			hasChunks = true
		} else {
			rec.PTSEnd = seg.fmp4.Duration
		}
		p.appendSeg(rec)
	}
	if !hasChunks {
		return nil
	}
	p.ranges = dvrPackRanges(p, 2*time.Second)
	return p
}

func minU32(v, cap uint32) uint32 {
	if v > cap {
		return cap
	}
	return v
}
