package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	hltypes "github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/shopspring/decimal"
)

const (
	bitgetPreviewMarginMode = "cross"
	bitgetSetMarginMode     = "crossed"
)

func (a *binanceAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	if instrument.ContractType == "spot" {
		return LeverageApplyResult{}, nil
	}
	values := url.Values{
		"symbol":   {instrument.ExchangeSymbol},
		"leverage": {leverage.Truncate(0).String()},
	}
	raw, err := a.signed(ctx, http.MethodPost, "/papi/v1/um/leverage", credentials, values, nil)
	if err != nil {
		return LeverageApplyResult{}, err
	}
	return parseBinanceLeverageAck(raw, instrument, leverage)
}

func (a *okxAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	if instrument.ContractType == "spot" {
		return LeverageApplyResult{}, nil
	}
	body := compactJSON(map[string]string{
		"instId":  okxInstID(instrument),
		"lever":   leverage.Truncate(0).String(),
		"mgnMode": "cross",
	})
	raw, err := a.signed(ctx, http.MethodPost, "/api/v5/account/set-leverage", credentials, body, nil)
	if err != nil {
		return LeverageApplyResult{}, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return LeverageApplyResult{}, uncertainDecode(err)
	}
	if payload.Code != "" && payload.Code != "0" {
		return LeverageApplyResult{}, fmt.Errorf("%w: %s", ErrRejected, payload.Msg)
	}
	return LeverageApplyResult{}, nil
}

func (a *bybitAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	if instrument.ContractType == "spot" {
		return LeverageApplyResult{}, nil
	}
	body := compactJSON(map[string]string{
		"category":     bybitCategory(instrument),
		"symbol":       instrument.ExchangeSymbol,
		"buyLeverage":  leverage.Truncate(0).String(),
		"sellLeverage": leverage.Truncate(0).String(),
	})
	raw, err := a.signed(ctx, http.MethodPost, "/v5/position/set-leverage", credentials, body, nil)
	if err != nil {
		return LeverageApplyResult{}, err
	}
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return LeverageApplyResult{}, uncertainDecode(err)
	}
	switch payload.RetCode {
	case 0:
		return LeverageApplyResult{}, nil
	case 110043:
		return LeverageApplyResult{}, nil
	default:
		return LeverageApplyResult{}, fmt.Errorf(
			"%w: bybit retCode=%d retMsg=%s",
			ErrRejected,
			payload.RetCode,
			payload.RetMsg,
		)
	}
}

func bitgetEstMaxOpenUnit(instrument Instrument) string {
	if strings.EqualFold(instrument.ContractType, "spot") {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(metadataString(instrument, "contractModel")), "inverse") {
		return ""
	}
	quote := strings.ToUpper(strings.TrimSpace(instrument.QuoteAsset))
	settle := strings.ToUpper(strings.TrimSpace(instrument.SettleAsset))
	if settle == "" {
		settle = quote
	}
	if quote == "" || settle == "" || quote != settle {
		return ""
	}
	category := bitgetCategory(instrument)
	switch category {
	case "USDT-FUTURES":
		if quote == "USDT" && settle == "USDT" {
			return EstMaxOpenUnitQuoteNotional
		}
	case "USDC-FUTURES":
		if quote == "USDC" && settle == "USDC" {
			return EstMaxOpenUnitQuoteNotional
		}
	}
	return ""
}

func (a *bitgetAdapter) PreviewSetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageSetPreview, error) {
	if instrument.ContractType == "spot" {
		return LeverageSetPreview{}, nil
	}
	values := url.Values{
		"category":   {bitgetCategory(instrument)},
		"symbol":     {instrument.ExchangeSymbol},
		"marginMode": {bitgetPreviewMarginMode},
		"leverage":   {leverage.Truncate(0).String()},
	}
	const endpoint = "/api/v3/account/pre-set-leverage"
	requestPath := endpoint + "?" + values.Encode()
	signPath := endpoint + "?" + bitgetSigningQuery(values)
	raw, err := a.signedPaths(ctx, http.MethodGet, requestPath, signPath, credentials, nil, nil)
	if err != nil {
		return LeverageSetPreview{}, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			EstMaxOpen     string `json:"estMaxOpen"`
			RequiredMargin string `json:"requiredMargin"`
			MarginChange   string `json:"marginChange"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return LeverageSetPreview{}, err
	}
	if payload.Code != "" && payload.Code != "00000" {
		return LeverageSetPreview{}, fmt.Errorf("%w: %s", ErrRejected, payload.Msg)
	}
	marginChange, err := decimal.NewFromString(strings.TrimSpace(payload.Data.MarginChange))
	if err != nil {
		return LeverageSetPreview{}, fmt.Errorf("bitget marginChange is invalid: %w", err)
	}
	preview := LeverageSetPreview{
		EstMaxOpen:     strings.TrimSpace(payload.Data.EstMaxOpen),
		EstMaxOpenUnit: bitgetEstMaxOpenUnit(instrument),
		MarginChange:   marginChange,
	}
	if preview.EstMaxOpen == "" {
		return LeverageSetPreview{}, fmt.Errorf("bitget estMaxOpen is missing")
	}
	if required, parseErr := decimal.NewFromString(strings.TrimSpace(payload.Data.RequiredMargin)); parseErr == nil {
		preview.RequiredMargin = required
	}
	return preview, nil
}

func (a *bitgetAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	if instrument.ContractType == "spot" {
		return LeverageApplyResult{}, nil
	}
	body := compactJSON(map[string]string{
		"category":   bitgetCategory(instrument),
		"symbol":     instrument.ExchangeSymbol,
		"leverage":   leverage.Truncate(0).String(),
		"marginMode": bitgetSetMarginMode,
	})
	raw, err := a.signed(ctx, http.MethodPost, "/api/v3/account/set-leverage", credentials, body, nil)
	if err != nil {
		return LeverageApplyResult{}, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return LeverageApplyResult{}, uncertainDecode(err)
	}
	if payload.Code != "" && payload.Code != "00000" {
		return LeverageApplyResult{}, fmt.Errorf("%w: %s", ErrRejected, payload.Msg)
	}
	return LeverageApplyResult{}, nil
}

func (a *gateAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	if instrument.ContractType == "spot" {
		return LeverageApplyResult{}, nil
	}
	path := "/api/v4/futures/" + gateSettle(instrument) + "/positions/" +
		url.PathEscape(gateFuturesSymbol(instrument)) +
		"/set_leverage?leverage=" + url.QueryEscape(leverage.Truncate(0).String()) +
		"&margin_mode=cross"
	raw, err := a.signed(ctx, http.MethodPost, path, credentials, nil, nil)
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUncertain) ||
		errors.Is(err, context.DeadlineExceeded) {
		return LeverageApplyResult{}, err
	}
	if label, message := gateResponseError(raw); label != "" {
		return LeverageApplyResult{}, fmt.Errorf(
			"%w: Gate code %s: %s", ErrRejected, label, firstNonEmpty(message, label),
		)
	}
	if err != nil {
		return LeverageApplyResult{}, err
	}
	return LeverageApplyResult{}, nil
}

func (a *asterAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	if instrument.ContractType == "spot" {
		return LeverageApplyResult{}, nil
	}
	values := url.Values{
		"symbol":   {instrument.ExchangeSymbol},
		"leverage": {leverage.Truncate(0).String()},
	}
	raw, err := a.signed(ctx, http.MethodPost, asterLeveragePath(credentials), credentials, values)
	if err != nil {
		return LeverageApplyResult{}, err
	}
	return parseAsterLeverageAck(raw, instrument, leverage)
}

func (a *hyperliquidAdapter) SetLeverage(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	client, release, err := a.client(credentials, instrument.ExchangeSymbol)
	if err != nil {
		return LeverageApplyResult{}, err
	}
	defer release()
	lev := int(leverage.Truncate(0).IntPart())
	if lev < 1 {
		return LeverageApplyResult{}, fmt.Errorf("%w: invalid leverage", ErrRejected)
	}
	_, err = client.Trade.SetLeverage(instrument.ExchangeSymbol, lev, hltypes.Cross)
	return LeverageApplyResult{}, err
}

func (a *lighterAdapter) SetLeverage(
	context.Context, Credentials, Instrument, decimal.Decimal,
) (LeverageApplyResult, error) {
	return LeverageApplyResult{}, ErrUnsupported
}

func parseBinanceLeverageAck(
	raw []byte,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	var payload struct {
		Symbol           string     `json:"symbol"`
		Leverage         flexString `json:"leverage"`
		MaxNotionalValue flexString `json:"maxNotionalValue"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return LeverageApplyResult{}, uncertainDecode(err)
	}
	if err := leverageAckMatches("binance", payload.Symbol, payload.Leverage, instrument, leverage); err != nil {
		return LeverageApplyResult{}, err
	}
	maxNotional, err := parsePositiveNotional(payload.MaxNotionalValue.String())
	if err != nil {
		return LeverageApplyResult{}, fmt.Errorf("%w: binance maxNotionalValue: %v", ErrUncertain, err)
	}
	return LeverageApplyResult{MaxNotional: maxNotional, CapacityKnown: true}, nil
}

func parseAsterLeverageAck(
	raw []byte,
	instrument Instrument,
	leverage decimal.Decimal,
) (LeverageApplyResult, error) {
	var payload struct {
		Symbol           string     `json:"symbol"`
		Leverage         flexString `json:"leverage"`
		MaxNotional      flexString `json:"maxNotional"`
		MaxNotionalValue flexString `json:"maxNotionalValue"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return LeverageApplyResult{}, uncertainDecode(err)
	}
	if err := leverageAckMatches("aster", payload.Symbol, payload.Leverage, instrument, leverage); err != nil {
		return LeverageApplyResult{}, err
	}
	maxNotional, err := parseAsterMaxNotional(payload.MaxNotional.String(), payload.MaxNotionalValue.String())
	if err != nil {
		return LeverageApplyResult{}, err
	}
	return LeverageApplyResult{MaxNotional: maxNotional, CapacityKnown: true}, nil
}

func leverageAckMatches(
	venue, symbol string,
	ackLeverage flexString,
	instrument Instrument,
	leverage decimal.Decimal,
) error {
	if !strings.EqualFold(strings.TrimSpace(symbol), instrument.ExchangeSymbol) {
		return fmt.Errorf(
			"%w: %s leverage ack symbol %q does not match %q",
			ErrUncertain, venue, symbol, instrument.ExchangeSymbol,
		)
	}
	got, err := decimal.NewFromString(strings.TrimSpace(ackLeverage.String()))
	if err != nil || !got.Equal(leverage.Truncate(0)) {
		return fmt.Errorf(
			"%w: %s leverage ack %q does not match %s",
			ErrUncertain, venue, ackLeverage.String(), leverage.Truncate(0),
		)
	}
	return nil
}

func parsePositiveNotional(raw string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return decimal.Zero, fmt.Errorf("invalid notional %q", raw)
	}
	return value, nil
}

func parseAsterMaxNotional(maxNotional, maxNotionalValue string) (decimal.Decimal, error) {
	primary, primaryOK, primaryPresent := parseOptionalPositiveDecimal(maxNotional)
	fallback, fallbackOK, fallbackPresent := parseOptionalPositiveDecimal(maxNotionalValue)
	if primaryPresent && fallbackPresent {
		if !primaryOK || !fallbackOK || !primary.Equal(fallback) {
			return decimal.Zero, fmt.Errorf("%w: aster maxNotional fields disagree", ErrUncertain)
		}
		return primary, nil
	}
	if primaryPresent {
		if !primaryOK {
			return decimal.Zero, fmt.Errorf("%w: aster maxNotional is invalid", ErrUncertain)
		}
		return primary, nil
	}
	if fallbackPresent {
		if !fallbackOK {
			return decimal.Zero, fmt.Errorf("%w: aster maxNotionalValue is invalid", ErrUncertain)
		}
		return fallback, nil
	}
	return decimal.Zero, fmt.Errorf("%w: aster maxNotional is missing", ErrUncertain)
}

func parseOptionalPositiveDecimal(raw string) (decimal.Decimal, bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decimal.Zero, false, false
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || !value.IsPositive() {
		return decimal.Zero, false, true
	}
	return value, true, true
}
