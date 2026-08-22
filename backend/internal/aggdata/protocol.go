package aggdata

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

const (
	gatewayMagic         = uint32(0x57475153)
	gatewayHeaderSize    = 52
	sqmdMagic            = uint32(0x444d5153)
	sqmdSchemaMajor      = uint16(2)
	sqmdSchemaMinor      = uint16(0)
	sqmdAggBboMessage    = uint16(8)
	sqmdAggBboRecordSize = 416
	sqmdRecordHeaderSize = 72
)

type GatewayFrame struct {
	Kind         Kind
	Format       uint8
	TopicID      uint16
	Flags        uint16
	RingEpoch    uint64
	RingSequence uint64
	Generation   uint64
	WallNS       uint64
	BBO          *BBO
	Book         *Book
}

func DecodeGatewayFrame(frame []byte) (GatewayFrame, error) {
	var result GatewayFrame
	if len(frame) < gatewayHeaderSize {
		return result, fmt.Errorf("SQGW frame is truncated")
	}
	le := binary.LittleEndian
	if le.Uint32(frame[0:4]) != gatewayMagic {
		return result, fmt.Errorf("invalid SQGW magic")
	}
	if le.Uint16(frame[4:6]) != 1 || le.Uint16(frame[6:8]) != 0 {
		return result, fmt.Errorf("unsupported SQGW version")
	}
	result.Kind = Kind(frame[8])
	result.Format = frame[9]
	result.TopicID = le.Uint16(frame[10:12])
	result.Flags = le.Uint16(frame[12:14])
	if le.Uint16(frame[14:16]) != gatewayHeaderSize {
		return result, fmt.Errorf("invalid SQGW header length")
	}
	payloadLength := int(le.Uint32(frame[16:20]))
	if payloadLength != len(frame)-gatewayHeaderSize {
		return result, fmt.Errorf("SQGW payload length mismatch")
	}
	result.RingEpoch = le.Uint64(frame[20:28])
	result.RingSequence = le.Uint64(frame[28:36])
	result.Generation = le.Uint64(frame[36:44])
	result.WallNS = le.Uint64(frame[44:52])
	payload := frame[gatewayHeaderSize:]
	switch result.Kind {
	case KindReset:
		if result.Format != 0 || result.Flags != 1 || len(payload) != 0 {
			return result, fmt.Errorf("invalid SQGW reset frame")
		}
	case KindBBO:
		if result.Format != 1 || result.Flags != 0 {
			return result, fmt.Errorf("invalid SQGW AggBbo frame")
		}
		value, err := decodeGatewayBBO(payload)
		if err != nil {
			return result, err
		}
		result.BBO = &value
	case KindBook:
		if result.Format != 2 || result.Flags != 0 {
			return result, fmt.Errorf("invalid SQGW AggOrderBook frame")
		}
		value, err := decodeGatewayBook(payload)
		if err != nil {
			return result, err
		}
		result.Book = &value
	default:
		return result, fmt.Errorf("unsupported SQGW frame kind %d", result.Kind)
	}
	return result, nil
}

func decodeGatewayBBO(payload []byte) (BBO, error) {
	var result BBO
	if len(payload) != sqmdAggBboRecordSize {
		return result, fmt.Errorf("invalid SQMD AggBbo length")
	}
	le := binary.LittleEndian
	if le.Uint32(payload[0:4]) != sqmdMagic ||
		le.Uint16(payload[4:6]) != sqmdSchemaMajor ||
		le.Uint16(payload[6:8]) != sqmdSchemaMinor ||
		le.Uint16(payload[8:10]) != sqmdAggBboMessage ||
		le.Uint16(payload[10:12]) != sqmdAggBboRecordSize {
		return result, fmt.Errorf("invalid SQMD AggBbo header")
	}
	result.HeaderFlags = le.Uint16(payload[70:72])
	if payload[68] > 4 || result.HeaderFlags&^uint16(3) != 0 {
		return result, fmt.Errorf("invalid SQMD AggBbo header fields")
	}
	var err error
	result.GatedBid, err = decodeABISide(payload[72:176])
	if err != nil {
		return result, fmt.Errorf("decode gated bid: %w", err)
	}
	result.GatedAsk, err = decodeABISide(payload[176:280])
	if err != nil {
		return result, fmt.Errorf("decode gated ask: %w", err)
	}
	result.RawBid = decodeABIRawSide(payload[280:304])
	result.RawAsk = decodeABIRawSide(payload[304:328])
	if result.Base, err = fixedText(payload[328:344]); err != nil {
		return result, fmt.Errorf("decode base asset: %w", err)
	}
	if result.Quote, err = fixedText(payload[344:360]); err != nil {
		return result, fmt.Errorf("decode quote asset: %w", err)
	}
	copy(result.VenueSlotIDs[:], payload[360:368])
	result.PriceScale = payload[368]
	result.QuantityScale = payload[369]
	result.MemberCount = payload[370]
	result.MemberMask = le.Uint32(payload[376:380])
	result.LiveMask = le.Uint32(payload[380:384])
	result.RawCrossBPS = int32(le.Uint32(payload[384:388]))
	result.GatedCrossBPS = int32(le.Uint32(payload[388:392]))
	result.SkewUS = le.Uint32(payload[392:396])
	result.CrossSkewThresholdUS = le.Uint32(payload[396:400])
	result.FXAgeUS = le.Uint32(payload[400:404])
	result.CrossBidVenue = payload[404]
	result.CrossAskVenue = payload[405]
	result.FXVenue = payload[406]
	if result.MemberCount > maxVenues ||
		!validateMask(result.MemberMask, result.MemberCount) ||
		bits.OnesCount32(result.MemberMask) != int(result.MemberCount) ||
		result.LiveMask >= 1<<maxVenues || result.LiveMask&^result.MemberMask != 0 ||
		result.RawCrossBPS < 0 || result.GatedCrossBPS < 0 ||
		!allZero(payload[371:376]) || !allZero(payload[407:416]) ||
		!allZero(payload[301:304]) || !allZero(payload[325:328]) ||
		!validVenueSlots(result.VenueSlotIDs, result.MemberMask) {
		return result, fmt.Errorf("invalid AggBbo member masks")
	}
	if err := validateSide(result.GatedBid, result.LiveMask); err != nil {
		return result, err
	}
	if err := validateSide(result.GatedAsk, result.LiveMask); err != nil {
		return result, err
	}
	if err := validateRawSide(result.RawBid, result.MemberMask); err != nil {
		return result, err
	}
	if err := validateRawSide(result.RawAsk, result.MemberMask); err != nil {
		return result, err
	}
	if result.CrossBidVenue != result.GatedBid.TimestampVenue ||
		result.CrossAskVenue != result.GatedAsk.TimestampVenue {
		return result, fmt.Errorf("invalid AggBbo cross venues")
	}
	result.RawSpreadBPS, err = signedSpreadBPS(result.RawBid.Price, result.RawAsk.Price)
	if err != nil {
		return result, err
	}
	result.GatedSpreadBPS, err = signedSpreadBPS(result.GatedBid.Price, result.GatedAsk.Price)
	if err != nil {
		return result, err
	}
	return result, nil
}

func decodeABISide(data []byte) (Side, error) {
	if !allZero(data[99:104]) {
		return Side{}, fmt.Errorf("nonzero AggBbo side padding")
	}
	le := binary.LittleEndian
	result := Side{
		Price:             int64(le.Uint64(data[0:8])),
		Quantity:          int64(le.Uint64(data[8:16])),
		ExchangeTSNS:      le.Uint64(data[80:88]),
		VenueMask:         le.Uint32(data[88:92]),
		WorstIngressAgeUS: le.Uint32(data[92:96]),
		BestVenue:         data[96],
		TimestampVenue:    data[97],
		ContributorCount:  data[98],
	}
	for index := range result.VenueQuantity {
		offset := 16 + index*8
		result.VenueQuantity[index] = int64(le.Uint64(data[offset : offset+8]))
	}
	return result, nil
}

func decodeABIRawSide(data []byte) RawSide {
	le := binary.LittleEndian
	return RawSide{
		Price:     int64(le.Uint64(data[0:8])),
		Quantity:  int64(le.Uint64(data[8:16])),
		VenueMask: le.Uint32(data[16:20]),
		BestVenue: data[20],
	}
}

func validateRawSide(side RawSide, memberMask uint32) error {
	if side.Price < 0 || side.Quantity < 0 || side.VenueMask >= 1<<maxVenues ||
		side.VenueMask&^memberMask != 0 {
		return fmt.Errorf("invalid raw BBO side")
	}
	if side.VenueMask == 0 {
		if side.Price != 0 || side.Quantity != 0 || side.BestVenue != 0 {
			return fmt.Errorf("invalid empty raw BBO side")
		}
	} else if side.Price <= 0 || side.BestVenue >= maxVenues ||
		side.VenueMask&(1<<side.BestVenue) == 0 {
		return fmt.Errorf("invalid raw BBO venue selection")
	}
	return nil
}

func decodeGatewayBook(payload []byte) (Book, error) {
	var result Book
	const metadataBytes = 69
	const levelBytes = 85
	if len(payload) < metadataBytes {
		return result, fmt.Errorf("compact AggOrderBook is truncated")
	}
	le := binary.LittleEndian
	result.ExchangeTSNS = le.Uint64(payload[0:8])
	result.BookGeneration = le.Uint32(payload[8:12])
	result.HeaderFlags = le.Uint16(payload[12:14])
	var err error
	if result.Base, err = fixedText(payload[14:30]); err != nil {
		return result, fmt.Errorf("decode base asset: %w", err)
	}
	if result.Quote, err = fixedText(payload[30:46]); err != nil {
		return result, fmt.Errorf("decode quote asset: %w", err)
	}
	copy(result.VenueSlotIDs[:], payload[46:54])
	result.PriceScale = payload[54]
	result.QuantityScale = payload[55]
	result.MemberCount = payload[56]
	result.MemberMask = le.Uint32(payload[57:61])
	result.ActiveMask = le.Uint32(payload[61:65])
	bids, asks := int(le.Uint16(payload[65:67])), int(le.Uint16(payload[67:69]))
	if bids > maxDepth || asks > maxDepth || result.HeaderFlags != 0 ||
		len(payload) != metadataBytes+levelBytes*(bids+asks) ||
		result.MemberCount > maxVenues ||
		!validateMask(result.MemberMask, result.MemberCount) ||
		bits.OnesCount32(result.MemberMask) != int(result.MemberCount) ||
		result.ActiveMask >= 1<<maxVenues || result.ActiveMask&^result.MemberMask != 0 ||
		!validVenueSlots(result.VenueSlotIDs, result.MemberMask) {
		return result, fmt.Errorf("invalid compact AggOrderBook metadata")
	}
	result.Bids = make([]Level, bids)
	result.Asks = make([]Level, asks)
	offset := metadataBytes
	for index := range result.Bids {
		result.Bids[index] = decodeCompactLevel(payload[offset : offset+levelBytes])
		if err := validateLevel(result.Bids[index], result.ActiveMask); err != nil {
			return result, fmt.Errorf("invalid bid %d: %w", index, err)
		}
		if index > 0 && result.Bids[index-1].Price <= result.Bids[index].Price {
			return result, fmt.Errorf("bids are not strictly descending")
		}
		offset += levelBytes
	}
	for index := range result.Asks {
		result.Asks[index] = decodeCompactLevel(payload[offset : offset+levelBytes])
		if err := validateLevel(result.Asks[index], result.ActiveMask); err != nil {
			return result, fmt.Errorf("invalid ask %d: %w", index, err)
		}
		if index > 0 && result.Asks[index-1].Price >= result.Asks[index].Price {
			return result, fmt.Errorf("asks are not strictly ascending")
		}
		offset += levelBytes
	}
	return result, nil
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func validVenueSlots(slots [8]uint8, memberMask uint32) bool {
	for index, id := range slots {
		if (memberMask&(1<<index) != 0) == (id == 0) {
			return false
		}
	}
	return true
}

func decodeCompactLevel(data []byte) Level {
	le := binary.LittleEndian
	result := Level{
		Price:            int64(le.Uint64(data[0:8])),
		Quantity:         int64(le.Uint64(data[8:16])),
		VenueMask:        le.Uint32(data[80:84]),
		ContributorCount: data[84],
	}
	for index := range result.VenueQuantity {
		offset := 16 + index*8
		result.VenueQuantity[index] = int64(le.Uint64(data[offset : offset+8]))
	}
	return result
}
