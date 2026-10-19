package eventstore

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

const (
	fileMagic   = "MTXE"
	fileVersion = uint16(1)

	headerSize = 16

	OpPoint uint8 = 1
	OpStart uint8 = 2
	OpEnd   uint8 = 3

	ClassMotion uint8 = 1
	ClassLine   uint8 = 2
	ClassField  uint8 = 3
	ClassInput  uint8 = 4
	ClassTamper uint8 = 5
	ClassOther  uint8 = 6
)

// Item is one ONVIF simple item kept with the event.
type Item struct {
	Name  string
	Value string
}

// Record is one append-only journal entry.
type Record struct {
	Recv   time.Time
	Camera time.Time
	Match  time.Time
	Op     uint8
	Class  uint8
	Topic  string
	Source string
	Data   []Item
}

// Span is a timeline interval. A point has Point set and a zero duration.
type Span struct {
	From   time.Time
	To     time.Time
	Point  bool
	Class  string
	Topic  string
	Source string
}

func className(c uint8) string {
	switch c {
	case ClassMotion:
		return "motion"
	case ClassLine:
		return "line"
	case ClassField:
		return "field"
	case ClassInput:
		return "input"
	case ClassTamper:
		return "tamper"
	default:
		return "other"
	}
}

func encodeRecord(rec Record) ([]byte, error) {
	topic := []byte(rec.Topic)
	source := []byte(rec.Source)
	if len(topic) > 0xFFFF || len(source) > 0xFFFF || len(rec.Data) > 0xFFFF {
		return nil, fmt.Errorf("event record field too long")
	}

	body := make([]byte, 0, 32+len(topic)+len(source))
	var num [8]byte
	putI64 := func(v int64) {
		binary.LittleEndian.PutUint64(num[:], uint64(v))
		body = append(body, num[:]...)
	}
	putI64(rec.Recv.UnixMicro())
	putI64(rec.Camera.UnixMicro())
	putI64(rec.Match.UnixMicro())
	body = append(body, rec.Op, rec.Class)
	body = appendU16(body, topic)
	body = appendU16(body, source)
	binary.LittleEndian.PutUint16(num[:2], uint16(len(rec.Data)))
	body = append(body, num[:2]...)
	for _, it := range rec.Data {
		if len(it.Name) > 0xFFFF || len(it.Value) > 0xFFFF {
			return nil, fmt.Errorf("event record field too long")
		}
		body = appendU16(body, []byte(it.Name))
		body = appendU16(body, []byte(it.Value))
	}

	frame := make([]byte, 8+len(body))
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(body)))
	copy(frame[8:], body)
	binary.LittleEndian.PutUint32(frame[0:4], crc32.ChecksumIEEE(frame[4:]))
	return frame, nil
}

func appendU16(dst []byte, v []byte) []byte {
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], uint16(len(v)))
	dst = append(dst, n[:]...)
	return append(dst, v...)
}

func takeBytes(b []byte) (val []byte, rest []byte, ok bool) {
	if len(b) < 2 {
		return nil, b, false
	}
	n := int(binary.LittleEndian.Uint16(b[:2]))
	b = b[2:]
	if n < 0 || len(b) < n {
		return nil, b, false
	}
	return b[:n], b[n:], true
}

func decodeRecord(frame []byte) (Record, error) {
	if len(frame) < 8 {
		return Record{}, io.ErrUnexpectedEOF
	}
	want := binary.LittleEndian.Uint32(frame[0:4])
	if crc32.ChecksumIEEE(frame[4:]) != want {
		return Record{}, fmt.Errorf("event record checksum mismatch")
	}
	n := binary.LittleEndian.Uint32(frame[4:8])
	body := frame[8:]
	if uint32(len(body)) != n || len(body) < 26 {
		return Record{}, io.ErrUnexpectedEOF
	}

	recv := int64(binary.LittleEndian.Uint64(body[0:8]))
	camera := int64(binary.LittleEndian.Uint64(body[8:16]))
	match := int64(binary.LittleEndian.Uint64(body[16:24]))
	rec := Record{
		Recv:   time.UnixMicro(recv),
		Camera: time.UnixMicro(camera),
		Op:     body[24],
		Class:  body[25],
	}
	if match != 0 {
		rec.Match = time.UnixMicro(match)
	}

	rest := body[26:]
	topic, rest, ok := takeBytes(rest)
	if !ok {
		return Record{}, io.ErrUnexpectedEOF
	}
	rec.Topic = string(topic)
	source, rest, ok := takeBytes(rest)
	if !ok {
		return Record{}, io.ErrUnexpectedEOF
	}
	rec.Source = string(source)
	if len(rest) < 2 {
		return Record{}, io.ErrUnexpectedEOF
	}
	count := binary.LittleEndian.Uint16(rest[:2])
	rest = rest[2:]
	rec.Data = make([]Item, 0, count)
	for range count {
		var name, value []byte
		name, rest, ok = takeBytes(rest)
		if !ok {
			return Record{}, io.ErrUnexpectedEOF
		}
		value, rest, ok = takeBytes(rest)
		if !ok {
			return Record{}, io.ErrUnexpectedEOF
		}
		rec.Data = append(rec.Data, Item{Name: string(name), Value: string(value)})
	}
	return rec, nil
}

func writeHeader(w io.Writer, day time.Time) error {
	var hdr [headerSize]byte
	copy(hdr[:4], fileMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], fileVersion)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(day.Unix()))
	_, err := w.Write(hdr[:])
	return err
}
