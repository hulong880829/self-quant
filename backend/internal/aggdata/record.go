package aggdata

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
	"os"

	"github.com/klauspost/compress/zstd"
)

const (
	containerMagic = uint32(0x43525153) // SQRC
	recordMagic    = uint32(0x46525153) // SQRF
	trailerMagic   = uint32(0x45525153) // SQRE
	maxRecordBytes = 1 << 20
)

type RecordedBBO struct {
	LiveSample
	RingEpoch    uint64
	RingSequence uint64
	Generation   uint64
	Flags        uint16
}

func ReadBBOShard(path string, visit func(RecordedBBO) error) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open SQREC shard: %w", err)
	}
	defer file.Close()
	decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return 0, fmt.Errorf("create zstd decoder: %w", err)
	}
	defer decoder.Close()
	reader := bufio.NewReaderSize(decoder, 64*1024)

	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, fmt.Errorf("read SQREC header: %w", err)
	}
	le := binary.LittleEndian
	major := le.Uint16(header[4:6])
	minor := le.Uint16(header[6:8])
	if le.Uint32(header[0:4]) != containerMagic ||
		(major != 1 && major != 2) ||
		minor > 0 ||
		le.Uint32(header[8:12]) != 0x01020304 ||
		le.Uint32(header[12:16]) != crc32.ChecksumIEEE(header[:12]) {
		return 0, fmt.Errorf("invalid SQREC container header")
	}

	var records uint64
	for {
		magicBytes := make([]byte, 4)
		if _, err := io.ReadFull(reader, magicBytes); err != nil {
			return records, fmt.Errorf("SQREC trailer is missing: %w", err)
		}
		magic := le.Uint32(magicBytes)
		if magic == trailerMagic {
			trailer := make([]byte, 12)
			if _, err := io.ReadFull(reader, trailer); err != nil {
				return records, fmt.Errorf("read SQREC trailer: %w", err)
			}
			expected := le.Uint64(trailer[0:8])
			checksumInput := append(append([]byte(nil), magicBytes...), trailer[:8]...)
			if le.Uint32(trailer[8:12]) != crc32.ChecksumIEEE(checksumInput) {
				return records, fmt.Errorf("SQREC trailer CRC mismatch")
			}
			if expected != records {
				return records, fmt.Errorf("SQREC record count mismatch")
			}
			if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
				if err == nil {
					return records, fmt.Errorf("data follows SQREC trailer")
				}
				return records, fmt.Errorf("finish zstd stream: %w", err)
			}
			return records, nil
		}
		if magic != recordMagic {
			return records, fmt.Errorf("invalid SQREC frame magic")
		}
		frameHeader := make([]byte, 8)
		if _, err := io.ReadFull(reader, frameHeader); err != nil {
			return records, fmt.Errorf("read SQREC frame header: %w", err)
		}
		length := le.Uint32(frameHeader[0:4])
		if length == 0 || length > maxRecordBytes {
			return records, fmt.Errorf("invalid SQREC frame length %d", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return records, fmt.Errorf("read SQREC frame payload: %w", err)
		}
		if le.Uint32(frameHeader[4:8]) != crc32.ChecksumIEEE(payload) {
			return records, fmt.Errorf("SQREC frame CRC mismatch")
		}
		record, err := decodeRecordedBBO(payload)
		if err != nil {
			return records, fmt.Errorf("decode SQREC record %d: %w", records, err)
		}
		records++
		if visit != nil {
			if err := visit(record); err != nil {
				return records, err
			}
		}
	}
}

type byteDecoder struct {
	data   []byte
	offset int
}

func (d *byteDecoder) take(size int) ([]byte, bool) {
	if size < 0 || d.offset+size > len(d.data) {
		return nil, false
	}
	result := d.data[d.offset : d.offset+size]
	d.offset += size
	return result, true
}

func (d *byteDecoder) u8() (uint8, bool) {
	value, ok := d.take(1)
	if !ok {
		return 0, false
	}
	return value[0], true
}

func (d *byteDecoder) u16() (uint16, bool) {
	value, ok := d.take(2)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint16(value), true
}

func (d *byteDecoder) u32() (uint32, bool) {
	value, ok := d.take(4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(value), true
}

func (d *byteDecoder) u64() (uint64, bool) {
	value, ok := d.take(8)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint64(value), true
}

func (d *byteDecoder) i32() (int32, bool) {
	value, ok := d.u32()
	return int32(value), ok
}

func (d *byteDecoder) i64() (int64, bool) {
	value, ok := d.u64()
	return int64(value), ok
}

func decodeRecordedBBO(payload []byte) (RecordedBBO, error) {
	var result RecordedBBO
	const recordedAggBboPayloadSize = 470
	if len(payload) != recordedAggBboPayloadSize {
		return result, fmt.Errorf("unexpected AggBbo payload length %d", len(payload))
	}
	in := byteDecoder{data: payload}
	kind, ok := in.u8()
	reserved, ok2 := in.u8()
	flags, ok3 := in.u16()
	if !ok || !ok2 || !ok3 || kind != uint8(KindBBO) || reserved != 0 || flags&^uint16(3) != 0 {
		return result, fmt.Errorf("invalid record metadata")
	}
	result.Flags = flags
	var good bool
	if result.WallNS, good = in.u64(); !good || result.WallNS == 0 {
		return result, fmt.Errorf("invalid record wall time")
	}
	if _, good = in.u64(); !good { // monotonic sample time
		return result, fmt.Errorf("truncated record metadata")
	}
	if result.RingEpoch, good = in.u64(); !good {
		return result, fmt.Errorf("truncated record epoch")
	}
	if result.RingSequence, good = in.u64(); !good {
		return result, fmt.Errorf("truncated record sequence")
	}
	if result.Generation, good = in.u64(); !good {
		return result, fmt.Errorf("truncated record generation")
	}
	header, good := in.take(sqmdRecordHeaderSize)
	if !good || binary.LittleEndian.Uint32(header[0:4]) != sqmdMagic ||
		binary.LittleEndian.Uint16(header[4:6]) != sqmdSchemaMajor ||
		binary.LittleEndian.Uint16(header[6:8]) != sqmdSchemaMinor ||
		binary.LittleEndian.Uint16(header[8:10]) != sqmdAggBboMessage ||
		binary.LittleEndian.Uint16(header[10:12]) != sqmdAggBboRecordSize ||
		header[68] > 4 || binary.LittleEndian.Uint16(header[70:72])&^uint16(3) != 0 {
		return result, fmt.Errorf("invalid recorded SQMD header")
	}
	gatedBid, err := decodeRecordSide(&in)
	if err != nil {
		return result, err
	}
	gatedAsk, err := decodeRecordSide(&in)
	if err != nil {
		return result, err
	}
	rawBid, err := decodeRecordRawSide(&in)
	if err != nil {
		return result, err
	}
	rawAsk, err := decodeRecordRawSide(&in)
	if err != nil {
		return result, err
	}
	base, good := in.take(16)
	if !good {
		return result, fmt.Errorf("truncated record identity")
	}
	if _, err := fixedText(base); err != nil {
		return result, err
	}
	quote, good := in.take(16)
	if !good {
		return result, fmt.Errorf("truncated record identity")
	}
	if _, err := fixedText(quote); err != nil {
		return result, err
	}
	slotBytes, good := in.take(8)
	if !good {
		return result, fmt.Errorf("truncated venue slots")
	}
	var slots [8]uint8
	copy(slots[:], slotBytes)
	_, scaleOK := in.u8()
	_, quantityScaleOK := in.u8()
	memberCount, memberOK := in.u8()
	memberMask, maskOK := in.u32()
	liveMask, liveOK := in.u32()
	if !scaleOK || !quantityScaleOK || !memberOK || !maskOK || !liveOK ||
		memberCount > maxVenues || !validateMask(memberMask, memberCount) ||
		bits.OnesCount32(memberMask) != int(memberCount) ||
		liveMask >= 1<<maxVenues || liveMask&^memberMask != 0 ||
		!validVenueSlots(slots, memberMask) {
		return result, fmt.Errorf("invalid recorded AggBbo identity")
	}
	if err := validateSide(gatedBid, liveMask); err != nil {
		return result, err
	}
	if err := validateSide(gatedAsk, liveMask); err != nil {
		return result, err
	}
	if err := validateRawSide(rawBid, memberMask); err != nil {
		return result, err
	}
	if err := validateRawSide(rawAsk, memberMask); err != nil {
		return result, err
	}
	rawCrossBPS, good := in.i32()
	if !good {
		return result, fmt.Errorf("truncated raw BPS")
	}
	gatedCrossBPS, good := in.i32()
	if !good {
		return result, fmt.Errorf("truncated gated BPS")
	}
	if rawCrossBPS < 0 || gatedCrossBPS < 0 {
		return result, fmt.Errorf("negative recorded cross BPS")
	}
	result.RawBPS, err = signedSpreadBPS(rawBid.Price, rawAsk.Price)
	if err != nil {
		return result, err
	}
	result.GatedBPS, err = signedSpreadBPS(gatedBid.Price, gatedAsk.Price)
	if err != nil {
		return result, err
	}
	if _, good = in.take(15); !good { // skew, threshold, FX age, and three venue IDs
		return result, fmt.Errorf("truncated AggBbo summary")
	}
	start, startOK := in.u64()
	end, endOK := in.u64()
	rawMin, ok := in.i32()
	rawMax, ok2 := in.i32()
	gatedMin, ok3 := in.i32()
	gatedMax, good := in.i32()
	result.RawMin, result.RawMax = float64(rawMin), float64(rawMax)
	result.GatedMin, result.GatedMax = float64(gatedMin), float64(gatedMax)
	result.WindowSamples, memberOK = in.u64()
	if !startOK || !endOK || !ok || !ok2 || !ok3 || !good || !memberOK ||
		in.offset != len(payload) ||
		(result.WindowSamples != 0 &&
			(start > end || result.RawMin > result.RawMax || result.GatedMin > result.GatedMax)) {
		return result, fmt.Errorf("invalid cross BPS window")
	}
	return result, nil
}

func decodeRecordSide(in *byteDecoder) (Side, error) {
	var result Side
	var ok bool
	if result.Price, ok = in.i64(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.Quantity, ok = in.i64(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	for index := range result.VenueQuantity {
		if result.VenueQuantity[index], ok = in.i64(); !ok {
			return result, io.ErrUnexpectedEOF
		}
	}
	if result.ExchangeTSNS, ok = in.u64(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.VenueMask, ok = in.u32(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.WorstIngressAgeUS, ok = in.u32(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.BestVenue, ok = in.u8(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.TimestampVenue, ok = in.u8(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.ContributorCount, ok = in.u8(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	return result, nil
}

func decodeRecordRawSide(in *byteDecoder) (RawSide, error) {
	var result RawSide
	var ok bool
	if result.Price, ok = in.i64(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.Quantity, ok = in.i64(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.VenueMask, ok = in.u32(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	if result.BestVenue, ok = in.u8(); !ok {
		return result, io.ErrUnexpectedEOF
	}
	return result, nil
}
