package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// ErrNoSettledHistory means the venue confirmed this contract has no settled
// funding rows yet. Callers treat it as a normal empty listing, not a fetch failure.
var ErrNoSettledHistory = errors.New("no settled funding history")

type Instrument struct {
	Exchange                 string
	ExchangeSymbol           string
	BaseAsset                string
	QuoteAsset               string
	GlobalSymbol             string
	IntervalHours            float64
	SettleAsset              string
	ContractType             string
	Status                   string
	ContractSize             float64
	PriceTick                float64
	QuantityStep             float64
	MinQuantity              float64
	MinNotional              float64
	MinQuantityStatus        string
	MinNotionalStatus        string
	MaxQuantity              string
	MarketQuantityStep       string
	MarketMinQuantity        string
	MarketMaxQuantity        string
	MarketMinNotional        string
	MaxQuantityStatus        string
	MarketQuantityStepStatus string
	MarketMinQuantityStatus  string
	MarketMaxQuantityStatus  string
	MarketMinNotionalStatus  string
	Metadata                 json.RawMessage
	SourceUpdatedAt          time.Time
}

const (
	ContractTypeSpot        = "spot"
	ContractTypePerpetual   = "perpetual"
	ConstraintKnown         = "known"
	ConstraintNotApplicable = "not_applicable"
	ConstraintUnknown       = "unknown"
)

type FundingRate struct {
	Exchange                string
	ExchangeSymbol          string
	Rate                    float64
	FundingTime             time.Time
	Settled                 bool
	IntervalHours           float64
	NextRate                *float64
	MarkPrice               float64
	IndexPrice              float64
	LastPrice               float64
	OpenInterestContracts   float64
	OpenInterestBase        float64
	OpenInterestNotionalUSD float64
	Volume24hBase           float64
	Turnover24hUSD          float64
	PriceChange24h          float64
	SourceUpdatedAt         time.Time
}

type Adapter interface {
	Name() string
	SyncInstruments(context.Context, string) ([]Instrument, error)
	FetchCurrent(context.Context, []Instrument) ([]FundingRate, error)
	FetchHistory(context.Context, Instrument, time.Time, int) ([]FundingRate, error)
}

// ContractTypeSupport lets an adapter advertise which instrument catalogs it
// can refresh. Adapters that omit it still sync spot and perpetual.
type ContractTypeSupport interface {
	SupportedContractTypes() []string
}

func AdapterContractTypes(adapter Adapter) []string {
	if support, ok := adapter.(ContractTypeSupport); ok {
		if types := support.SupportedContractTypes(); len(types) > 0 {
			return append([]string(nil), types...)
		}
	}
	return []string{ContractTypeSpot, ContractTypePerpetual}
}

type client struct {
	baseURL string
	http    *http.Client
	limiter *requestLimiter
}

func newClient(baseURL string, timeout time.Duration) client {
	interval := 100 * time.Millisecond
	if strings.Contains(baseURL, "hyperliquid") {
		interval = 1200 * time.Millisecond
	}
	return client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
		limiter: &requestLimiter{interval: interval},
	}
}

type requestLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func (l *requestLimiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	wait := l.next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	l.next = now.Add(wait).Add(l.interval)
	l.mu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c client) post(ctx context.Context, path string, body string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c client) do(req *http.Request, out any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var retryAfter time.Duration
		if err := c.limiter.wait(req.Context()); err != nil {
			return err
		}
		attemptRequest := req.Clone(req.Context())
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return err
			}
			attemptRequest.Body = body
		}
		res, err := c.http.Do(attemptRequest)
		if err != nil {
			lastErr = err
		} else if res.StatusCode >= 200 && res.StatusCode < 300 {
			decodeErr := json.NewDecoder(res.Body).Decode(out)
			res.Body.Close()
			if decodeErr != nil {
				return fmt.Errorf("decode %s: %w", req.URL.Host, decodeErr)
			}
			return nil
		} else {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			retryAfter = parseRetryAfter(res.Header.Get("Retry-After"))
			res.Body.Close()
			lastErr = fmt.Errorf(
				"%s: HTTP %d: %s",
				req.URL.Host, res.StatusCode, strings.TrimSpace(string(body)),
			)
			if res.StatusCode != http.StatusTooManyRequests && res.StatusCode < 500 {
				return lastErr
			}
		}
		if attempt < 2 {
			backoff := time.Duration(1<<attempt)*200*time.Millisecond +
				time.Duration(time.Now().UnixNano()%int64(100*time.Millisecond))
			if retryAfter > backoff {
				backoff = retryAfter
			}
			timer := time.NewTimer(backoff)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return req.Context().Err()
			case <-timer.C:
			}
		}
	}
	return lastErr
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if duration := time.Until(deadline); duration > 0 {
			return duration
		}
	}
	return 0
}

func waitAfterBatch(ctx context.Context, processed int) error {
	if processed == 0 || processed%100 != 0 {
		return nil
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func GlobalSymbol(base, quote string) string {
	clean := func(value string) string {
		r := strings.NewReplacer("-", "", "_", "", "/", "", ":", "")
		return strings.ToUpper(r.Replace(value))
	}
	return clean(base) + clean(quote)
}

func instrumentMetadata(value any, model, sizeUnit string) json.RawMessage {
	raw, _ := json.Marshal(value)
	metadata := make(map[string]any)
	_ = json.Unmarshal(raw, &metadata)
	metadata["contractModel"] = model
	metadata["positionSizeUnit"] = sizeUnit
	result, _ := json.Marshal(metadata)
	return result
}

func parseFloat(value string) (float64, error) {
	var number json.Number = json.Number(value)
	return number.Float64()
}

func knownConstraint(value string) (float64, string) {
	parsed, err := parseFloat(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, ConstraintUnknown
	}
	return parsed, ConstraintKnown
}

func knownDecimalConstraint(value string) (string, string) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || !parsed.IsPositive() {
		return "", ConstraintUnknown
	}
	return parsed.String(), ConstraintKnown
}

func maximumDecimalConstraint(value string) (string, string) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", ConstraintUnknown
	}
	parsed, err := decimal.NewFromString(trimmed)
	if err != nil || parsed.IsNegative() {
		return "", ConstraintUnknown
	}
	if parsed.IsZero() {
		return "", ConstraintNotApplicable
	}
	return parsed.String(), ConstraintKnown
}

func mustParsePositiveFloat(value string, fallback float64) float64 {
	parsed, err := parseFloat(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func milliseconds(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func seconds(value int64) time.Time {
	return time.Unix(value, 0).UTC()
}

func clampLimit(limit, maximum int) int {
	if limit <= 0 || limit > maximum {
		return maximum
	}
	return limit
}

func floatPointer(value float64) *float64 { return &value }
