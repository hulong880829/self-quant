package funding

import "testing"

func sixVenueBTCRates() []Rate {
	return []Rate{
		{Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1},
		{Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1},
		{Exchange: "bybit", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1},
		{Exchange: "bybit", ExchangeSymbol: "BTCUSDC", GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 8, ContractMultiplier: 1},
		{Exchange: "bitget", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1},
		{Exchange: "gate", ExchangeSymbol: "BTC_USDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 0.0001},
		{Exchange: "hyperliquid", ExchangeSymbol: "BTC", GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1},
	}
}

func spreadPairSet(spreads []Spread) map[string]bool {
	result := make(map[string]bool, len(spreads))
	for _, spread := range spreads {
		left := spread.Long.Exchange + "/" + spread.Long.ExchangeSymbol
		right := spread.Short.Exchange + "/" + spread.Short.ExchangeSymbol
		if left > right {
			left, right = right, left
		}
		result[left+"|"+right] = true
	}
	return result
}

func TestSixVenuePairSetUnchangedWhenNewVenuesAreAbsent(t *testing.T) {
	got := spreadPairSet(BuildSpreads(sixVenueBTCRates()))
	if len(got) != 15 {
		t.Fatalf("six-venue BTC pairs=%d set=%v", len(got), got)
	}
	if _, ok := got["bybit/BTCUSDC|hyperliquid/BTC"]; ok {
		t.Fatal("HL must not pair Bybit USDC")
	}
	if _, ok := got["binance/BTCUSDT|hyperliquid/BTC"]; !ok {
		t.Fatal("HL must still pair Binance USDT")
	}
}

func TestPairableRatesAllowsHyperliquidWithLighterUSDCOnly(t *testing.T) {
	hl := Rate{Exchange: "hyperliquid", ExchangeSymbol: "BTC", GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1}
	lighter := Rate{Exchange: "lighter", ExchangeSymbol: "BTC", GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1}
	bybitUSDC := Rate{Exchange: "bybit", ExchangeSymbol: "BTCUSDC", GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 8, ContractMultiplier: 1}
	asterUSDC := Rate{Exchange: "aster", ExchangeSymbol: "BTCUSDC", GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 8, ContractMultiplier: 1}
	binance := Rate{Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1}
	if !PairableRates(hl, lighter) {
		t.Fatal("HL USDC must pair Lighter USDC")
	}
	if PairableRates(hl, bybitUSDC) {
		t.Fatal("HL USDC must not pair Bybit USDC")
	}
	if PairableRates(hl, asterUSDC) {
		t.Fatal("HL USDC must not pair Aster USDC")
	}
	if !PairableRates(lighter, binance) {
		t.Fatal("Lighter USDC must pair CEX USDT")
	}
}

func TestPairableRatesComparesMultiplierOnlyForNewVenues(t *testing.T) {
	gate := Rate{Exchange: "gate", ExchangeSymbol: "BTC_USDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 0.0001}
	binance := Rate{Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1}
	aster := Rate{Exchange: "aster", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1}
	asterQuanto := Rate{Exchange: "aster", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 0.01}
	if !PairableRates(gate, binance) {
		t.Fatal("existing six venues must ignore contract_size")
	}
	if !PairableRates(aster, binance) {
		t.Fatal("matching multiplier should pair")
	}
	if PairableRates(asterQuanto, binance) {
		t.Fatal("different new-venue multiplier must not pair")
	}
	if NormalizeContractMultiplier(0) != 1 || NormalizeContractMultiplier(-2) != 1 {
		t.Fatal("missing multiplier must normalize to 1")
	}
}

func TestPairableRatesAllowsBinanceTradifiWithHyperliquidHIP3(t *testing.T) {
	binance := Rate{
		Exchange: "binance", ExchangeSymbol: "ZHIPUUSDT", GlobalSymbol: "ZHIPUUSDT",
		BaseAsset: "ZHIPU", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1,
		VenueContractType: VenueContractTypeTradifi,
	}
	hip3 := Rate{
		Exchange: "hyperliquid", ExchangeSymbol: "xyz:ZHIPU", GlobalSymbol: "ZHIPUUSDC",
		BaseAsset: "ZHIPU", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
		VenueContractType: VenueContractTypeHIP3,
	}
	if PairingSymbol(hip3) != "ZHIPUUSDT" {
		t.Fatalf("pairing symbol=%q", PairingSymbol(hip3))
	}
	if !PairableRates(binance, hip3) {
		t.Fatal("Binance TradFi must pair Hyperliquid HIP-3 on the same base")
	}
}

func TestBuildSpreadsDedupsLighterDualKey(t *testing.T) {
	rates := append(sixVenueBTCRates(), Rate{
		Exchange: "lighter", ExchangeSymbol: "BTC", GlobalSymbol: "BTCUSDC",
		BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
	})
	spreads := BuildSpreads(rates)
	pairs := spreadPairSet(spreads)
	if !pairs["hyperliquid/BTC|lighter/BTC"] {
		t.Fatalf("missing HL/Lighter pair: %v", pairs)
	}
	if _, ok := pairs["bybit/BTCUSDC|hyperliquid/BTC"]; ok {
		t.Fatal("HL still must not pair Bybit USDC")
	}
	seen := map[string]int{}
	for _, spread := range spreads {
		if (spread.Long.Exchange == "lighter" && spread.Short.Exchange == "binance") ||
			(spread.Long.Exchange == "binance" && spread.Short.Exchange == "lighter") {
			seen["binance-lighter"]++
		}
	}
	if seen["binance-lighter"] != 1 {
		t.Fatalf("lighter/binance pairs=%d", seen["binance-lighter"])
	}
}

func TestRankingCanonicalAndHistorySourceSymbols(t *testing.T) {
	for _, test := range []struct {
		rate      Rate
		canonical string
		source    string
	}{
		{
			rate: Rate{
				Exchange: "aster", GlobalSymbol: "BTCUSDT",
				BaseAsset: "BTC", QuoteAsset: "USDT",
			},
			canonical: "BTCUSDT", source: "BTCUSDT",
		},
		{
			rate: Rate{
				Exchange: "lighter", GlobalSymbol: "BTCUSDC",
				BaseAsset: "BTC", QuoteAsset: "USDC",
			},
			canonical: "BTCUSDT", source: "BTCUSDC",
		},
		{
			rate: Rate{
				Exchange: "hyperliquid", GlobalSymbol: "ETHUSDC",
				BaseAsset: "ETH", QuoteAsset: "USDC",
			},
			canonical: "ETHUSDT", source: "ETHUSDC",
		},
	} {
		if got := RankingCanonicalSymbol(test.rate); got != test.canonical {
			t.Fatalf("canonical=%q want=%q", got, test.canonical)
		}
		if got := HistorySourceSymbol(test.rate); got != test.source {
			t.Fatalf("source=%q want=%q", got, test.source)
		}
	}
}

func TestPairableRatesRejectsMissingNewVenueMultiplier(t *testing.T) {
	aster := Rate{
		Exchange: "aster", GlobalSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	binance := Rate{
		Exchange: "binance", GlobalSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", ContractMultiplier: 1,
	}
	if PairableRates(aster, binance) {
		t.Fatal("missing Aster multiplier must be rejected")
	}
}

func entropyANTH() Rate {
	return Rate{
		Exchange: "entropy", ExchangeSymbol: "io:ANTH", GlobalSymbol: "ANTHUSDC",
		BaseAsset: "ANTH", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
		VenueContractType: VenueContractTypeHIP3,
	}
}

func TestPairableRatesAllowsConfirmedEntropySymbols(t *testing.T) {
	anth := entropyANTH()
	binance := Rate{
		Exchange: "binance", ExchangeSymbol: "ANTHUSDT", GlobalSymbol: "ANTHUSDT",
		BaseAsset: "ANTH", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1,
	}
	hl := Rate{
		Exchange: "hyperliquid", ExchangeSymbol: "ANTH", GlobalSymbol: "ANTHUSDC",
		BaseAsset: "ANTH", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
	}
	lighter := Rate{
		Exchange: "lighter", ExchangeSymbol: "ANTH", GlobalSymbol: "ANTHUSDC",
		BaseAsset: "ANTH", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
	}
	bybitUSDC := Rate{
		Exchange: "bybit", ExchangeSymbol: "ANTHUSDC", GlobalSymbol: "ANTHUSDC",
		BaseAsset: "ANTH", QuoteAsset: "USDC", IntervalHours: 8, ContractMultiplier: 1,
	}
	if PairingSymbol(anth) != "ANTHUSDT" {
		t.Fatalf("pairing symbol=%q", PairingSymbol(anth))
	}
	if !PairableRates(anth, binance) {
		t.Fatal("allowlisted Entropy must pair CEX USDT")
	}
	if !PairableRates(anth, hl) {
		t.Fatal("allowlisted Entropy must pair Hyperliquid USDC")
	}
	if !PairableRates(anth, lighter) {
		t.Fatal("allowlisted Entropy must pair Lighter USDC")
	}
	if PairableRates(anth, bybitUSDC) {
		t.Fatal("Entropy must not pair Bybit USDC")
	}
	if PairableRates(anth, anth) {
		t.Fatal("Entropy must not pair with itself")
	}
}

func TestPairableRatesRejectsUnconfirmedEntropySymbols(t *testing.T) {
	unknown := Rate{
		Exchange: "entropy", ExchangeSymbol: "io:UNKNOWN", GlobalSymbol: "UNKNOWNUSDC",
		BaseAsset: "UNKNOWN", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
	}
	binance := Rate{
		Exchange: "binance", ExchangeSymbol: "UNKNOWNUSDT", GlobalSymbol: "UNKNOWNUSDT",
		BaseAsset: "UNKNOWN", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1,
	}
	hl := Rate{
		Exchange: "hyperliquid", ExchangeSymbol: "UNKNOWN", GlobalSymbol: "UNKNOWNUSDC",
		BaseAsset: "UNKNOWN", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
	}
	if PairableRates(unknown, binance) || PairableRates(unknown, hl) {
		t.Fatal("unconfirmed Entropy symbols must stay single-venue")
	}
}

func TestBuildSpreadsKeepsExistingPairsWhenEntropyIsAbsent(t *testing.T) {
	original := spreadPairSet(BuildSpreads(sixVenueBTCRates()))
	unknown := Rate{
		Exchange: "entropy", ExchangeSymbol: "io:UNKNOWN", GlobalSymbol: "BTCUSDC",
		BaseAsset: "BTC", QuoteAsset: "USDC", IntervalHours: 1, ContractMultiplier: 1,
	}
	withUnknown := append(sixVenueBTCRates(), unknown)
	got := spreadPairSet(BuildSpreads(withUnknown))
	if len(got) != len(original) {
		t.Fatalf("unlisted Entropy must not add spreads: original=%d got=%d extra=%v", len(original), len(got), got)
	}
	for key := range original {
		if !got[key] {
			t.Fatalf("missing original pair %s", key)
		}
	}
}

func TestBuildSpreadsAddsAllowlistedEntropyWithoutChangingOriginalSet(t *testing.T) {
	original := spreadPairSet(BuildSpreads(sixVenueBTCRates()))
	anth := entropyANTH()
	binanceANTH := Rate{
		Exchange: "binance", ExchangeSymbol: "ANTHUSDT", GlobalSymbol: "ANTHUSDT",
		BaseAsset: "ANTH", QuoteAsset: "USDT", IntervalHours: 8, ContractMultiplier: 1,
	}
	rates := append(sixVenueBTCRates(), anth, binanceANTH)
	spreads := BuildSpreads(rates)
	got := spreadPairSet(spreads)
	if !got["binance/ANTHUSDT|entropy/io:ANTH"] {
		t.Fatalf("missing Entropy/Binance pair: %v", got)
	}
	filtered := make([]Spread, 0, len(spreads))
	for _, spread := range spreads {
		if spread.Long.Exchange == "entropy" || spread.Short.Exchange == "entropy" {
			continue
		}
		filtered = append(filtered, spread)
	}
	withoutEntropy := spreadPairSet(filtered)
	if len(withoutEntropy) != len(original) {
		t.Fatalf("original pairs changed: original=%d remaining=%d", len(original), len(withoutEntropy))
	}
	for key := range original {
		if !withoutEntropy[key] {
			t.Fatalf("original pair dropped: %s", key)
		}
	}
}
