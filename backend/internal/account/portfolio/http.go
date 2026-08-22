package portfolio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type upstreamHTTPError struct {
	StatusCode int
	Body       []byte
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
}

type flexDecimalString string

func (value *flexDecimalString) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*value = ""
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		var decoded string
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		raw = decoded
	}
	if _, err := decimal.NewFromString(raw); err != nil {
		return fmt.Errorf("invalid decimal %q", raw)
	}
	*value = flexDecimalString(raw)
	return nil
}

func getJSON(ctx context.Context, client *http.Client, rawURL string, headers http.Header, target any) error {
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header = headers.Clone()
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := json.Unmarshal(body, target); err != nil {
				return fmt.Errorf("decode upstream response: %w", err)
			}
			return nil
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout
		if !retryable || attempt == 3 {
			return &upstreamHTTPError{StatusCode: resp.StatusCode, Body: body}
		}
		delay := retryDelay(resp.Header.Get("Retry-After"), attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func retryDelay(value string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		if delay <= 5*time.Second {
			return delay
		}
		return 5 * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := time.Until(at)
		if delay > 0 && delay <= 5*time.Second {
			return delay
		}
	}
	return time.Duration(100*(1<<attempt)) * time.Millisecond
}

func hmacHex256(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func hmacBase64(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func hmacHex512(secret, payload string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func parseDecimal(value string) (decimal.Decimal, error) {
	if strings.TrimSpace(value) == "" {
		return decimal.Zero, fmt.Errorf("missing decimal")
	}
	result, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, fmt.Errorf("invalid decimal %q", value)
	}
	return result, nil
}

func absoluteDecimal(value string) (decimal.Decimal, error) {
	parsed, err := parseDecimal(value)
	if err != nil {
		return decimal.Zero, err
	}
	return parsed.Abs(), nil
}

func sideFromSigned(value decimal.Decimal) string {
	if value.IsNegative() {
		return "short"
	}
	return "long"
}

func signedQuantity(value decimal.Decimal, side string) decimal.Decimal {
	value = value.Abs()
	if strings.EqualFold(side, "short") {
		return value.Neg()
	}
	return value
}

func normalizeSymbol(value string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
}

func baseAssetSymbol(value string) string {
	value = normalizeSymbol(value)
	for _, suffix := range []string{"-PERP", "-SWAP"} {
		value = strings.TrimSuffix(value, suffix)
	}
	for _, suffix := range []string{"USDT", "USDC", "USD"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return strings.TrimRight(value, "-_")
}

func setSpotBalance(balances map[string]string, asset string, value decimal.Decimal) {
	asset = normalizeSymbol(asset)
	if asset == "" || value.IsZero() {
		return
	}
	if existing, ok := balances[asset]; ok {
		if current, err := decimal.NewFromString(existing); err == nil {
			value = current.Add(value)
		}
	}
	balances[asset] = value.String()
}

func attachQuantities(snapshot *Snapshot) {
	if snapshot.SpotBalances == nil {
		snapshot.SpotBalances = make(map[string]string)
	}
	for index := range snapshot.Positions {
		position := &snapshot.Positions[index]
		size, err := parseDecimal(position.Size)
		if err == nil {
			position.SignedContractSize = signedQuantity(size, position.Side).String()
		}
		position.SpotSize = snapshot.SpotBalances[baseAssetSymbol(position.Symbol)]
		if position.SpotSize == "" {
			position.SpotSize = "0"
		}
	}
}

func riskPercent(maintenance, equity string) string {
	mm, mmErr := parseDecimal(maintenance)
	eq, eqErr := parseDecimal(equity)
	if mmErr != nil || eqErr != nil || !eq.IsPositive() {
		return ""
	}
	return mm.Div(eq).Mul(decimal.NewFromInt(100)).String()
}
