package exchange

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

func unmarshalJSON(raw []byte, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	return nil
}

func uncertainDecode(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) ||
		errors.Is(err, ErrRateLimited) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrUncertain, err)
}

type flexInt64 int64

func (v *flexInt64) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*v = 0
		return nil
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		*v = flexInt64(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return err
	}
	*v = flexInt64(parsed)
	return nil
}

func (v flexInt64) Int64() int64 {
	return int64(v)
}

// flexString preserves identifiers and decimal values that venues may encode
// as either JSON strings or numbers. Decoding through interface{} would turn
// large numeric order IDs into float64 and silently lose precision.
type flexString string

func (v *flexString) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*v = ""
		return nil
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
	} else {
		text = string(raw)
	}
	*v = flexString(strings.TrimSpace(text))
	return nil
}

func (v flexString) String() string {
	return string(v)
}

func canonicalDecimal(value string, positive bool) (string, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || (positive && !parsed.IsPositive()) {
		return "", fmt.Errorf("%w: invalid decimal value %q", ErrRejected, value)
	}
	return parsed.String(), nil
}

func canonicalDecimalMultiple(value, step string, positive bool) (string, error) {
	canonical, err := canonicalDecimal(value, positive)
	if err != nil {
		return "", err
	}
	step = strings.TrimSpace(step)
	if step == "" {
		return canonical, nil
	}
	parsed, _ := decimal.NewFromString(canonical)
	increment, stepErr := decimal.NewFromString(step)
	if stepErr != nil || !increment.IsPositive() {
		return "", fmt.Errorf("%w: invalid venue increment %q", ErrRejected, step)
	}
	if !parsed.Div(increment).Equal(parsed.Div(increment).Truncate(0)) {
		return "", fmt.Errorf(
			"%w: decimal value %s is not an exact multiple of %s",
			ErrRejected, canonical, increment.String(),
		)
	}
	return canonical, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func orderTimeInForce(request OrderRequest) string {
	if request.PostOnly {
		return "post_only"
	}
	return strings.ToLower(strings.TrimSpace(request.TimeInForce))
}

func bbo(bid, ask string, timestamp time.Time) (BBO, error) {
	if strings.TrimSpace(bid) == "" || strings.TrimSpace(ask) == "" {
		return BBO{}, fmt.Errorf("decode venue BBO: missing bid or ask")
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return BBO{BidPrice: bid, AskPrice: ask, Timestamp: timestamp}, nil
}

func unixTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}
	}
	if number < 1e12 {
		number *= 1000
	}
	millis := int64(number)
	return time.Unix(millis/1000, (millis%1000)*int64(time.Millisecond))
}

func timeNowUnix() int64 {
	return time.Now().Unix()
}

func stableHex(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	text := fmt.Sprintf("%x", sum[:])
	if length > 0 && length < len(text) {
		return text[:length]
	}
	return text
}

func quotientDecimal(numerator, denominator string) string {
	n, nErr := decimal.NewFromString(strings.TrimSpace(numerator))
	d, dErr := decimal.NewFromString(strings.TrimSpace(denominator))
	if nErr != nil || dErr != nil || d.IsZero() {
		return ""
	}
	return n.Div(d).String()
}

func requirePerpetual(instrument Instrument) error {
	if !strings.EqualFold(strings.TrimSpace(instrument.ContractType), "perpetual") {
		return fmt.Errorf("%w: only perpetual instruments are supported", ErrUnsupported)
	}
	return nil
}

func validateVenueOrderRules(request OrderRequest) error {
	step := request.Instrument.QuantityStep
	minimum := request.Instrument.MinQuantity
	maximum := request.Instrument.MaxQuantity
	minimumStatus := request.Instrument.MinQuantityStatus
	maximumStatus := request.Instrument.MaxQuantityStatus
	if strings.EqualFold(request.OrderType, "market") {
		if strings.TrimSpace(request.Instrument.MarketQuantityStep) != "" {
			step = request.Instrument.MarketQuantityStep
		}
		if request.Instrument.MarketMinQuantityStatus == ConstraintKnown {
			minimum, minimumStatus = request.Instrument.MarketMinQuantity, ConstraintKnown
		}
		if request.Instrument.MarketMaxQuantityStatus == ConstraintKnown {
			maximum, maximumStatus = request.Instrument.MarketMaxQuantity, ConstraintKnown
		}
	}
	quantity, err := decimal.NewFromString(strings.TrimSpace(request.Quantity))
	if err != nil || !quantity.IsPositive() {
		return fmt.Errorf("%w: invalid venue quantity", ErrInvalidQuantity)
	}
	if strings.TrimSpace(step) != "" {
		increment, incrementErr := decimal.NewFromString(step)
		if incrementErr != nil || !increment.IsPositive() ||
			!quantity.Div(increment).Equal(quantity.Div(increment).Truncate(0)) {
			return fmt.Errorf("%w: quantity does not match venue step", ErrInvalidQuantity)
		}
	}
	if minimumStatus == ConstraintKnown && strings.TrimSpace(minimum) != "" {
		value, valueErr := decimal.NewFromString(minimum)
		if valueErr != nil || quantity.LessThan(value) {
			return fmt.Errorf("%w: quantity below venue minimum", ErrInvalidQuantity)
		}
	}
	if maximumStatus == ConstraintKnown && strings.TrimSpace(maximum) != "" {
		value, valueErr := decimal.NewFromString(maximum)
		if valueErr != nil || quantity.GreaterThan(value) {
			return fmt.Errorf("%w: quantity above venue maximum", ErrInvalidQuantity)
		}
	}
	if !strings.EqualFold(request.OrderType, "limit") {
		return nil
	}
	price, err := decimal.NewFromString(strings.TrimSpace(request.Price))
	if err != nil || !price.IsPositive() {
		return fmt.Errorf("%w: invalid venue price", ErrRejected)
	}
	if tick := strings.TrimSpace(request.Instrument.PriceTick); tick != "" {
		increment, incrementErr := decimal.NewFromString(tick)
		if incrementErr != nil || !increment.IsPositive() ||
			!price.Div(increment).Equal(price.Div(increment).Truncate(0)) {
			return fmt.Errorf("%w: price does not match venue tick", ErrRejected)
		}
	}
	if !request.ReduceOnly &&
		request.Instrument.MinNotionalStatus == ConstraintKnown &&
		strings.TrimSpace(request.Instrument.MinNotional) != "" {
		minimum, minimumErr := decimal.NewFromString(request.Instrument.MinNotional)
		if minimumErr != nil || quantity.Mul(price).LessThan(minimum) {
			return fmt.Errorf("%w: order below venue minimum notional", ErrInvalidQuantity)
		}
	}
	return nil
}

func errorsIsOrderNotFound(err error) bool {
	return errors.Is(err, ErrOrderNotFound)
}

func requireOrderLookupID(request QueryRequest) error {
	if strings.TrimSpace(request.VenueOrderID) == "" &&
		strings.TrimSpace(request.ClientOrderID) == "" {
		return fmt.Errorf("%w: missing venue and client order id", ErrUncertain)
	}
	return nil
}

func resolvedVenueResult(result Result) bool {
	status := strings.ToLower(strings.TrimSpace(result.Status))
	return status != "" && status != "unknown"
}

func unknownQueryError(result Result, err error) (Result, error) {
	if err == nil {
		return result, nil
	}
	result.Status = "unknown"
	return result, err
}

func exactOrderResolution(result Result, err error) (OrderResolution, error) {
	if err == nil && resolvedVenueResult(result) {
		return OrderResolution{
			Result: result,
			Found:  true,
			Active: !terminalOrderStatus(result.Status),
		}, nil
	}
	result, err = unknownQueryError(result, err)
	if err == nil || errors.Is(err, ErrOrderNotFound) {
		result.Status = "unknown"
		return OrderResolution{Result: result, ConfirmedAbsent: true}, nil
	}
	return OrderResolution{Result: result}, err
}

func sanitizeVenueHealth(err error) string {
	if err == nil {
		return ""
	}
	return "venue health check failed"
}

type venueCircuit struct {
	mu       sync.Mutex
	accounts map[string]venueCircuitState
}

type venueCircuitState struct {
	failures  int
	openUntil time.Time
}

func newVenueCircuit() *venueCircuit {
	return &venueCircuit{accounts: make(map[string]venueCircuitState)}
}

func (c *venueCircuit) before(credentials Credentials, now time.Time) error {
	if c == nil {
		return nil
	}
	key := venueAccountKey(credentials)
	c.mu.Lock()
	state := c.accounts[key]
	c.mu.Unlock()
	if now.Before(state.openUntil) {
		return fmt.Errorf("%w: venue adapter temporarily unavailable", ErrRateLimited)
	}
	return nil
}

func (c *venueCircuit) record(credentials Credentials, err error, now time.Time) {
	if c == nil {
		return
	}
	key := venueAccountKey(credentials)
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.accounts[key]
	if err == nil {
		delete(c.accounts, key)
		return
	}
	if !circuitFailure(err) {
		return
	}
	state.failures++
	if state.failures >= 5 {
		exponent := min(state.failures-5, 4)
		state.openUntil = now.Add(5 * time.Second * time.Duration(1<<exponent))
	}
	c.accounts[key] = state
}

func (c *venueCircuit) open(credentials Credentials, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.accounts[venueAccountKey(credentials)].openUntil)
}

func circuitFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUncertain) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sign") || strings.Contains(message, "nonce") ||
		strings.Contains(message, "timestamp")
}

func venueAccountKey(credentials Credentials) string {
	value := strings.Join([]string{
		credentials.CredentialKind,
		credentials.APIKey,
		credentials.SigningAddress,
		credentials.VaultAddress,
		formatOptionalInt64(credentials.AccountIndex),
		formatOptionalInt32(credentials.APIKeyIndex),
	}, "\x00")
	return stableHex(value, 32)
}

func formatOptionalInt64(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}

func formatOptionalInt32(value *int32) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(int64(*value), 10)
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func int32Value(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func trustedCancelTerminal(result Result) bool {
	switch normalizeStatus(result.Status) {
	case "filled", "canceled", "expired":
		return strings.TrimSpace(result.FilledQuantity) != ""
	default:
		return false
	}
}

func commandOnlyCancelResult(result Result, err error) (Result, error) {
	if normalizeStatus(result.Status) == "rejected" {
		result.Status = "unknown"
	}
	if err != nil {
		if strings.TrimSpace(result.Status) == "" {
			result.Status = "unknown"
		}
		result.LocalCommandAck = false
		return result, err
	}
	if trustedCancelTerminal(result) {
		result.LocalCommandAck = false
		return result, nil
	}
	result.Status = "pending"
	result.LocalCommandAck = true
	return result, nil
}

func getOrderAfterCancel(
	ctx context.Context,
	get func(context.Context, Credentials, QueryRequest) (Result, error),
	credentials Credentials,
	request CancelRequest,
	accepted Result,
	venue string,
) (Result, error) {
	latest, queryErr := get(ctx, credentials, QueryRequest{
		Instrument:    request.Instrument,
		ClientOrderID: request.ClientOrderID,
		VenueOrderID:  firstNonEmpty(accepted.VenueOrderID, request.VenueOrderID),
	})
	if queryErr == nil && terminalOrderStatus(latest.Status) {
		return latest, nil
	}
	if queryErr == nil {
		queryErr = fmt.Errorf("order remains %s", latest.Status)
		accepted = latest
	}
	if accepted.Status == "unknown" {
		accepted.Status = "pending"
	}
	return accepted, fmt.Errorf(
		"%w: %s cancel accepted but final state query failed: %v",
		ErrUncertain, venue, queryErr,
	)
}
