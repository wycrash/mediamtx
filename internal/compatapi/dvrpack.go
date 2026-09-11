package compatapi

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"sort"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// On-disk index v2. One camera-day on one disk is a single file of
// fixed-length records: a sealed day is a .pack, the day being recorded is a
// .wal appended on every closed segment. Both hold the same segment record, so
// sealing rewrites the array instead of converting it.
//
// Segment file names are not stored: they are Path{Start}.Encode(recordPath),
// and recordPath is part of the config hash, so a template change invalidates
// the index instead of producing wrong paths. Only names that do not match the
// template are kept, in a side table.
const (
	dvrPackMagic   = "MTX2"
	dvrWalMagic    = "MTXW"
	dvrPackVersion = uint16(1)

	dvrPackHeaderSize = 64
	dvrWalHeaderSize  = 32
	dvrPackSegSize    = 20
	dvrPackChunkSize  = 16
	dvrPackRangeSize  = 16

	dvrWalOpSeg    = uint8(1)
	dvrWalOpDelete = uint8(2)
	dvrWalOpCodec  = uint8(3)

	dvrPackSegReady   = uint8(1 << 0)
	dvrPackSegHasName = uint8(1 << 1)

	// A day is read whole, so refuse implausible counts rather than allocate
	// from a corrupt length field. A day cannot hold more segments than it has
	// deciseconds.
	dvrPackMaxSegs   = 1 << 20
	dvrPackMaxChunks = 1 << 22
)

var (
	errDvrPackMagic   = errors.New("dvr pack: bad magic")
	errDvrPackVersion = errors.New("dvr pack: unsupported version")
	errDvrPackHash    = errors.New("dvr pack: config hash mismatch")
	errDvrPackCRC     = errors.New("dvr pack: checksum mismatch")
	errDvrPackTrunc   = errors.New("dvr pack: truncated")
)

// dvrPackSeg is one recording file. Used to hand records in and out of a pack;
// the pack itself stores them column-wise.
type dvrPackSeg struct {
	Start    time.Time
	Duration time.Duration
	PTSEnd   time.Duration
	Moof     uint16
	CodecID  uint8
	Ready    bool
	// Name is set only when the file name does not follow the record template.
	Name   string
	Chunks []dvrPackChunk
}

// dvrPackChunk is one HLS playlist entry inside a segment file. Byte length is
// not stored: chunks are contiguous, so it is the distance to the next chunk
// and, for the last one, to the end of the file.
type dvrPackChunk struct {
	Off      uint32
	Duration time.Duration
	PTSEnd   time.Duration
	Moof     uint16
}

// dvrPack is one camera-day held as struct-of-arrays: a day loads with one
// read and costs no per-segment allocation.
type dvrPack struct {
	hash    uint64
	dayUnix int64

	segStartUs  []int64
	segDurMs    []uint32
	segPTSEndMs []uint32
	segChunkAt  []uint32
	segChunkN   []uint16
	segMoof     []uint16
	segCodecID  []uint8
	segFlags    []uint8

	chunkOff      []uint32
	chunkDurMs    []uint32
	chunkPTSEndMs []uint32
	chunkMoof     []uint16

	ranges []RecordingRange
	codecs [][]*fmp4.InitTrack
	// names is sparse: only segments whose file name does not follow the
	// record template appear here, keyed by segment index.
	names map[uint32]string
}

// dvrPackSummary is what tier 0 needs to answer ranges and calendar requests.
// It sits in the head of the pack so it can be read without the record arrays.
type dvrPackSummary struct {
	DayUnix      int64
	NSeg         uint32
	FirstStartUs int64
	LastEndUs    int64
	Ranges       []RecordingRange
}

func (p *dvrPack) segCount() int { return len(p.segStartUs) }

func (p *dvrPack) chunkCount() int { return len(p.chunkOff) }

func (p *dvrPack) segStart(i int) time.Time {
	return time.Unix(p.segStartUs[i]/1e6, (p.segStartUs[i]%1e6)*1000)
}

func (p *dvrPack) segEndUs(i int) int64 {
	return p.segStartUs[i] + int64(p.segDurMs[i])*1000
}

// seg rebuilds a whole record. Playlist generation reads the columns directly;
// this is for callers that want one segment.
func (p *dvrPack) seg(i int) dvrPackSeg {
	out := dvrPackSeg{
		Start:    p.segStart(i),
		Duration: time.Duration(p.segDurMs[i]) * time.Millisecond,
		PTSEnd:   time.Duration(p.segPTSEndMs[i]) * time.Millisecond,
		Moof:     p.segMoof[i],
		CodecID:  p.segCodecID[i],
		Ready:    p.segFlags[i]&dvrPackSegReady != 0,
		Name:     p.names[uint32(i)],
	}
	at, n := int(p.segChunkAt[i]), int(p.segChunkN[i])
	if n > 0 {
		out.Chunks = make([]dvrPackChunk, n)
		for j := 0; j < n; j++ {
			out.Chunks[j] = dvrPackChunk{
				Off:      p.chunkOff[at+j],
				Duration: time.Duration(p.chunkDurMs[at+j]) * time.Millisecond,
				PTSEnd:   time.Duration(p.chunkPTSEndMs[at+j]) * time.Millisecond,
				Moof:     p.chunkMoof[at+j],
			}
		}
	}
	return out
}

// findSeg returns the index of the segment starting at start, or -1.
// Segments are sorted, so this is a binary search over the start column.
func (p *dvrPack) findSeg(start time.Time) int {
	us := start.UnixMicro()
	i := sort.Search(len(p.segStartUs), func(i int) bool { return p.segStartUs[i] >= us })
	if i < len(p.segStartUs) && p.segStartUs[i] == us {
		return i
	}
	return -1
}

func (p *dvrPack) appendSeg(seg dvrPackSeg) {
	i := uint32(len(p.segStartUs))
	flags := uint8(0)
	if seg.Ready {
		flags |= dvrPackSegReady
	}
	if seg.Name != "" {
		flags |= dvrPackSegHasName
		if p.names == nil {
			p.names = make(map[uint32]string)
		}
		p.names[i] = seg.Name
	}
	p.segStartUs = append(p.segStartUs, seg.Start.UnixMicro())
	p.segDurMs = append(p.segDurMs, uint32(seg.Duration/time.Millisecond))
	p.segPTSEndMs = append(p.segPTSEndMs, uint32(seg.PTSEnd/time.Millisecond))
	p.segChunkAt = append(p.segChunkAt, uint32(len(p.chunkOff)))
	p.segChunkN = append(p.segChunkN, uint16(len(seg.Chunks)))
	p.segMoof = append(p.segMoof, seg.Moof)
	p.segCodecID = append(p.segCodecID, seg.CodecID)
	p.segFlags = append(p.segFlags, flags)

	for _, ch := range seg.Chunks {
		p.chunkOff = append(p.chunkOff, ch.Off)
		p.chunkDurMs = append(p.chunkDurMs, uint32(ch.Duration/time.Millisecond))
		p.chunkPTSEndMs = append(p.chunkPTSEndMs, uint32(ch.PTSEnd/time.Millisecond))
		p.chunkMoof = append(p.chunkMoof, ch.Moof)
	}
}

func encodeDvrPack(p *dvrPack) ([]byte, error) {
	nSeg, nChunk := p.segCount(), p.chunkCount()

	codecBlobs := make([][]byte, 0, len(p.codecs))
	for _, tracks := range p.codecs {
		blob, err := encodeTracks(tracks)
		if err != nil {
			return nil, err
		}
		codecBlobs = append(codecBlobs, blob)
	}

	nameKeys := make([]uint32, 0, len(p.names))
	for i := range p.names {
		nameKeys = append(nameKeys, i)
	}
	sort.Slice(nameKeys, func(a, b int) bool { return nameKeys[a] < nameKeys[b] })

	var first, last int64
	for i := 0; i < nSeg; i++ {
		if i == 0 || p.segStartUs[i] < first {
			first = p.segStartUs[i]
		}
		if end := p.segEndUs(i); end > last {
			last = end
		}
	}

	out := make([]byte, 0, dvrPackHeaderSize+
		len(p.ranges)*dvrPackRangeSize+nSeg*dvrPackSegSize+nChunk*dvrPackChunkSize+4)

	out = append(out, dvrPackMagic...)
	out = appendU16(out, dvrPackVersion)
	out = appendU16(out, 0)
	out = appendU64(out, p.hash)
	out = appendU64(out, uint64(p.dayUnix))
	out = appendU32(out, uint32(nSeg))
	out = appendU32(out, uint32(nChunk))
	out = appendU32(out, uint32(len(p.ranges)))
	out = appendU32(out, uint32(len(nameKeys)))
	out = appendU32(out, uint32(len(codecBlobs)))
	out = appendU64(out, uint64(first))
	out = appendU64(out, uint64(last))
	out = appendU32(out, 0)

	for _, r := range p.ranges {
		out = appendU64(out, uint64(r.From))
		out = appendU64(out, uint64(r.Duration))
	}

	base := p.dayUnix * 1e6
	for i := 0; i < nSeg; i++ {
		off := p.segStartUs[i] - base
		if off < 0 {
			off = 0
		}
		out = appendU32(out, uint32(off/1000))
		out = appendU32(out, p.segDurMs[i])
		out = appendU32(out, p.segPTSEndMs[i])
		out = appendU16(out, p.segChunkN[i])
		out = appendU16(out, p.segMoof[i])
		out = appendU16(out, uint16(off%1000))
		out = appendU8(out, p.segCodecID[i])
		out = appendU8(out, p.segFlags[i])
	}

	for i := 0; i < nChunk; i++ {
		out = appendU32(out, p.chunkOff[i])
		out = appendU32(out, p.chunkDurMs[i])
		out = appendU32(out, p.chunkPTSEndMs[i])
		out = appendU16(out, p.chunkMoof[i])
		out = appendU16(out, 0)
	}

	for _, blob := range codecBlobs {
		out = appendU32(out, uint32(len(blob)))
		out = append(out, blob...)
	}

	for _, i := range nameKeys {
		name := p.names[i]
		out = appendU32(out, i)
		out = appendU16(out, uint16(len(name)))
		out = append(out, name...)
	}

	return appendU32(out, crc32.ChecksumIEEE(out)), nil
}

// decodeDvrPackSummary reads only the head of a pack. Losing the per-camera
// head file costs one small read per day instead of a directory walk.
func decodeDvrPackSummary(buf []byte, hash uint64) (dvrPackSummary, error) {
	h, err := decodeDvrPackHeader(buf, dvrPackMagic, dvrPackHeaderSize, hash)
	if err != nil {
		return dvrPackSummary{}, err
	}
	out := dvrPackSummary{
		DayUnix:      h.dayUnix,
		NSeg:         h.nSeg,
		FirstStartUs: int64(binary.LittleEndian.Uint64(buf[44:52])),
		LastEndUs:    int64(binary.LittleEndian.Uint64(buf[52:60])),
	}
	need := dvrPackHeaderSize + int(h.nRanges)*dvrPackRangeSize
	if len(buf) < need {
		return dvrPackSummary{}, errDvrPackTrunc
	}
	out.Ranges = decodeDvrRanges(buf[dvrPackHeaderSize:need], int(h.nRanges))
	return out, nil
}

type dvrPackHeader struct {
	dayUnix  int64
	nSeg     uint32
	nChunk   uint32
	nRanges  uint32
	nNames   uint32
	nCodecs  uint32
	bodyFrom int
}

func decodeDvrPackHeader(buf []byte, magic string, size int, hash uint64) (dvrPackHeader, error) {
	if len(buf) < size {
		return dvrPackHeader{}, errDvrPackTrunc
	}
	if string(buf[0:4]) != magic {
		return dvrPackHeader{}, errDvrPackMagic
	}
	if binary.LittleEndian.Uint16(buf[4:6]) != dvrPackVersion {
		return dvrPackHeader{}, errDvrPackVersion
	}
	if binary.LittleEndian.Uint64(buf[8:16]) != hash {
		return dvrPackHeader{}, errDvrPackHash
	}
	h := dvrPackHeader{
		dayUnix:  int64(binary.LittleEndian.Uint64(buf[16:24])),
		bodyFrom: size,
	}
	if size == dvrPackHeaderSize {
		h.nSeg = binary.LittleEndian.Uint32(buf[24:28])
		h.nChunk = binary.LittleEndian.Uint32(buf[28:32])
		h.nRanges = binary.LittleEndian.Uint32(buf[32:36])
		h.nNames = binary.LittleEndian.Uint32(buf[36:40])
		h.nCodecs = binary.LittleEndian.Uint32(buf[40:44])
		if h.nSeg > dvrPackMaxSegs || h.nChunk > dvrPackMaxChunks ||
			h.nNames > h.nSeg || h.nRanges > dvrPackMaxSegs {
			return dvrPackHeader{}, errDvrPackTrunc
		}
	}
	return h, nil
}

func decodeDvrRanges(buf []byte, n int) []RecordingRange {
	if n == 0 {
		return nil
	}
	out := make([]RecordingRange, n)
	for i := 0; i < n; i++ {
		rec := buf[i*dvrPackRangeSize:]
		out[i] = RecordingRange{
			From:     int64(binary.LittleEndian.Uint64(rec[0:8])),
			Duration: int64(binary.LittleEndian.Uint64(rec[8:16])),
		}
	}
	return out
}

func decodeDvrPack(buf []byte, hash uint64) (*dvrPack, error) {
	h, err := decodeDvrPackHeader(buf, dvrPackMagic, dvrPackHeaderSize, hash)
	if err != nil {
		return nil, err
	}
	if len(buf) < 4 || crc32.ChecksumIEEE(buf[:len(buf)-4]) !=
		binary.LittleEndian.Uint32(buf[len(buf)-4:]) {
		return nil, errDvrPackCRC
	}

	nSeg, nChunk := int(h.nSeg), int(h.nChunk)
	p := &dvrPack{
		hash:          hash,
		dayUnix:       h.dayUnix,
		segStartUs:    make([]int64, nSeg),
		segDurMs:      make([]uint32, nSeg),
		segPTSEndMs:   make([]uint32, nSeg),
		segChunkAt:    make([]uint32, nSeg),
		segChunkN:     make([]uint16, nSeg),
		segMoof:       make([]uint16, nSeg),
		segCodecID:    make([]uint8, nSeg),
		segFlags:      make([]uint8, nSeg),
		chunkOff:      make([]uint32, nChunk),
		chunkDurMs:    make([]uint32, nChunk),
		chunkPTSEndMs: make([]uint32, nChunk),
		chunkMoof:     make([]uint16, nChunk),
	}

	at := h.bodyFrom
	end := at + int(h.nRanges)*dvrPackRangeSize
	if len(buf) < end {
		return nil, errDvrPackTrunc
	}
	p.ranges = decodeDvrRanges(buf[at:end], int(h.nRanges))
	at = end

	if end = at + nSeg*dvrPackSegSize; len(buf) < end {
		return nil, errDvrPackTrunc
	}
	base := h.dayUnix * 1e6
	chunkAt := uint32(0)
	for i := 0; i < nSeg; i++ {
		rec := buf[at+i*dvrPackSegSize:]
		ms := int64(binary.LittleEndian.Uint32(rec[0:4]))
		p.segDurMs[i] = binary.LittleEndian.Uint32(rec[4:8])
		p.segPTSEndMs[i] = binary.LittleEndian.Uint32(rec[8:12])
		p.segChunkN[i] = binary.LittleEndian.Uint16(rec[12:14])
		p.segMoof[i] = binary.LittleEndian.Uint16(rec[14:16])
		p.segStartUs[i] = base + ms*1000 + int64(binary.LittleEndian.Uint16(rec[16:18]))
		p.segCodecID[i] = rec[18]
		p.segFlags[i] = rec[19]
		p.segChunkAt[i] = chunkAt
		chunkAt += uint32(p.segChunkN[i])
	}
	if int(chunkAt) != nChunk {
		return nil, errDvrPackTrunc
	}
	at = end

	if end = at + nChunk*dvrPackChunkSize; len(buf) < end {
		return nil, errDvrPackTrunc
	}
	for i := 0; i < nChunk; i++ {
		rec := buf[at+i*dvrPackChunkSize:]
		p.chunkOff[i] = binary.LittleEndian.Uint32(rec[0:4])
		p.chunkDurMs[i] = binary.LittleEndian.Uint32(rec[4:8])
		p.chunkPTSEndMs[i] = binary.LittleEndian.Uint32(rec[8:12])
		p.chunkMoof[i] = binary.LittleEndian.Uint16(rec[12:14])
	}
	at = end

	for i := uint32(0); i < h.nCodecs; i++ {
		if len(buf) < at+4 {
			return nil, errDvrPackTrunc
		}
		n := int(binary.LittleEndian.Uint32(buf[at : at+4]))
		at += 4
		if n < 0 || len(buf) < at+n {
			return nil, errDvrPackTrunc
		}
		tracks, err := decodeTracks(buf[at : at+n])
		if err != nil {
			return nil, err
		}
		p.codecs = append(p.codecs, tracks)
		at += n
	}

	for i := uint32(0); i < h.nNames; i++ {
		if len(buf) < at+6 {
			return nil, errDvrPackTrunc
		}
		idx := binary.LittleEndian.Uint32(buf[at : at+4])
		n := int(binary.LittleEndian.Uint16(buf[at+4 : at+6]))
		at += 6
		if len(buf) < at+n || int(idx) >= nSeg {
			return nil, errDvrPackTrunc
		}
		if p.names == nil {
			p.names = make(map[uint32]string, h.nNames)
		}
		p.names[idx] = string(buf[at : at+n])
		at += n
	}

	return p, nil
}

// dvrWalBuilder replays an open-day journal. Only today's day goes through it;
// sealed days decode straight into columns.
type dvrWalBuilder struct {
	hash    uint64
	dayUnix int64
	segs    []dvrPackSeg
	byStart map[int64]int
	codecs  [][]*fmp4.InitTrack
}

func newDvrWalBuilder(hash uint64, dayUnix int64) *dvrWalBuilder {
	return &dvrWalBuilder{hash: hash, dayUnix: dayUnix, byStart: map[int64]int{}}
}

func (b *dvrWalBuilder) upsert(seg dvrPackSeg) {
	us := seg.Start.UnixMicro()
	if i, ok := b.byStart[us]; ok {
		b.segs[i] = seg
		return
	}
	b.byStart[us] = len(b.segs)
	b.segs = append(b.segs, seg)
}

func (b *dvrWalBuilder) delete(start time.Time) {
	us := start.UnixMicro()
	i, ok := b.byStart[us]
	if !ok {
		return
	}
	b.segs = append(b.segs[:i], b.segs[i+1:]...)
	delete(b.byStart, us)
	for j := i; j < len(b.segs); j++ {
		b.byStart[b.segs[j].Start.UnixMicro()] = j
	}
}

func (b *dvrWalBuilder) setCodec(id uint8, tracks []*fmp4.InitTrack) {
	if id == 0 {
		return
	}
	for len(b.codecs) < int(id) {
		b.codecs = append(b.codecs, nil)
	}
	b.codecs[id-1] = tracks
}

// build sorts the replayed records into a pack. gap is the largest hole that
// still counts as continuous recording when deriving ranges.
func (b *dvrWalBuilder) build(gap time.Duration) *dvrPack {
	sort.SliceStable(b.segs, func(i, j int) bool { return b.segs[i].Start.Before(b.segs[j].Start) })
	p := &dvrPack{hash: b.hash, dayUnix: b.dayUnix, codecs: b.codecs}
	for _, seg := range b.segs {
		p.appendSeg(seg)
	}
	p.ranges = dvrPackRanges(p, gap)
	return p
}

func dvrPackRanges(p *dvrPack, gap time.Duration) []RecordingRange {
	var out []RecordingRange
	for i := 0; i < p.segCount(); i++ {
		from := p.segStartUs[i] / 1e6
		to := p.segEndUs(i) / 1e6
		if n := len(out); n > 0 {
			prevEnd := out[n-1].From + out[n-1].Duration
			if from-prevEnd <= int64(gap/time.Second) {
				if to > prevEnd {
					out[n-1].Duration = to - out[n-1].From
				}
				continue
			}
		}
		out = append(out, RecordingRange{From: from, Duration: to - from})
	}
	return out
}

func encodeDvrWalHeader(hash uint64, dayUnix int64) []byte {
	out := make([]byte, 0, dvrWalHeaderSize)
	out = append(out, dvrWalMagic...)
	out = appendU16(out, dvrPackVersion)
	out = appendU16(out, 0)
	out = appendU64(out, hash)
	out = appendU64(out, uint64(dayUnix))
	return appendU64(out, 0)
}

func dvrWalFrame(op uint8, payload []byte) []byte {
	out := make([]byte, 0, 3+len(payload)+4)
	out = appendU8(out, op)
	out = appendU16(out, uint16(len(payload)))
	out = append(out, payload...)
	return appendU32(out, crc32.ChecksumIEEE(out))
}

func encodeDvrWalSeg(seg dvrPackSeg, dayUnix int64) []byte {
	off := seg.Start.UnixMicro() - dayUnix*1e6
	if off < 0 {
		off = 0
	}
	flags := uint8(0)
	if seg.Ready {
		flags |= dvrPackSegReady
	}
	if seg.Name != "" {
		flags |= dvrPackSegHasName
	}

	payload := make([]byte, 0, dvrPackSegSize+len(seg.Chunks)*dvrPackChunkSize+2+len(seg.Name))
	payload = appendU32(payload, uint32(off/1000))
	payload = appendU32(payload, uint32(seg.Duration/time.Millisecond))
	payload = appendU32(payload, uint32(seg.PTSEnd/time.Millisecond))
	payload = appendU16(payload, uint16(len(seg.Chunks)))
	payload = appendU16(payload, seg.Moof)
	payload = appendU16(payload, uint16(off%1000))
	payload = appendU8(payload, seg.CodecID)
	payload = appendU8(payload, flags)
	for _, ch := range seg.Chunks {
		payload = appendU32(payload, ch.Off)
		payload = appendU32(payload, uint32(ch.Duration/time.Millisecond))
		payload = appendU32(payload, uint32(ch.PTSEnd/time.Millisecond))
		payload = appendU16(payload, ch.Moof)
		payload = appendU16(payload, 0)
	}
	if seg.Name != "" {
		payload = appendU16(payload, uint16(len(seg.Name)))
		payload = append(payload, seg.Name...)
	}
	return dvrWalFrame(dvrWalOpSeg, payload)
}

func encodeDvrWalDelete(start time.Time, dayUnix int64) []byte {
	off := start.UnixMicro() - dayUnix*1e6
	if off < 0 {
		off = 0
	}
	payload := appendU32(nil, uint32(off/1000))
	payload = appendU16(payload, uint16(off%1000))
	return dvrWalFrame(dvrWalOpDelete, payload)
}

func encodeDvrWalCodec(id uint8, tracks []*fmp4.InitTrack) ([]byte, error) {
	blob, err := encodeTracks(tracks)
	if err != nil {
		return nil, err
	}
	payload := appendU8(nil, id)
	payload = append(payload, blob...)
	return dvrWalFrame(dvrWalOpCodec, payload), nil
}

func decodeDvrWalSeg(payload []byte, dayUnix int64) (dvrPackSeg, error) {
	if len(payload) < dvrPackSegSize {
		return dvrPackSeg{}, errDvrPackTrunc
	}
	ms := int64(binary.LittleEndian.Uint32(payload[0:4]))
	nChunk := int(binary.LittleEndian.Uint16(payload[12:14]))
	flags := payload[19]
	seg := dvrPackSeg{
		Start: time.UnixMicro(dayUnix*1e6 + ms*1000 +
			int64(binary.LittleEndian.Uint16(payload[16:18]))),
		Duration: time.Duration(binary.LittleEndian.Uint32(payload[4:8])) * time.Millisecond,
		PTSEnd:   time.Duration(binary.LittleEndian.Uint32(payload[8:12])) * time.Millisecond,
		Moof:     binary.LittleEndian.Uint16(payload[14:16]),
		CodecID:  payload[18],
		Ready:    flags&dvrPackSegReady != 0,
	}

	at := dvrPackSegSize
	if len(payload) < at+nChunk*dvrPackChunkSize {
		return dvrPackSeg{}, errDvrPackTrunc
	}
	if nChunk > 0 {
		seg.Chunks = make([]dvrPackChunk, nChunk)
		for i := 0; i < nChunk; i++ {
			rec := payload[at+i*dvrPackChunkSize:]
			seg.Chunks[i] = dvrPackChunk{
				Off:      binary.LittleEndian.Uint32(rec[0:4]),
				Duration: time.Duration(binary.LittleEndian.Uint32(rec[4:8])) * time.Millisecond,
				PTSEnd:   time.Duration(binary.LittleEndian.Uint32(rec[8:12])) * time.Millisecond,
				Moof:     binary.LittleEndian.Uint16(rec[12:14]),
			}
		}
	}
	at += nChunk * dvrPackChunkSize

	if flags&dvrPackSegHasName != 0 {
		if len(payload) < at+2 {
			return dvrPackSeg{}, errDvrPackTrunc
		}
		n := int(binary.LittleEndian.Uint16(payload[at : at+2]))
		at += 2
		if len(payload) < at+n {
			return dvrPackSeg{}, errDvrPackTrunc
		}
		seg.Name = string(payload[at : at+n])
	}
	return seg, nil
}

// decodeDvrWal replays a journal and reports how many leading bytes were
// intact. A crash truncates the tail mid-frame: everything before the first
// bad frame is kept, and the caller truncates the file to good.
func decodeDvrWal(buf []byte, hash uint64, gap time.Duration) (p *dvrPack, good int, err error) {
	h, err := decodeDvrPackHeader(buf, dvrWalMagic, dvrWalHeaderSize, hash)
	if err != nil {
		return nil, 0, err
	}

	b := newDvrWalBuilder(hash, h.dayUnix)
	at := dvrWalHeaderSize
	for at+3+4 <= len(buf) {
		n := int(binary.LittleEndian.Uint16(buf[at+1 : at+3]))
		frameEnd := at + 3 + n + 4
		if frameEnd > len(buf) {
			break
		}
		if crc32.ChecksumIEEE(buf[at:frameEnd-4]) !=
			binary.LittleEndian.Uint32(buf[frameEnd-4:frameEnd]) {
			break
		}
		payload := buf[at+3 : at+3+n]

		switch buf[at] {
		case dvrWalOpSeg:
			seg, err2 := decodeDvrWalSeg(payload, h.dayUnix)
			if err2 != nil {
				return b.build(gap), at, nil
			}
			b.upsert(seg)
		case dvrWalOpDelete:
			if len(payload) < 6 {
				return b.build(gap), at, nil
			}
			ms := int64(binary.LittleEndian.Uint32(payload[0:4]))
			rem := int64(binary.LittleEndian.Uint16(payload[4:6]))
			b.delete(time.UnixMicro(h.dayUnix*1e6 + ms*1000 + rem))
		case dvrWalOpCodec:
			if len(payload) < 1 {
				return b.build(gap), at, nil
			}
			tracks, err2 := decodeTracks(payload[1:])
			if err2 != nil {
				return b.build(gap), at, nil
			}
			b.setCodec(payload[0], tracks)
		default:
			return b.build(gap), at, nil
		}
		at = frameEnd
	}
	return b.build(gap), at, nil
}
