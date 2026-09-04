package compatapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recordstore"
)

const (
	dvrSnapMagic     = "MTXI"
	dvrJournalMagic  = "MTXJ"
	dvrMetaMagic     = "MTXM"
	dvrIndexVersion  = uint16(1)
	dvrSnapName      = ".mtx-dvr-index"
	dvrMetaName      = ".mtx-dvr-meta"
	dvrJournalSuffix = ".journal"
	dvrOpUpsert      = uint8(1)
	dvrOpDelete      = uint8(2)
	dvrOpCodec       = uint8(3)
	dvrSegReady      = uint8(1)
	dvrCompactEvery  = 512
	dayCacheLimit    = 8
)

var errDvrIndexBadMagic = errors.New("dvr index: bad magic")

type dvrSegRec struct {
	Rel      string
	Start    time.Time
	Duration time.Duration
	Moof     uint32
	CodecID  uint8
	Ready    bool
}

type dvrSnapshot struct {
	Hash   uint64
	Codecs [][]*fmp4.InitTrack
	Segs   []dvrSegRec
}

type dvrJournalOp struct {
	Op      uint8
	CodecID uint8
	Tracks  []*fmp4.InitTrack
	Seg     dvrSegRec
}

type dvrPersist struct {
	hash        uint64
	snapPath    string
	journalPath string
	journal     *os.File
	journalOps  int
	ready       bool
	savedCodec  map[uint8]struct{}
}

func dvrIndexHash(pathConf *conf.Path, pathName string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pathConf.RecordPath))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(pathConf.RecordFormat))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(pathName))
	// ranges-v2: do not treat next-file delta up to 3× segmentDuration as
	// contiguous (that hid archive holes on round-robin storage disks).
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte("ranges-v2"))
	if pathConf.Storage != "" {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(pathConf.Storage))
		for _, d := range pathConf.StorageDisks {
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(d))
		}
	}
	return h.Sum64()
}

func sanitizeIndexName(pathName string) string {
	var b strings.Builder
	for _, r := range pathName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "path"
	}
	return b.String()
}

type dvrDayInfo struct {
	Date string
	NSeg uint32
}

type dvrMeta struct {
	Hash   uint64
	Ranges []RecordingRange
	Days   []dvrDayInfo
}

type dvrPathLayout struct {
	common  string
	meta    string
	dateDir bool
	fileTag string
}

func recordPathHasDateDir(recordPath string) bool {
	for _, part := range strings.Split(filepath.ToSlash(recordPath), "/") {
		if part == "%Y-%m-%d" {
			return true
		}
	}
	return false
}

func dvrDayDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format("2006-01-02")
}

func isDayDirName(name string) bool {
	if len(name) != 10 || name[4] != '-' || name[7] != '-' {
		return false
	}
	_, err := time.ParseInLocation("2006-01-02", name, time.Local)
	return err == nil
}

func makeDvrLayoutFrom(recordPathTpl string, pathConf *conf.Path, pathName string) dvrPathLayout {
	recordPath := recordstore.PathAddExtension(
		strings.ReplaceAll(recordPathTpl, "%path", pathName),
		pathConf.RecordFormat,
	)
	recordPath, _ = filepath.Abs(recordPath)
	common := recordstore.CommonPath(recordPath)
	l := dvrPathLayout{
		common:  common,
		dateDir: recordPathHasDateDir(recordPathTpl),
	}
	if filepath.Base(common) != pathName && filepath.Base(common) != filepath.Base(pathName) {
		l.fileTag = "." + sanitizeIndexName(pathName)
	}
	l.meta = filepath.Join(common, dvrMetaName+l.fileTag)
	return l
}

func (l dvrPathLayout) walkRoot(pathName string) string {
	if l.common == "" {
		return ""
	}
	if pathName == "" || strings.Contains(pathName, "..") {
		return l.common
	}
	sub := filepath.Join(l.common, filepath.FromSlash(pathName))
	if info, err := os.Stat(sub); err == nil && info.IsDir() {
		return sub
	}
	return l.common
}

func makeDvrLayout(pathConf *conf.Path, pathName string) dvrPathLayout {
	layouts := makeDvrLayouts(pathConf, pathName)
	if len(layouts) == 0 {
		return dvrPathLayout{}
	}
	return layouts[0]
}

func makeDvrLayouts(pathConf *conf.Path, pathName string) []dvrPathLayout {
	if pathConf == nil {
		return nil
	}
	formats := pathConf.RecordPathFormats()
	out := make([]dvrPathLayout, 0, len(formats))
	for _, raw := range formats {
		out = append(out, makeDvrLayoutFrom(raw, pathConf, pathName))
	}
	return out
}

func layoutForFpath(layouts []dvrPathLayout, fpath string) dvrPathLayout {
	if len(layouts) == 0 {
		return dvrPathLayout{}
	}
	if fpath == "" {
		return layouts[0]
	}
	abs, err := filepath.Abs(fpath)
	if err != nil {
		abs = fpath
	}
	for _, l := range layouts {
		if l.common == "" {
			continue
		}
		commonAbs, err := filepath.Abs(l.common)
		if err != nil {
			commonAbs = l.common
		}
		rel, err := filepath.Rel(commonAbs, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return l
	}
	return layouts[0]
}

func mergeDayInfosSum(dst, src []dvrDayInfo) []dvrDayInfo {
	by := make(map[string]uint32, len(dst)+len(src))
	for _, d := range dst {
		by[d.Date] += d.NSeg
	}
	for _, d := range src {
		by[d.Date] += d.NSeg
	}
	if len(by) == 0 {
		return nil
	}
	out := make([]dvrDayInfo, 0, len(by))
	for date, n := range by {
		out = append(out, dvrDayInfo{Date: date, NSeg: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

func (l dvrPathLayout) daySnap(day string) string {
	if l.dateDir {
		return filepath.Join(l.common, day, dvrSnapName+l.fileTag)
	}
	return filepath.Join(l.common, dvrSnapName+l.fileTag+"."+day)
}

func (l dvrPathLayout) dayJournal(day string) string {
	return l.daySnap(day) + dvrJournalSuffix
}

func (l dvrPathLayout) removeLegacyMonolith() {
	if l.common == "" {
		return
	}
	base := filepath.Join(l.common, dvrSnapName+l.fileTag)
	_ = os.Remove(base)
	_ = os.Remove(base + dvrJournalSuffix)
}

func isDvrIndexFile(name string) bool {
	return name == dvrSnapName || strings.HasPrefix(name, dvrSnapName) ||
		name == dvrMetaName || strings.HasPrefix(name, dvrMetaName)
}

func dvrIndexPaths(pathConf *conf.Path, pathName string) (snapPath, journalPath string, common string) {
	l := makeDvrLayout(pathConf, pathName)
	day := dvrDayDate(time.Now())
	return l.daySnap(day), l.dayJournal(day), l.common
}

func dvrRelPath(common, fpath string) string {
	absCommon, err := filepath.Abs(common)
	if err != nil {
		return filepath.ToSlash(filepath.Base(fpath))
	}
	absFpath, err := filepath.Abs(fpath)
	if err != nil {
		return filepath.ToSlash(filepath.Base(fpath))
	}
	rel, err := filepath.Rel(absCommon, absFpath)
	if err != nil {
		return filepath.ToSlash(filepath.Base(fpath))
	}
	return filepath.ToSlash(rel)
}

func dvrAbsPath(common, rel string) string {
	return filepath.Join(common, filepath.FromSlash(rel))
}

func encodeTracks(tracks []*fmp4.InitTrack) ([]byte, error) {
	if len(tracks) == 0 {
		return nil, nil
	}
	var buf seekablebuffer.Buffer
	err := (fmp4.Init{Tracks: tracks}).Marshal(&buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeTracks(blob []byte) ([]*fmp4.InitTrack, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	var init fmp4.Init
	err := init.Unmarshal(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	return init.Tracks, nil
}

func appendU8(dst []byte, v uint8) []byte { return append(dst, v) }
func appendU16(dst []byte, v uint16) []byte {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	return append(dst, tmp[:]...)
}
func appendU32(dst []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(dst, tmp[:]...)
}
func appendU64(dst []byte, v uint64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(dst, tmp[:]...)
}

func encodeSegRec(dst []byte, rec dvrSegRec) []byte {
	flags := uint8(0)
	if rec.Ready {
		flags = dvrSegReady
	}
	dst = appendU8(dst, flags)
	dst = appendU64(dst, uint64(rec.Start.UnixNano()))
	ms := rec.Duration.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if ms > int64(^uint32(0)) {
		ms = int64(^uint32(0))
	}
	dst = appendU32(dst, uint32(ms))
	dst = appendU32(dst, rec.Moof)
	dst = appendU8(dst, rec.CodecID)
	rel := rec.Rel
	if len(rel) > 0xFFFF {
		rel = rel[:0xFFFF]
	}
	dst = appendU16(dst, uint16(len(rel)))
	dst = append(dst, rel...)
	return dst
}

func decodeSegRec(src []byte) (dvrSegRec, int, error) {
	need := 1 + 8 + 4 + 4 + 1 + 2
	if len(src) < need {
		return dvrSegRec{}, 0, io.ErrUnexpectedEOF
	}
	var rec dvrSegRec
	rec.Ready = src[0]&dvrSegReady != 0
	n := 1
	rec.Start = time.Unix(0, int64(binary.LittleEndian.Uint64(src[n:n+8]))).UTC()
	n += 8
	rec.Duration = time.Duration(binary.LittleEndian.Uint32(src[n:n+4])) * time.Millisecond
	n += 4
	rec.Moof = binary.LittleEndian.Uint32(src[n : n+4])
	n += 4
	rec.CodecID = src[n]
	n++
	relLen := int(binary.LittleEndian.Uint16(src[n : n+2]))
	n += 2
	if len(src) < n+relLen {
		return dvrSegRec{}, 0, io.ErrUnexpectedEOF
	}
	rec.Rel = string(src[n : n+relLen])
	n += relLen
	return rec, n, nil
}

func encodeSnapshot(s dvrSnapshot) ([]byte, error) {
	out := make([]byte, 0, 64+len(s.Segs)*48)
	out = append(out, dvrSnapMagic...)
	out = appendU16(out, dvrIndexVersion)
	out = appendU64(out, s.Hash)
	if len(s.Codecs) > 255 {
		s.Codecs = s.Codecs[:255]
	}
	out = appendU8(out, uint8(len(s.Codecs)))
	for _, tracks := range s.Codecs {
		blob, err := encodeTracks(tracks)
		if err != nil {
			return nil, err
		}
		out = appendU32(out, uint32(len(blob)))
		out = append(out, blob...)
	}
	out = appendU32(out, uint32(len(s.Segs)))
	for _, rec := range s.Segs {
		out = encodeSegRec(out, rec)
	}
	sum := crc32.ChecksumIEEE(out)
	out = appendU32(out, sum)
	return out, nil
}

func decodeSnapshot(data []byte) (dvrSnapshot, error) {
	if len(data) < 4+2+8+1+4+4 {
		return dvrSnapshot{}, io.ErrUnexpectedEOF
	}
	body, crcBytes := data[:len(data)-4], data[len(data)-4:]
	want := binary.LittleEndian.Uint32(crcBytes)
	if crc32.ChecksumIEEE(body) != want {
		return dvrSnapshot{}, errors.New("dvr index: snapshot crc mismatch")
	}
	if string(body[:4]) != dvrSnapMagic {
		return dvrSnapshot{}, errDvrIndexBadMagic
	}
	n := 4
	ver := binary.LittleEndian.Uint16(body[n : n+2])
	n += 2
	if ver != dvrIndexVersion {
		return dvrSnapshot{}, errors.New("dvr index: unsupported version")
	}
	var s dvrSnapshot
	s.Hash = binary.LittleEndian.Uint64(body[n : n+8])
	n += 8
	nCodec := int(body[n])
	n++
	s.Codecs = make([][]*fmp4.InitTrack, nCodec)
	for i := 0; i < nCodec; i++ {
		if n+4 > len(body) {
			return dvrSnapshot{}, io.ErrUnexpectedEOF
		}
		blobLen := int(binary.LittleEndian.Uint32(body[n : n+4]))
		n += 4
		if n+blobLen > len(body) {
			return dvrSnapshot{}, io.ErrUnexpectedEOF
		}
		tracks, err := decodeTracks(body[n : n+blobLen])
		if err != nil {
			return dvrSnapshot{}, err
		}
		s.Codecs[i] = tracks
		n += blobLen
	}
	if n+4 > len(body) {
		return dvrSnapshot{}, io.ErrUnexpectedEOF
	}
	nSeg := int(binary.LittleEndian.Uint32(body[n : n+4]))
	n += 4
	s.Segs = make([]dvrSegRec, 0, nSeg)
	for i := 0; i < nSeg; i++ {
		rec, used, err := decodeSegRec(body[n:])
		if err != nil {
			return dvrSnapshot{}, err
		}
		s.Segs = append(s.Segs, rec)
		n += used
	}
	return s, nil
}

func readSnapshotFile(path string) (dvrSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return dvrSnapshot{}, err
	}
	return decodeSnapshot(data)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func writeSnapshotFile(path string, s dvrSnapshot) error {
	data, err := encodeSnapshot(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func encodeMeta(m dvrMeta) []byte {
	out := append([]byte(dvrMetaMagic), 0, 0)
	binary.LittleEndian.PutUint16(out[4:], dvrIndexVersion)
	out = appendU64(out, m.Hash)
	out = appendU32(out, uint32(len(m.Ranges)))
	for _, r := range m.Ranges {
		out = appendU64(out, uint64(r.From))
		out = appendU64(out, uint64(r.Duration))
	}
	out = appendU32(out, uint32(len(m.Days)))
	for _, d := range m.Days {
		date := d.Date
		if len(date) > 10 {
			date = date[:10]
		}
		for len(date) < 10 {
			date += " "
		}
		out = append(out, date...)
		out = appendU32(out, d.NSeg)
	}
	sum := crc32.ChecksumIEEE(out)
	return appendU32(out, sum)
}

func decodeMeta(data []byte) (dvrMeta, error) {
	if len(data) < 4+2+8+4+4+4 {
		return dvrMeta{}, io.ErrUnexpectedEOF
	}
	body, crcBytes := data[:len(data)-4], data[len(data)-4:]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(crcBytes) {
		return dvrMeta{}, errors.New("dvr meta: crc mismatch")
	}
	if string(body[:4]) != dvrMetaMagic {
		return dvrMeta{}, errDvrIndexBadMagic
	}
	n := 4
	ver := binary.LittleEndian.Uint16(body[n : n+2])
	n += 2
	if ver != dvrIndexVersion {
		return dvrMeta{}, errors.New("dvr meta: unsupported version")
	}
	var m dvrMeta
	m.Hash = binary.LittleEndian.Uint64(body[n : n+8])
	n += 8
	nRange := int(binary.LittleEndian.Uint32(body[n : n+4]))
	n += 4
	if n+nRange*16 > len(body) {
		return dvrMeta{}, io.ErrUnexpectedEOF
	}
	m.Ranges = make([]RecordingRange, 0, nRange)
	for i := 0; i < nRange; i++ {
		from := int64(binary.LittleEndian.Uint64(body[n : n+8]))
		n += 8
		dur := int64(binary.LittleEndian.Uint64(body[n : n+8]))
		n += 8
		m.Ranges = append(m.Ranges, RecordingRange{From: from, Duration: dur})
	}
	if n+4 > len(body) {
		return dvrMeta{}, io.ErrUnexpectedEOF
	}
	nDays := int(binary.LittleEndian.Uint32(body[n : n+4]))
	n += 4
	if n+nDays*14 > len(body) {
		return dvrMeta{}, io.ErrUnexpectedEOF
	}
	m.Days = make([]dvrDayInfo, 0, nDays)
	for i := 0; i < nDays; i++ {
		date := strings.TrimSpace(string(body[n : n+10]))
		n += 10
		nseg := binary.LittleEndian.Uint32(body[n : n+4])
		n += 4
		m.Days = append(m.Days, dvrDayInfo{Date: date, NSeg: nseg})
	}
	return m, nil
}

func readMetaFile(path string) (dvrMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return dvrMeta{}, err
	}
	return decodeMeta(data)
}

func writeMetaFile(path string, m dvrMeta) error {
	return writeFileAtomic(path, encodeMeta(m))
}

func encodeJournalOp(op dvrJournalOp) ([]byte, error) {
	payload := []byte{op.Op}
	switch op.Op {
	case dvrOpCodec:
		blob, err := encodeTracks(op.Tracks)
		if err != nil {
			return nil, err
		}
		payload = appendU8(payload, op.CodecID)
		payload = appendU32(payload, uint32(len(blob)))
		payload = append(payload, blob...)
	case dvrOpUpsert:
		payload = encodeSegRec(payload, op.Seg)
	case dvrOpDelete:
		rel := op.Seg.Rel
		if len(rel) > 0xFFFF {
			rel = rel[:0xFFFF]
		}
		payload = appendU16(payload, uint16(len(rel)))
		payload = append(payload, rel...)
	default:
		return nil, errors.New("dvr index: unknown journal op")
	}
	sum := crc32.ChecksumIEEE(payload)
	out := appendU32(nil, uint32(len(payload)))
	out = append(out, payload...)
	out = appendU32(out, sum)
	return out, nil
}

func decodeJournalOp(payload []byte) (dvrJournalOp, error) {
	if len(payload) < 1 {
		return dvrJournalOp{}, io.ErrUnexpectedEOF
	}
	op := dvrJournalOp{Op: payload[0]}
	rest := payload[1:]
	switch op.Op {
	case dvrOpCodec:
		if len(rest) < 1+4 {
			return dvrJournalOp{}, io.ErrUnexpectedEOF
		}
		op.CodecID = rest[0]
		blobLen := int(binary.LittleEndian.Uint32(rest[1:5]))
		if len(rest) < 5+blobLen {
			return dvrJournalOp{}, io.ErrUnexpectedEOF
		}
		tracks, err := decodeTracks(rest[5 : 5+blobLen])
		if err != nil {
			return dvrJournalOp{}, err
		}
		op.Tracks = tracks
	case dvrOpUpsert:
		rec, _, err := decodeSegRec(rest)
		if err != nil {
			return dvrJournalOp{}, err
		}
		op.Seg = rec
	case dvrOpDelete:
		if len(rest) < 2 {
			return dvrJournalOp{}, io.ErrUnexpectedEOF
		}
		relLen := int(binary.LittleEndian.Uint16(rest[:2]))
		if len(rest) < 2+relLen {
			return dvrJournalOp{}, io.ErrUnexpectedEOF
		}
		op.Seg.Rel = string(rest[2 : 2+relLen])
	default:
		return dvrJournalOp{}, errors.New("dvr index: unknown journal op")
	}
	return op, nil
}

func journalHeader(hash uint64) []byte {
	out := append([]byte(dvrJournalMagic), 0, 0)
	binary.LittleEndian.PutUint16(out[4:], dvrIndexVersion)
	out = appendU64(out, hash)
	return out
}

func readJournalFile(path string, wantHash uint64) ([]dvrJournalOp, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	hdr := make([]byte, 4+2+8)
	_, err = io.ReadFull(f, hdr)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil
		}
		return nil, err
	}
	if string(hdr[:4]) != dvrJournalMagic {
		return nil, errDvrIndexBadMagic
	}
	ver := binary.LittleEndian.Uint16(hdr[4:6])
	if ver != dvrIndexVersion {
		return nil, errors.New("dvr index: unsupported journal version")
	}
	hash := binary.LittleEndian.Uint64(hdr[6:14])
	if hash != wantHash {
		return nil, errors.New("dvr index: journal hash mismatch")
	}

	var ops []dvrJournalOp
	lenBuf := make([]byte, 4)
	for {
		_, err = io.ReadFull(f, lenBuf)
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		n := int(binary.LittleEndian.Uint32(lenBuf))
		if n <= 0 || n > 16<<20 {
			break
		}
		payload := make([]byte, n)
		_, err = io.ReadFull(f, payload)
		if err != nil {
			break
		}
		crcBuf := make([]byte, 4)
		_, err = io.ReadFull(f, crcBuf)
		if err != nil {
			break
		}
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(crcBuf) {
			break
		}
		op, err := decodeJournalOp(payload)
		if err != nil {
			break
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *dvrPersist) closeJournal() {
	if p != nil && p.journal != nil {
		_ = p.journal.Sync()
		_ = p.journal.Close()
		p.journal = nil
	}
}

func (p *dvrPersist) openJournalAppend() error {
	if p == nil {
		return nil
	}
	p.closeJournal()
	if err := os.MkdirAll(filepath.Dir(p.journalPath), 0o755); err != nil {
		return err
	}
	fi, err := os.Stat(p.journalPath)
	createHdr := err != nil || fi.Size() == 0
	f, err := os.OpenFile(p.journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if createHdr {
		if _, err = f.Write(journalHeader(p.hash)); err != nil {
			f.Close()
			return err
		}
	}
	p.journal = f
	p.ready = true
	if p.savedCodec == nil {
		p.savedCodec = make(map[uint8]struct{})
	}
	return nil
}

func (p *dvrPersist) truncateJournal() error {
	if p == nil {
		return nil
	}
	p.closeJournal()
	if err := os.MkdirAll(filepath.Dir(p.journalPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p.journalPath)
	if err != nil {
		return err
	}
	if _, err = f.Write(journalHeader(p.hash)); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	p.journal = f
	p.journalOps = 0
	p.ready = true
	p.savedCodec = make(map[uint8]struct{})
	return nil
}

func (p *dvrPersist) writeOp(op dvrJournalOp) error {
	if p == nil || p.journal == nil {
		return nil
	}
	raw, err := encodeJournalOp(op)
	if err != nil {
		return err
	}
	_, err = p.journal.Write(raw)
	if err != nil {
		return err
	}
	p.journalOps++
	return nil
}

func newDvrPersist(pathConf *conf.Path, pathName string) *dvrPersist {
	return &dvrPersist{
		hash:       dvrIndexHash(pathConf, pathName),
		savedCodec: make(map[uint8]struct{}),
	}
}

func (p *dvrPersist) bindDay(l dvrPathLayout, day string) {
	if p == nil || day == "" {
		return
	}
	nextSnap := l.daySnap(day)
	if p.snapPath == nextSnap && p.ready {
		return
	}
	p.closeJournal()
	p.snapPath = nextSnap
	p.journalPath = l.dayJournal(day)
	p.ready = false
	p.journalOps = 0
	p.savedCodec = make(map[uint8]struct{})
}
