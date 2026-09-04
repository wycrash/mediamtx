package moq

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

func h264CodecString(sps []byte) string {
	if len(sps) < 4 {
		return "avc3.640028"
	}
	return "avc1." + hex.EncodeToString(sps[1:4])
}

func h265EncodeProfileSpace(v uint8) string {
	if v >= 1 && v <= 3 {
		return string('A' + (v - 1))
	}
	return ""
}

func h265EncodeCompatibilityFlag(v [32]bool) string {
	var o uint32
	for i, b := range v {
		if b {
			o |= 1 << i
		}
	}
	return fmt.Sprintf("%x", o)
}

func h265EncodeGeneralTierFlag(v uint8) string {
	if v > 0 {
		return "H"
	}
	return "L"
}

func h265EncodeGeneralConstraintIndicatorFlags(v *h265.SPS_ProfileTierLevel) string {
	var ret []string

	var o1 uint8
	if v.GeneralProgressiveSourceFlag {
		o1 |= 1 << 7
	}
	if v.GeneralInterlacedSourceFlag {
		o1 |= 1 << 6
	}
	if v.GeneralNonPackedConstraintFlag {
		o1 |= 1 << 5
	}
	if v.GeneralFrameOnlyConstraintFlag {
		o1 |= 1 << 4
	}
	if v.GeneralMax12bitConstraintFlag {
		o1 |= 1 << 3
	}
	if v.GeneralMax10bitConstraintFlag {
		o1 |= 1 << 2
	}
	if v.GeneralMax8bitConstraintFlag {
		o1 |= 1 << 1
	}
	if v.GeneralMax422ChromeConstraintFlag {
		o1 |= 1 << 0
	}
	ret = append(ret, fmt.Sprintf("%x", o1))

	var o2 uint8
	if v.GeneralMax420ChromaConstraintFlag {
		o2 |= 1 << 7
	}
	if v.GeneralMaxMonochromeConstraintFlag {
		o2 |= 1 << 6
	}
	if v.GeneralIntraConstraintFlag {
		o2 |= 1 << 5
	}
	if v.GeneralOnePictureOnlyConstraintFlag {
		o2 |= 1 << 4
	}
	if v.GeneralLowerBitRateConstraintFlag {
		o2 |= 1 << 3
	}
	if v.GeneralMax14BitConstraintFlag {
		o2 |= 1 << 2
	}
	if o2 != 0 {
		ret = append(ret, fmt.Sprintf("%x", o2))
	}

	return strings.Join(ret, ".")
}

func h265CodecString(sps []byte) string {
	var parsed h265.SPS
	if err := parsed.Unmarshal(sps); err != nil {
		return "hev1.1.6.L93.B0"
	}
	ptl := parsed.ProfileTierLevel
	return "hvc1." +
		h265EncodeProfileSpace(ptl.GeneralProfileSpace) +
		strconv.FormatInt(int64(ptl.GeneralProfileIdc), 10) + "." +
		h265EncodeCompatibilityFlag(ptl.GeneralProfileCompatibilityFlag) + "." +
		h265EncodeGeneralTierFlag(ptl.GeneralTierFlag) +
		strconv.FormatInt(int64(ptl.GeneralLevelIdc), 10) + "." +
		h265EncodeGeneralConstraintIndicatorFlags(&ptl)
}

func makeAvcC(sps, pps []byte) []byte {
	avcC := make([]byte, 11+len(sps)+len(pps))
	avcC[0] = 0x01
	avcC[1] = sps[1]
	avcC[2] = sps[2]
	avcC[3] = sps[3]
	avcC[4] = 0xff
	avcC[5] = 0xe1
	binary.BigEndian.PutUint16(avcC[6:8], uint16(len(sps)))
	off := 8
	copy(avcC[off:], sps)
	off += len(sps)
	avcC[off] = 0x01
	off++
	binary.BigEndian.PutUint16(avcC[off:off+2], uint16(len(pps)))
	off += 2
	copy(avcC[off:], pps)
	return avcC
}

func makeHvcC(vps, sps, pps []byte) []byte {
	var parsed h265.SPS
	err := parsed.Unmarshal(sps)

	numTemporalLayers := uint8(1)
	temporalIDNested := uint8(0)
	chromaFormat := uint8(1)
	bitDepthLumaMinus8 := uint8(0)
	bitDepthChromaMinus8 := uint8(0)
	ptl0 := byte(0x01)
	compat := [4]byte{0x60, 0, 0, 0}
	constraint := [6]byte{0x90, 0, 0, 0, 0, 0}
	level := byte(93)

	if err == nil {
		numTemporalLayers = parsed.MaxSubLayersMinus1 + 1
		if parsed.TemporalIDNestingFlag {
			temporalIDNested = 1
		}
		chromaFormat = uint8(parsed.ChromaFormatIdc)
		bitDepthLumaMinus8 = uint8(parsed.BitDepthLumaMinus8)
		bitDepthChromaMinus8 = uint8(parsed.BitDepthChromaMinus8)
		ptl := parsed.ProfileTierLevel
		ptl0 = (ptl.GeneralProfileSpace << 6) | (ptl.GeneralTierFlag << 5) | ptl.GeneralProfileIdc
		for i, b := range ptl.GeneralProfileCompatibilityFlag {
			if b {
				compat[i/8] |= 1 << (7 - (i % 8))
			}
		}
		level = ptl.GeneralLevelIdc
	}
	if len(sps) >= 15 {
		copy(constraint[:], sps[8:14])
		if err != nil {
			ptl0 = sps[3]
			copy(compat[:], sps[4:8])
			level = sps[14]
		}
	}

	arrays := [][]byte{vps, sps, pps}
	types := []byte{
		byte(h265.NALUType_VPS_NUT),
		byte(h265.NALUType_SPS_NUT),
		byte(h265.NALUType_PPS_NUT),
	}
	size := 23
	for _, nalu := range arrays {
		size += 5 + len(nalu)
	}
	hvcC := make([]byte, size)
	hvcC[0] = 0x01
	hvcC[1] = ptl0
	copy(hvcC[2:6], compat[:])
	copy(hvcC[6:12], constraint[:])
	hvcC[12] = level
	hvcC[13] = 0xf0
	hvcC[14] = 0x00
	hvcC[15] = 0xfc
	hvcC[16] = 0xfc | (chromaFormat & 0x03)
	hvcC[17] = 0xf8 | (bitDepthLumaMinus8 & 0x07)
	hvcC[18] = 0xf8 | (bitDepthChromaMinus8 & 0x07)
	hvcC[19] = 0x00
	hvcC[20] = 0x00
	hvcC[21] = ((numTemporalLayers & 0x07) << 3) | ((temporalIDNested & 0x01) << 2) | 0x03
	hvcC[22] = byte(len(arrays))
	off := 23
	for i, nalu := range arrays {
		hvcC[off] = 0x80 | (types[i] & 0x3f)
		off++
		binary.BigEndian.PutUint16(hvcC[off:off+2], 1)
		off += 2
		binary.BigEndian.PutUint16(hvcC[off:off+2], uint16(len(nalu)))
		off += 2
		copy(hvcC[off:], nalu)
		off += len(nalu)
	}
	return hvcC
}

func parseAvcC(avcC []byte) (sps, pps []byte, err error) {
	if len(avcC) < 7 {
		return nil, nil, fmt.Errorf("avcC too short")
	}
	off := 5
	numSPS := int(avcC[off] & 0x1f)
	off++
	for i := 0; i < numSPS; i++ {
		if off+2 > len(avcC) {
			return nil, nil, fmt.Errorf("invalid avcC SPS length")
		}
		n := int(binary.BigEndian.Uint16(avcC[off : off+2]))
		off += 2
		if off+n > len(avcC) {
			return nil, nil, fmt.Errorf("invalid avcC SPS")
		}
		if sps == nil {
			sps = avcC[off : off+n]
		}
		off += n
	}
	if off >= len(avcC) {
		return nil, nil, fmt.Errorf("invalid avcC PPS count")
	}
	numPPS := int(avcC[off])
	off++
	for i := 0; i < numPPS; i++ {
		if off+2 > len(avcC) {
			return nil, nil, fmt.Errorf("invalid avcC PPS length")
		}
		n := int(binary.BigEndian.Uint16(avcC[off : off+2]))
		off += 2
		if off+n > len(avcC) {
			return nil, nil, fmt.Errorf("invalid avcC PPS")
		}
		if pps == nil {
			pps = avcC[off : off+n]
		}
		off += n
	}
	if sps == nil || pps == nil {
		return nil, nil, fmt.Errorf("avcC missing parameter sets")
	}
	return sps, pps, nil
}

func parseHvcC(hvcC []byte) (vps, sps, pps []byte, err error) {
	if len(hvcC) < 23 {
		return nil, nil, nil, fmt.Errorf("hvcC too short")
	}
	numArrays := int(hvcC[22])
	off := 23
	for i := 0; i < numArrays; i++ {
		if off+3 > len(hvcC) {
			return nil, nil, nil, fmt.Errorf("invalid hvcC array")
		}
		nalType := hvcC[off] & 0x3f
		off++
		numNalus := int(binary.BigEndian.Uint16(hvcC[off : off+2]))
		off += 2
		for j := 0; j < numNalus; j++ {
			if off+2 > len(hvcC) {
				return nil, nil, nil, fmt.Errorf("invalid hvcC NAL length")
			}
			n := int(binary.BigEndian.Uint16(hvcC[off : off+2]))
			off += 2
			if off+n > len(hvcC) {
				return nil, nil, nil, fmt.Errorf("invalid hvcC NAL")
			}
			nalu := hvcC[off : off+n]
			off += n
			switch h265.NALUType(nalType) {
			case h265.NALUType_VPS_NUT:
				if vps == nil {
					vps = nalu
				}
			case h265.NALUType_SPS_NUT:
				if sps == nil {
					sps = nalu
				}
			case h265.NALUType_PPS_NUT:
				if pps == nil {
					pps = nalu
				}
			}
		}
	}
	if vps == nil || sps == nil || pps == nil {
		return nil, nil, nil, fmt.Errorf("hvcC missing parameter sets")
	}
	return vps, sps, pps, nil
}

func stripH264Params(nalus [][]byte) [][]byte {
	n := 0
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1f)
		if typ == h264.NALUTypeSPS || typ == h264.NALUTypePPS || typ == h264.NALUTypeAccessUnitDelimiter {
			continue
		}
		n++
	}
	if n == 0 {
		return nil
	}
	out := make([][]byte, 0, n)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1f)
		if typ == h264.NALUTypeSPS || typ == h264.NALUTypePPS || typ == h264.NALUTypeAccessUnitDelimiter {
			continue
		}
		out = append(out, nalu)
	}
	return out
}

func stripH265Params(nalus [][]byte) [][]byte {
	n := 0
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		typ := h265.NALUType((nalu[0] >> 1) & 0x3f)
		switch typ {
		case h265.NALUType_VPS_NUT, h265.NALUType_SPS_NUT, h265.NALUType_PPS_NUT, h265.NALUType_AUD_NUT:
			continue
		}
		n++
	}
	if n == 0 {
		return nil
	}
	out := make([][]byte, 0, n)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		typ := h265.NALUType((nalu[0] >> 1) & 0x3f)
		switch typ {
		case h265.NALUType_VPS_NUT, h265.NALUType_SPS_NUT, h265.NALUType_PPS_NUT, h265.NALUType_AUD_NUT:
			continue
		}
		out = append(out, nalu)
	}
	return out
}
