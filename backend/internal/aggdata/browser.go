package aggdata

import (
	"encoding/binary"
	"fmt"
	"math"
)

const browserMagic = uint32(0x42415153) // SQAB

func EncodeBrowserSnapshot(snapshot *Snapshot, depth int) ([]byte, error) {
	if snapshot == nil || !snapshot.Ready || (snapshot.BBO == nil && snapshot.Book == nil) {
		return nil, fmt.Errorf("snapshot is not ready")
	}
	if depth <= 0 || depth > maxDepth {
		return nil, fmt.Errorf("depth must be within 1..%d", maxDepth)
	}
	symbol := []byte(snapshot.Symbol)
	if len(symbol) == 0 || len(symbol) > 255 {
		return nil, fmt.Errorf("invalid browser symbol")
	}
	kind := snapshot.Kind
	var priceScale, quantityScale uint8
	var venueIDs [8]uint8
	var memberCount uint8
	if kind == KindBBO {
		priceScale, quantityScale = snapshot.BBO.PriceScale, snapshot.BBO.QuantityScale
		venueIDs, memberCount = snapshot.BBO.VenueSlotIDs, snapshot.BBO.MemberCount
	} else if kind == KindBook {
		priceScale, quantityScale = snapshot.Book.PriceScale, snapshot.Book.QuantityScale
		venueIDs, memberCount = snapshot.Book.VenueSlotIDs, snapshot.Book.MemberCount
	} else {
		return nil, fmt.Errorf("unsupported browser snapshot kind")
	}
	if memberCount > maxVenues {
		return nil, fmt.Errorf("invalid browser venue count")
	}

	body := make([]byte, 0, 4096)
	body = append(body, symbol...)
	body = append(body, venueIDs[:memberCount]...)
	if kind == KindBBO {
		body = appendCompactSide(body, snapshot.BBO.GatedBid)
		body = appendCompactSide(body, snapshot.BBO.GatedAsk)
		body = appendRawSide(body, snapshot.BBO.RawBid)
		body = appendRawSide(body, snapshot.BBO.RawAsk)
		body = binary.LittleEndian.AppendUint64(body, math.Float64bits(snapshot.BBO.RawSpreadBPS))
		body = binary.LittleEndian.AppendUint64(body, math.Float64bits(snapshot.BBO.GatedSpreadBPS))
	} else {
		bids := min(depth, len(snapshot.Book.Bids))
		asks := min(depth, len(snapshot.Book.Asks))
		body = binary.LittleEndian.AppendUint16(body, uint16(bids))
		body = binary.LittleEndian.AppendUint16(body, uint16(asks))
		for _, level := range snapshot.Book.Bids[:bids] {
			body = appendCompactLevel(body, level)
		}
		for _, level := range snapshot.Book.Asks[:asks] {
			body = appendCompactLevel(body, level)
		}
	}

	const headerBytes = 42
	frame := make([]byte, 0, headerBytes+len(body))
	frame = binary.LittleEndian.AppendUint32(frame, browserMagic)
	frame = append(frame, 1, 0, byte(kind), 0)
	frame = binary.LittleEndian.AppendUint16(frame, headerBytes)
	frame = binary.LittleEndian.AppendUint32(frame, uint32(len(body)))
	frame = append(frame, byte(len(symbol)), priceScale, quantityScale, memberCount)
	frame = binary.LittleEndian.AppendUint64(frame, snapshot.RingSequence)
	frame = binary.LittleEndian.AppendUint64(frame, snapshot.Generation)
	frame = binary.LittleEndian.AppendUint64(frame, snapshot.WallNS)
	frame = append(frame, body...)
	return frame, nil
}

func appendCompactSide(output []byte, side Side) []byte {
	output = binary.LittleEndian.AppendUint64(output, uint64(side.Price))
	output = binary.LittleEndian.AppendUint64(output, uint64(side.Quantity))
	output = binary.LittleEndian.AppendUint32(output, side.VenueMask)
	output = append(output, side.ContributorCount)
	for index, quantity := range side.VenueQuantity {
		if side.VenueMask&(1<<index) != 0 {
			output = append(output, byte(index))
			output = binary.LittleEndian.AppendUint64(output, uint64(quantity))
		}
	}
	return output
}

func appendRawSide(output []byte, side RawSide) []byte {
	output = binary.LittleEndian.AppendUint64(output, uint64(side.Price))
	output = binary.LittleEndian.AppendUint64(output, uint64(side.Quantity))
	output = binary.LittleEndian.AppendUint32(output, side.VenueMask)
	output = append(output, side.BestVenue)
	return output
}

func appendCompactLevel(output []byte, level Level) []byte {
	output = binary.LittleEndian.AppendUint64(output, uint64(level.Price))
	output = binary.LittleEndian.AppendUint64(output, uint64(level.Quantity))
	output = binary.LittleEndian.AppendUint32(output, level.VenueMask)
	output = append(output, level.ContributorCount)
	for index, quantity := range level.VenueQuantity {
		if level.VenueMask&(1<<index) != 0 {
			output = append(output, byte(index))
			output = binary.LittleEndian.AppendUint64(output, uint64(quantity))
		}
	}
	return output
}
