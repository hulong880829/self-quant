package funding

import "strings"

const MaxRateLookupKeys = 256

const (
	RateLookupHit     = "hit"
	RateLookupMissing = "missing"
)

type RateLookupKey struct {
	Exchange       string
	ExchangeSymbol string
	BaseAsset      string
	QuoteAsset     string
}

func NormalizeRateLookupKey(key RateLookupKey) RateLookupKey {
	return RateLookupKey{
		Exchange:       strings.TrimSpace(key.Exchange),
		ExchangeSymbol: strings.TrimSpace(key.ExchangeSymbol),
		BaseAsset:      strings.TrimSpace(key.BaseAsset),
		QuoteAsset:     strings.TrimSpace(key.QuoteAsset),
	}
}

func ExactRateKey(exchange, exchangeSymbol string) string {
	return strings.ToLower(strings.TrimSpace(exchange)) + "\x00" +
		strings.ToLower(strings.TrimSpace(exchangeSymbol))
}

func FallbackRateKey(exchange, baseAsset, quoteAsset string) string {
	return strings.ToLower(strings.TrimSpace(exchange)) + "\x00" +
		baseAsset + "\x00" + quoteAsset
}

func (s *Snapshot) LookupRate(key RateLookupKey) (Rate, bool) {
	if s == nil || len(s.Rates) == 0 {
		return Rate{}, false
	}
	key = NormalizeRateLookupKey(key)
	if key.Exchange == "" {
		return Rate{}, false
	}
	if key.ExchangeSymbol != "" {
		if index, ok := s.exact[ExactRateKey(key.Exchange, key.ExchangeSymbol)]; ok {
			return s.Rates[index], true
		}
	}
	if key.BaseAsset == "" || key.QuoteAsset == "" {
		return Rate{}, false
	}
	index, ok := s.fallback[FallbackRateKey(key.Exchange, key.BaseAsset, key.QuoteAsset)]
	if !ok {
		return Rate{}, false
	}
	return s.Rates[index], true
}
