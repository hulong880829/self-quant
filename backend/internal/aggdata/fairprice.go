package aggdata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const fairPriceModelVersion = "fp-v1"

type FairPriceConfig struct {
	Enabled             bool
	DepthK              int
	LambdaPerBP         float64
	ImbalanceAlpha      float64
	EWMATau             time.Duration
	ImpactNotional      float64
	VenueDominanceRatio float64
}

func DefaultFairPriceConfig() FairPriceConfig {
	return FairPriceConfig{
		Enabled:             true,
		DepthK:              20,
		LambdaPerBP:         0.10,
		ImbalanceAlpha:      1,
		EWMATau:             500 * time.Millisecond,
		VenueDominanceRatio: 0.90,
	}
}

func (c FairPriceConfig) validate() error {
	if c.DepthK <= 0 || c.DepthK > maxDepth {
		return fmt.Errorf("fair price depth must be within 1..%d", maxDepth)
	}
	if c.LambdaPerBP < 0 || math.IsNaN(c.LambdaPerBP) || math.IsInf(c.LambdaPerBP, 0) {
		return fmt.Errorf("fair price lambda must be finite and non-negative")
	}
	if c.ImbalanceAlpha < 0 || c.ImbalanceAlpha > 1 ||
		math.IsNaN(c.ImbalanceAlpha) || math.IsInf(c.ImbalanceAlpha, 0) {
		return fmt.Errorf("fair price imbalance alpha must be within 0..1")
	}
	if c.EWMATau <= 0 {
		return fmt.Errorf("fair price EWMA tau must be positive")
	}
	if c.ImpactNotional < 0 || math.IsNaN(c.ImpactNotional) || math.IsInf(c.ImpactNotional, 0) {
		return fmt.Errorf("fair price impact notional must be finite and non-negative")
	}
	if c.VenueDominanceRatio <= 0 || c.VenueDominanceRatio > 1 ||
		math.IsNaN(c.VenueDominanceRatio) || math.IsInf(c.VenueDominanceRatio, 0) {
		return fmt.Errorf("fair price venue dominance ratio must be within (0,1]")
	}
	return nil
}

type FixedValue struct {
	Mantissa int64
	Scale    uint8
}

type FairPriceSnapshot struct {
	Profile       string
	Symbol        string
	ModelID       string
	RingEpoch     uint64
	RingSequence  uint64
	Generation    uint64
	WallNS        uint64
	ExchangeTSNS  uint64
	PriceScale    uint8
	QuantityScale uint8
	Ready         bool
	ResetReason   string

	Price            FixedValue
	PriceRaw         FixedValue
	Mid              FixedValue
	Microprice       FixedValue
	BestBid          FixedValue
	BestAsk          FixedValue
	EffectiveBid     FixedValue
	EffectiveAsk     FixedValue
	ImpactBid        *FixedValue
	ImpactAsk        *FixedValue
	CrossedQuantity  *FixedValue
	Imbalance        float64
	SpreadBPS        float64
	CrossBPS         *float64
	WeightedBidDepth float64
	WeightedAskDepth float64
	DepthBidNotional float64
	DepthAskNotional float64
	ActiveVenueMask  uint32
	Crossed          bool
	Degraded         bool
	DegradedReasons  []string
	JSON             []byte
}

type fairModelState struct {
	WallNS uint64
	Price  float64
}

type fairPriceEngine struct {
	config  FairPriceConfig
	modelID string
}

func newFairPriceEngine(config FairPriceConfig) (*fairPriceEngine, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	canonical := fmt.Sprintf(
		"%s|k=%d|lambda=%.12g|alpha=%.12g|tau_ns=%d|impact=%.12g|dominance=%.12g",
		fairPriceModelVersion, config.DepthK, config.LambdaPerBP,
		config.ImbalanceAlpha, config.EWMATau.Nanoseconds(),
		config.ImpactNotional, config.VenueDominanceRatio,
	)
	hash := sha256.Sum256([]byte(canonical))
	return &fairPriceEngine{
		config:  config,
		modelID: fairPriceModelVersion + ":" + hex.EncodeToString(hash[:8]),
	}, nil
}

type residualLevel struct {
	price         int64
	quantity      int64
	venueQuantity [maxVenues]int64
}

func (e *fairPriceEngine) compute(
	profile, symbol string,
	snapshot *Snapshot,
	previous *fairModelState,
) (*FairPriceSnapshot, *fairModelState) {
	result := &FairPriceSnapshot{
		Profile: profile, Symbol: symbol, ModelID: e.modelID, Ready: false,
		DegradedReasons: []string{},
	}
	if snapshot == nil || snapshot.Book == nil || !snapshot.Ready {
		result.ResetReason = "orderbook_not_ready"
		return result, previous
	}
	result.RingEpoch = snapshot.RingEpoch
	result.RingSequence = snapshot.RingSequence
	result.Generation = snapshot.Generation
	result.WallNS = snapshot.WallNS
	book := snapshot.Book
	result.ExchangeTSNS = book.ExchangeTSNS
	result.PriceScale = book.PriceScale
	result.QuantityScale = book.QuantityScale
	result.ActiveVenueMask = book.ActiveMask
	if book.ActiveMask == 0 {
		result.ResetReason = "no_active_venues"
		return result, previous
	}
	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		result.ResetReason = "empty_side"
		return result, previous
	}
	if book.PriceScale > 18 || book.QuantityScale > 18 {
		result.ResetReason = "invalid_scale"
		return result, previous
	}

	bidDepth, askDepth := min(maxDepth, len(book.Bids)), min(maxDepth, len(book.Asks))
	bids := make([]residualLevel, bidDepth)
	asks := make([]residualLevel, askDepth)
	for index, level := range book.Bids[:bidDepth] {
		if level.Price <= 0 || level.Quantity <= 0 {
			result.ResetReason = "invalid_level"
			return result, previous
		}
		bids[index] = residualLevel{
			price: level.Price, quantity: level.Quantity,
			venueQuantity: level.VenueQuantity,
		}
	}
	for index, level := range book.Asks[:askDepth] {
		if level.Price <= 0 || level.Quantity <= 0 {
			result.ResetReason = "invalid_level"
			return result, previous
		}
		asks[index] = residualLevel{
			price: level.Price, quantity: level.Quantity,
			venueQuantity: level.VenueQuantity,
		}
	}
	originalBid, originalAsk := bids[0].price, asks[0].price
	result.Crossed = originalBid >= originalAsk
	var crossedQuantity int64
	if result.Crossed {
		var ok bool
		bids, asks, crossedQuantity, ok = virtualUncross(bids, asks)
		if !ok {
			result.ResetReason = "uncross_depth_exhausted"
			return result, previous
		}
		result.DegradedReasons = append(result.DegradedReasons, "book_crossed")
		result.CrossedQuantity = &FixedValue{Mantissa: crossedQuantity, Scale: book.QuantityScale}
	}
	if len(bids) == 0 || len(asks) == 0 || bids[0].price >= asks[0].price {
		result.ResetReason = "uncross_depth_exhausted"
		return result, previous
	}

	priceFactor := math.Pow10(int(book.PriceScale))
	quantityFactor := math.Pow10(int(book.QuantityScale))
	bid := float64(bids[0].price) / priceFactor
	ask := float64(asks[0].price) / priceFactor
	mid := (bid + ask) / 2
	if mid <= 0 || !finite(mid) {
		result.ResetReason = "invalid_numeric"
		return result, previous
	}
	topBidQuantity := float64(bids[0].quantity) / quantityFactor
	topAskQuantity := float64(asks[0].quantity) / quantityFactor
	topTotal := topBidQuantity + topAskQuantity
	if topTotal <= 0 || !finite(topTotal) {
		result.ResetReason = "invalid_numeric"
		return result, previous
	}
	topImbalance := topBidQuantity / topTotal
	microprice := topImbalance*ask + (1-topImbalance)*bid

	depth := e.config.DepthK
	bidCount, askCount := min(depth, len(bids)), min(depth, len(asks))
	if bidCount < depth || askCount < depth {
		result.DegradedReasons = append(result.DegradedReasons, "insufficient_depth")
	}
	var weightedBid, weightedAsk float64
	for _, level := range bids[:bidCount] {
		price := float64(level.price) / priceFactor
		quantity := float64(level.quantity) / quantityFactor
		weight := math.Exp(-e.config.LambdaPerBP * math.Abs(price-mid) / mid * 10_000)
		weightedBid += weight * quantity
		result.DepthBidNotional += price * quantity
	}
	for _, level := range asks[:askCount] {
		price := float64(level.price) / priceFactor
		quantity := float64(level.quantity) / quantityFactor
		weight := math.Exp(-e.config.LambdaPerBP * math.Abs(price-mid) / mid * 10_000)
		weightedAsk += weight * quantity
		result.DepthAskNotional += price * quantity
	}
	weightedTotal := weightedBid + weightedAsk
	if weightedTotal <= 0 || !finite(weightedTotal) {
		result.ResetReason = "invalid_numeric"
		return result, previous
	}
	imbalance := weightedBid / weightedTotal
	raw := mid + (imbalance-0.5)*(ask-bid)*e.config.ImbalanceAlpha
	raw = clamp(raw, bid, ask)
	if !finite(raw) || !finite(microprice) {
		result.ResetReason = "invalid_numeric"
		return result, previous
	}

	smoothed := raw
	if previous != nil && snapshot.WallNS > previous.WallNS {
		delta := time.Duration(snapshot.WallNS - previous.WallNS)
		weight := 1 - math.Exp(-float64(delta)/float64(e.config.EWMATau))
		smoothed = previous.Price + weight*(raw-previous.Price)
	}
	smoothed = clamp(smoothed, bid, ask)
	nextState := &fairModelState{WallNS: snapshot.WallNS, Price: smoothed}

	outputScale := book.PriceScale
	if outputScale <= 15 {
		outputScale += 3
	} else {
		outputScale = 18
	}
	var ok bool
	if result.Price, ok = fixedFromFloat(smoothed, outputScale); !ok {
		result.ResetReason = "fixed_overflow"
		return result, previous
	}
	values := []struct {
		target *FixedValue
		value  float64
	}{
		{&result.PriceRaw, raw},
		{&result.Mid, mid},
		{&result.Microprice, microprice},
		{&result.BestBid, float64(originalBid) / priceFactor},
		{&result.BestAsk, float64(originalAsk) / priceFactor},
		{&result.EffectiveBid, bid},
		{&result.EffectiveAsk, ask},
	}
	for _, value := range values {
		if *value.target, ok = fixedFromFloat(value.value, outputScale); !ok {
			result.ResetReason = "fixed_overflow"
			return result, previous
		}
	}

	if e.config.ImpactNotional > 0 {
		impactBid, bidOK := impactPrice(bids, e.config.ImpactNotional, priceFactor, quantityFactor)
		impactAsk, askOK := impactPrice(asks, e.config.ImpactNotional, priceFactor, quantityFactor)
		if impactBid > 0 {
			value, converted := fixedFromFloat(impactBid, outputScale)
			if !converted {
				result.ResetReason = "fixed_overflow"
				return result, previous
			}
			result.ImpactBid = &value
		}
		if impactAsk > 0 {
			value, converted := fixedFromFloat(impactAsk, outputScale)
			if !converted {
				result.ResetReason = "fixed_overflow"
				return result, previous
			}
			result.ImpactAsk = &value
		}
		if !bidOK || !askOK {
			result.DegradedReasons = append(result.DegradedReasons, "impact_depth_insufficient")
		}
	}
	if book.ActiveMask != book.MemberMask {
		result.DegradedReasons = append(result.DegradedReasons, "inactive_venues")
	}
	if venueDominant(bids, asks, e.config.DepthK, e.config.VenueDominanceRatio) {
		result.DegradedReasons = append(result.DegradedReasons, "single_venue_dominant")
	}
	result.Imbalance = imbalance
	result.WeightedBidDepth = weightedBid
	result.WeightedAskDepth = weightedAsk
	result.SpreadBPS = (ask - bid) / mid * 10_000
	if result.Crossed {
		crossBPS := (float64(originalBid) - float64(originalAsk)) * 20_000 /
			(float64(originalBid) + float64(originalAsk))
		result.CrossBPS = &crossBPS
	}
	result.Degraded = len(result.DegradedReasons) > 0
	result.Ready = true
	payload, err := json.Marshal(fairPriceJSON(result))
	if err != nil {
		result.Ready = false
		result.ResetReason = "json_encode_failed"
		return result, previous
	}
	result.JSON = payload
	return result, nextState
}

func virtualUncross(
	bids, asks []residualLevel,
) ([]residualLevel, []residualLevel, int64, bool) {
	bidIndex, askIndex := 0, 0
	var crossed int64
	for bidIndex < len(bids) && askIndex < len(asks) &&
		bids[bidIndex].price >= asks[askIndex].price {
		matched := min(bids[bidIndex].quantity, asks[askIndex].quantity)
		if matched <= 0 || crossed > math.MaxInt64-matched {
			return nil, nil, 0, false
		}
		crossed += matched
		consumeResidual(&bids[bidIndex], matched)
		consumeResidual(&asks[askIndex], matched)
		if bids[bidIndex].quantity == 0 {
			bidIndex++
		}
		if asks[askIndex].quantity == 0 {
			askIndex++
		}
	}
	if bidIndex >= len(bids) || askIndex >= len(asks) {
		return nil, nil, crossed, false
	}
	return bids[bidIndex:], asks[askIndex:], crossed, true
}

func consumeResidual(level *residualLevel, quantity int64) {
	level.quantity -= quantity
	remaining := quantity
	for index, contribution := range level.venueQuantity {
		consumed := min(contribution, remaining)
		level.venueQuantity[index] -= consumed
		remaining -= consumed
		if remaining == 0 {
			return
		}
	}
}

func impactPrice(
	levels []residualLevel,
	notional, priceFactor, quantityFactor float64,
) (float64, bool) {
	var accumulated, baseQuantity float64
	for _, level := range levels {
		price := float64(level.price) / priceFactor
		quantity := float64(level.quantity) / quantityFactor
		available := price * quantity
		used := math.Min(notional-accumulated, available)
		if used > 0 {
			accumulated += used
			baseQuantity += used / price
		}
		if accumulated >= notional {
			return accumulated / baseQuantity, finite(baseQuantity) && baseQuantity > 0
		}
	}
	if baseQuantity > 0 && finite(baseQuantity) {
		return accumulated / baseQuantity, false
	}
	return 0, false
}

func venueDominant(
	bids, asks []residualLevel,
	depth int,
	threshold float64,
) bool {
	var totals [maxVenues]float64
	var total float64
	accumulate := func(levels []residualLevel) {
		for _, level := range levels[:min(depth, len(levels))] {
			for index, quantity := range level.venueQuantity {
				totals[index] += float64(quantity)
				total += float64(quantity)
			}
		}
	}
	accumulate(bids)
	accumulate(asks)
	if total <= 0 {
		return false
	}
	for _, quantity := range totals {
		if quantity/total >= threshold {
			return true
		}
	}
	return false
}

func fixedFromFloat(value float64, scale uint8) (FixedValue, bool) {
	if scale > 18 || !finite(value) {
		return FixedValue{}, false
	}
	scaled := value * math.Pow10(int(scale))
	rounded := math.Round(scaled)
	if !finite(rounded) ||
		rounded >= float64(math.MaxInt64) ||
		rounded <= float64(math.MinInt64) {
		return FixedValue{}, false
	}
	return FixedValue{Mantissa: int64(rounded), Scale: scale}, true
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clamp(value, lower, upper float64) float64 {
	return math.Max(lower, math.Min(upper, value))
}

type fairFixedJSON struct {
	Mantissa string `json:"mantissa"`
	Scale    uint8  `json:"scale"`
}

type fairPriceMessage struct {
	Type             string         `json:"type"`
	Channel          string         `json:"channel"`
	Profile          string         `json:"profile"`
	Symbol           string         `json:"symbol"`
	ModelID          string         `json:"model_id"`
	RingEpoch        string         `json:"ring_epoch"`
	Sequence         string         `json:"seq"`
	Generation       string         `json:"generation"`
	WallNS           string         `json:"wall_ns"`
	ExchangeTSNS     string         `json:"exchange_ts_ns"`
	Price            fairFixedJSON  `json:"price"`
	PriceRaw         fairFixedJSON  `json:"price_raw"`
	Mid              fairFixedJSON  `json:"mid"`
	Microprice       fairFixedJSON  `json:"microprice"`
	BestBid          fairFixedJSON  `json:"best_bid"`
	BestAsk          fairFixedJSON  `json:"best_ask"`
	EffectiveBid     fairFixedJSON  `json:"effective_bid"`
	EffectiveAsk     fairFixedJSON  `json:"effective_ask"`
	ImpactBid        *fairFixedJSON `json:"impact_bid"`
	ImpactAsk        *fairFixedJSON `json:"impact_ask"`
	CrossedQuantity  *fairFixedJSON `json:"crossed_quantity"`
	Imbalance        float64        `json:"imbalance"`
	SpreadBPS        float64        `json:"spread_bps"`
	CrossBPS         *float64       `json:"cross_bps"`
	WeightedBidDepth float64        `json:"weighted_bid_depth"`
	WeightedAskDepth float64        `json:"weighted_ask_depth"`
	DepthBidNotional float64        `json:"depth_bid_notional"`
	DepthAskNotional float64        `json:"depth_ask_notional"`
	ActiveVenueMask  uint32         `json:"active_venue_mask"`
	Crossed          bool           `json:"crossed"`
	Degraded         bool           `json:"degraded"`
	DegradedReasons  []string       `json:"degraded_reasons"`
}

func fairPriceJSON(value *FairPriceSnapshot) fairPriceMessage {
	return fairPriceMessage{
		Type: "data", Channel: "fairprice",
		Profile: value.Profile, Symbol: value.Symbol, ModelID: value.ModelID,
		RingEpoch:    strconv.FormatUint(value.RingEpoch, 10),
		Sequence:     strconv.FormatUint(value.RingSequence, 10),
		Generation:   strconv.FormatUint(value.Generation, 10),
		WallNS:       strconv.FormatUint(value.WallNS, 10),
		ExchangeTSNS: strconv.FormatUint(value.ExchangeTSNS, 10),
		Price:        fixedJSONValue(value.Price), PriceRaw: fixedJSONValue(value.PriceRaw),
		Mid: fixedJSONValue(value.Mid), Microprice: fixedJSONValue(value.Microprice),
		BestBid: fixedJSONValue(value.BestBid), BestAsk: fixedJSONValue(value.BestAsk),
		EffectiveBid:     fixedJSONValue(value.EffectiveBid),
		EffectiveAsk:     fixedJSONValue(value.EffectiveAsk),
		ImpactBid:        optionalFixedJSON(value.ImpactBid),
		ImpactAsk:        optionalFixedJSON(value.ImpactAsk),
		CrossedQuantity:  optionalFixedJSON(value.CrossedQuantity),
		Imbalance:        value.Imbalance,
		SpreadBPS:        value.SpreadBPS,
		CrossBPS:         value.CrossBPS,
		WeightedBidDepth: value.WeightedBidDepth,
		WeightedAskDepth: value.WeightedAskDepth,
		DepthBidNotional: value.DepthBidNotional,
		DepthAskNotional: value.DepthAskNotional,
		ActiveVenueMask:  value.ActiveVenueMask, Crossed: value.Crossed,
		Degraded:        value.Degraded,
		DegradedReasons: append([]string(nil), value.DegradedReasons...),
	}
}

func fixedJSONValue(value FixedValue) fairFixedJSON {
	return fairFixedJSON{Mantissa: strconv.FormatInt(value.Mantissa, 10), Scale: value.Scale}
}

func optionalFixedJSON(value *FixedValue) *fairFixedJSON {
	if value == nil {
		return nil
	}
	result := fixedJSONValue(*value)
	return &result
}

func fixedDecimalString(value FixedValue) string {
	negative := value.Mantissa < 0
	digits := strconv.FormatInt(value.Mantissa, 10)
	if negative {
		digits = strings.TrimPrefix(digits, "-")
	}
	scale := int(value.Scale)
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		index := len(digits) - scale
		digits = digits[:index] + "." + digits[index:]
	}
	if negative {
		return "-" + digits
	}
	return digits
}
