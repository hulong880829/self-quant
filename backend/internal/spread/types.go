package spread

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shopspring/decimal"
)

var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrUnavailable     = errors.New("basis spread unavailable")
	ErrTimeout         = errors.New("basis spread query timed out")
	ErrQueryFailed     = errors.New("basis spread query failed")
)

type Range string

const (
	Range1h  Range = "1h"
	Range4h  Range = "4h"
	Range8h  Range = "8h"
	Range24h Range = "24h"
	Range7d  Range = "7d"
)

type Availability string

const (
	AvailabilityAvailable   Availability = "available"
	AvailabilityUnavailable Availability = "unavailable"
)

type HistoryRequest struct {
	Venue                       string
	CompareVenue                string
	BaseAsset                   string
	QuoteAsset                  string
	VenueCanonicalSymbol        string
	CompareVenueCanonicalSymbol string
	Range                       Range
	Now                         time.Time
}

type Point struct {
	TS           time.Time
	SpreadBps    decimal.Decimal
	SpotAsk      decimal.Decimal
	PerpetualAsk decimal.Decimal
	Samples      int
}

type Summary struct {
	CurrentBps decimal.Decimal
	MinBps     decimal.Decimal
	MaxBps     decimal.Decimal
	AvgBps     decimal.Decimal
	Coverage   decimal.Decimal
}

type History struct {
	Venue             string
	CompareVenue      string
	BaseAsset         string
	QuoteAsset        string
	CanonicalSymbol   string
	Range             Range
	ResolutionSeconds int
	Availability      Availability
	AsOf              time.Time
	Points            []Point
	Summary           Summary
}

type bucketRow struct {
	Bucket     time.Time
	SpotAskRaw int64
	SpotScale  uint8
	PerpAskRaw int64
	PerpScale  uint8
	Samples    uint32
}

func ParseRange(value string) (Range, error) {
	switch Range(strings.ToLower(strings.TrimSpace(value))) {
	case Range1h, Range4h, Range8h, Range24h, Range7d:
		return Range(strings.ToLower(strings.TrimSpace(value))), nil
	default:
		return "", ErrInvalidArgument
	}
}

func (r Range) Window() time.Duration {
	switch r {
	case Range1h:
		return time.Hour
	case Range4h:
		return 4 * time.Hour
	case Range8h:
		return 8 * time.Hour
	case Range24h:
		return 24 * time.Hour
	case Range7d:
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

func (r Range) Resolution() time.Duration {
	switch r {
	case Range1h:
		return 5 * time.Second
	case Range4h:
		return 15 * time.Second
	case Range8h:
		return 30 * time.Second
	case Range24h:
		return time.Minute
	case Range7d:
		return 5 * time.Minute
	default:
		return 0
	}
}

func (r Range) ExpectedBuckets() int {
	resolution := r.Resolution()
	if resolution <= 0 {
		return 0
	}
	return int(r.Window() / resolution)
}

func CanonicalSymbol(baseAsset, quoteAsset string) string {
	return strings.ToUpper(strings.TrimSpace(baseAsset)) +
		strings.ToUpper(strings.TrimSpace(quoteAsset))
}

func NormalizeRequest(input HistoryRequest) (HistoryRequest, error) {
	venue := strings.ToLower(strings.TrimSpace(input.Venue))
	compareVenue := strings.ToLower(strings.TrimSpace(input.CompareVenue))
	base := strings.ToUpper(strings.TrimSpace(input.BaseAsset))
	quote := strings.ToUpper(strings.TrimSpace(input.QuoteAsset))
	venueSymbol := strings.ToUpper(strings.TrimSpace(input.VenueCanonicalSymbol))
	compareSymbol := strings.ToUpper(strings.TrimSpace(input.CompareVenueCanonicalSymbol))
	if !ValidVenue(venue) || !ValidAsset(base) || !ValidAsset(quote) {
		return HistoryRequest{}, ErrInvalidArgument
	}
	if compareVenue != "" && (!ValidVenue(compareVenue) || compareVenue == venue) {
		return HistoryRequest{}, ErrInvalidArgument
	}
	if venueSymbol == "" {
		venueSymbol = CanonicalSymbol(base, quote)
	}
	if compareSymbol == "" {
		compareSymbol = CanonicalSymbol(base, quote)
	}
	if !ValidCanonicalSymbol(venueSymbol) ||
		(compareVenue != "" && !ValidCanonicalSymbol(compareSymbol)) {
		return HistoryRequest{}, ErrInvalidArgument
	}
	parsed, err := ParseRange(string(input.Range))
	if err != nil {
		return HistoryRequest{}, err
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return HistoryRequest{
		Venue: venue, CompareVenue: compareVenue, BaseAsset: base, QuoteAsset: quote,
		VenueCanonicalSymbol: venueSymbol, CompareVenueCanonicalSymbol: compareSymbol,
		Range: parsed, Now: now,
	}, nil
}

func ValidVenue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	count := utf8.RuneCountInString(value)
	if count < 2 || count > 32 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func ValidAsset(value string) bool {
	return validLetterOrDigitToken(value, 1, 16)
}

func ValidCanonicalSymbol(value string) bool {
	return validLetterOrDigitToken(value, 2, 32)
}

func validLetterOrDigitToken(value string, minRunes, maxRunes int) bool {
	if !utf8.ValidString(value) {
		return false
	}
	count := utf8.RuneCountInString(value)
	if count < minRunes || count > maxRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func decodeAsk(raw int64, scale uint8) (decimal.Decimal, bool) {
	if raw <= 0 {
		return decimal.Zero, false
	}
	value := decimal.NewFromInt(raw)
	if scale > 0 {
		value = value.Shift(-int32(scale))
	}
	return value, value.IsPositive()
}

func spreadBps(perpAsk, spotAsk decimal.Decimal) (decimal.Decimal, bool) {
	if !spotAsk.IsPositive() || !perpAsk.IsPositive() {
		return decimal.Zero, false
	}
	return perpAsk.Div(spotAsk).Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(10000)), true
}

func coverage(paired, expected int) decimal.Decimal {
	if expected <= 0 || paired <= 0 {
		return decimal.Zero
	}
	if paired >= expected {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromInt(int64(paired)).Div(decimal.NewFromInt(int64(expected)))
}

func buildHistory(req HistoryRequest, rows []bucketRow) History {
	history := History{
		Venue: req.Venue, CompareVenue: req.CompareVenue,
		BaseAsset: req.BaseAsset, QuoteAsset: req.QuoteAsset,
		CanonicalSymbol: CanonicalSymbol(req.BaseAsset, req.QuoteAsset),
		Range:           req.Range, ResolutionSeconds: int(req.Range.Resolution().Seconds()),
		Availability: AvailabilityUnavailable, AsOf: req.Now,
		Points: make([]Point, 0, len(rows)),
	}
	var hasSpot bool
	var sum decimal.Decimal
	for _, row := range rows {
		spotAsk, spotOK := decodeAsk(row.SpotAskRaw, row.SpotScale)
		if spotOK {
			hasSpot = true
		}
		perpAsk, perpOK := decodeAsk(row.PerpAskRaw, row.PerpScale)
		if !spotOK || !perpOK {
			continue
		}
		bps, ok := spreadBps(perpAsk, spotAsk)
		if !ok {
			continue
		}
		history.Points = append(history.Points, Point{
			TS: row.Bucket.UTC(), SpreadBps: bps, SpotAsk: spotAsk,
			PerpetualAsk: perpAsk, Samples: int(row.Samples),
		})
		sum = sum.Add(bps)
	}
	if !hasSpot {
		history.Points = nil
		return history
	}
	history.Availability = AvailabilityAvailable
	if len(history.Points) == 0 {
		history.Summary.Coverage = coverage(0, req.Range.ExpectedBuckets())
		return history
	}
	minBps := history.Points[0].SpreadBps
	maxBps := history.Points[0].SpreadBps
	for _, point := range history.Points[1:] {
		minBps = decimal.Min(minBps, point.SpreadBps)
		maxBps = decimal.Max(maxBps, point.SpreadBps)
	}
	history.Summary = Summary{
		CurrentBps: history.Points[len(history.Points)-1].SpreadBps,
		MinBps:     minBps,
		MaxBps:     maxBps,
		AvgBps:     sum.Div(decimal.NewFromInt(int64(len(history.Points)))),
		Coverage:   coverage(len(history.Points), req.Range.ExpectedBuckets()),
	}
	return history
}
