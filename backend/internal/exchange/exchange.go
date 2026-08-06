package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Instrument struct {
	Exchange        string
	ExchangeSymbol  string
	BaseAsset       string
	QuoteAsset      string
	GlobalSymbol    string
	IntervalHours   float64
	SettleAsset     string
	ContractType    string
	Status          string
	ContractSize    float64
	PriceTick       float64
	QuantityStep    float64
	Metadata        json.RawMessage
	SourceUpdatedAt time.Time
}

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
	SyncInstruments(context.Context) ([]Instrument, error)
	FetchCurrent(context.Context, []Instrument) ([]FundingRate, error)
	FetchHistory(context.Context, Instrument, time.Time, int) ([]FundingRate, error)
}

type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string, timeout time.Duration) client {
	return client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}}
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

func GlobalSymbol(base, quote string) string {
	clean := func(value string) string {
		r := strings.NewReplacer("-", "", "_", "", "/", "", ":", "")
		return strings.ToUpper(r.Replace(value))
	}
	return clean(base) + clean(quote)
}

func parseFloat(value string) (float64, error) {
	var number json.Number = json.Number(value)
	return number.Float64()
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
