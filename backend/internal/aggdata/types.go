package aggdata

import (
	"fmt"
	"math"
	"math/bits"
	"strings"
	"sync/atomic"
)

const (
	maxVenues = 8
	maxDepth  = 50
)

type Kind uint8

const (
	KindBBO   Kind = 1
	KindBook  Kind = 2
	KindReset Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindBBO:
		return "aggbbo"
	case KindBook:
		return "aggorderbook"
	default:
		return "unknown"
	}
}

type Identity struct {
	Profile string
	Symbol  string
	Stream  string
}

func (i Identity) key() string {
	return i.Profile + "\x00" + i.Symbol
}

type Market struct {
	Profile       string   `json:"profile"`
	Symbol        string   `json:"symbol"`
	Base          string   `json:"base"`
	Quote         string   `json:"quote"`
	Venues        []string `json:"venues"`
	PriceScale    uint8    `json:"price_scale"`
	QuantityScale uint8    `json:"quantity_scale"`
	HasBBO        bool     `json:"has_bbo"`
	HasOrderBook  bool     `json:"has_order_book"`
	Recording     bool     `json:"recording"`
	Live          bool     `json:"live"`
	UpdatedNS     uint64   `json:"updated_ns"`
}

type Side struct {
	Price             int64    `json:"price"`
	Quantity          int64    `json:"quantity"`
	VenueQuantity     [8]int64 `json:"-"`
	ExchangeTSNS      uint64   `json:"exchange_ts_ns"`
	VenueMask         uint32   `json:"venue_mask"`
	WorstIngressAgeUS uint32   `json:"worst_ingress_age_us"`
	BestVenue         uint8    `json:"best_venue"`
	TimestampVenue    uint8    `json:"timestamp_venue"`
	ContributorCount  uint8    `json:"contributor_count"`
}

type RawSide struct {
	Price     int64  `json:"price"`
	Quantity  int64  `json:"quantity"`
	VenueMask uint32 `json:"venue_mask"`
	BestVenue uint8  `json:"best_venue"`
}

type BBO struct {
	Base                 string
	Quote                string
	VenueSlotIDs         [8]uint8
	PriceScale           uint8
	QuantityScale        uint8
	MemberCount          uint8
	MemberMask           uint32
	LiveMask             uint32
	HeaderFlags          uint16
	GatedBid             Side
	GatedAsk             Side
	RawBid               RawSide
	RawAsk               RawSide
	RawCrossBPS          int32
	GatedCrossBPS        int32
	RawSpreadBPS         float64
	GatedSpreadBPS       float64
	SkewUS               uint32
	CrossSkewThresholdUS uint32
	FXAgeUS              uint32
	CrossBidVenue        uint8
	CrossAskVenue        uint8
	FXVenue              uint8
}

type Level struct {
	Price            int64
	Quantity         int64
	VenueQuantity    [8]int64
	VenueMask        uint32
	ContributorCount uint8
}

type Book struct {
	ExchangeTSNS   uint64
	BookGeneration uint32
	HeaderFlags    uint16
	Base           string
	Quote          string
	VenueSlotIDs   [8]uint8
	PriceScale     uint8
	QuantityScale  uint8
	MemberCount    uint8
	MemberMask     uint32
	ActiveMask     uint32
	Bids           []Level
	Asks           []Level
}

type Snapshot struct {
	Profile      string
	Symbol       string
	Kind         Kind
	TopicID      uint16
	RingEpoch    uint64
	RingSequence uint64
	Generation   uint64
	WallNS       uint64
	Ready        bool
	BBO          *BBO
	Book         *Book
	Browser20    []byte
	Browser50    []byte
}

type snapshotSlot struct {
	value atomic.Pointer[Snapshot]
}

func validateMask(mask uint32, count uint8) bool {
	return count <= maxVenues && mask < 1<<maxVenues
}

func validateSide(side Side, allowedMask uint32) error {
	if side.Price < 0 || side.Quantity < 0 || side.VenueMask >= 1<<maxVenues ||
		side.VenueMask&^allowedMask != 0 {
		return fmt.Errorf("invalid BBO side")
	}
	if bits.OnesCount32(side.VenueMask) != int(side.ContributorCount) {
		return fmt.Errorf("BBO contributor count mismatch")
	}
	var total int64
	for index, quantity := range side.VenueQuantity {
		if quantity < 0 || (side.VenueMask&(1<<index) == 0 && quantity != 0) {
			return fmt.Errorf("invalid BBO venue quantity")
		}
		total += quantity
	}
	if total != side.Quantity {
		return fmt.Errorf("BBO venue quantities do not sum to total")
	}
	if side.VenueMask == 0 {
		if side.Price != 0 || side.Quantity != 0 || side.ExchangeTSNS != 0 ||
			side.WorstIngressAgeUS != 0 || side.BestVenue != 0 ||
			side.TimestampVenue != 0 || side.ContributorCount != 0 {
			return fmt.Errorf("invalid empty BBO side")
		}
	} else if side.Price <= 0 || side.BestVenue >= maxVenues ||
		side.TimestampVenue >= maxVenues ||
		side.VenueMask&(1<<side.BestVenue) == 0 ||
		side.VenueMask&(1<<side.TimestampVenue) == 0 {
		return fmt.Errorf("invalid BBO venue selection")
	}
	return nil
}

func validateLevel(level Level, activeMask uint32) error {
	if level.Price <= 0 || level.Quantity <= 0 || level.VenueMask >= 1<<maxVenues ||
		level.VenueMask&^activeMask != 0 {
		return fmt.Errorf("invalid order book level")
	}
	if bits.OnesCount32(level.VenueMask) != int(level.ContributorCount) {
		return fmt.Errorf("order book contributor count mismatch")
	}
	var total int64
	for index, quantity := range level.VenueQuantity {
		if quantity < 0 || (level.VenueMask&(1<<index) == 0 && quantity != 0) {
			return fmt.Errorf("invalid order book venue quantity")
		}
		total += quantity
	}
	if total != level.Quantity {
		return fmt.Errorf("order book venue quantities do not sum to total")
	}
	return nil
}

func signedSpreadBPS(bid, ask int64) (float64, error) {
	if bid <= 0 || ask <= 0 {
		return 0, nil
	}
	value := float64(ask-bid) * 20_000 / (float64(ask) + float64(bid))
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("spread BPS is not finite")
	}
	return value, nil
}

func venueName(id uint8) string {
	switch id {
	case 1:
		return "binance"
	case 2:
		return "okx"
	case 3:
		return "bybit"
	case 4:
		return "gate"
	case 5:
		return "bitget"
	case 6:
		return "polymarket"
	case 7:
		return "sse"
	case 8:
		return "hyperliquid"
	default:
		return fmt.Sprintf("venue-%d", id)
	}
}

func fixedText(value []byte) (string, error) {
	end := len(value)
	for index, b := range value {
		if b == 0 {
			end = index
			break
		}
	}
	for _, b := range value[end:] {
		if b != 0 {
			return "", fmt.Errorf("fixed string contains data after NUL")
		}
	}
	text := string(value[:end])
	if text == "" || strings.IndexFunc(text, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return "", fmt.Errorf("invalid fixed string")
	}
	return text, nil
}
