package marketdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type subscriptionControl struct {
	correlation string
	operation   string
	symbol      string
	err         error
	deliver     bool
}

func parseSubscriptionControl(
	venue string,
	payload []byte,
) (subscriptionControl, bool, error) {
	switch venue {
	case VenueBinance:
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"msg"`
			} `json:"error"`
		}
		if err := decodeJSON(payload, &message); err != nil {
			return subscriptionControl{}, false, err
		}
		if len(bytes.TrimSpace(message.ID)) == 0 ||
			(len(bytes.TrimSpace(message.Result)) == 0 && message.Error == nil) {
			return subscriptionControl{}, false, nil
		}
		control := subscriptionControl{
			correlation: requestCorrelation(rawCorrelationID(message.ID)),
		}
		if message.Error != nil {
			control.err = fmt.Errorf(
				"binance subscription rejected (%d): %s",
				message.Error.Code, message.Error.Message,
			)
		}
		return control, true, nil
	case VenueAster:
		var message struct {
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"msg"`
		}
		if err := decodeJSON(payload, &message); err != nil {
			return subscriptionControl{}, false, err
		}
		hasResult := len(bytes.TrimSpace(message.Result)) > 0
		hasCode := len(bytes.TrimSpace(message.Code)) > 0
		if len(bytes.TrimSpace(message.ID)) == 0 || (!hasResult && !hasCode) {
			return subscriptionControl{}, false, nil
		}
		control := subscriptionControl{
			correlation: requestCorrelation(rawCorrelationID(message.ID)),
		}
		if hasCode {
			detail := strings.TrimSpace(message.Message)
			if detail == "" {
				detail = "request rejected"
			}
			control.err = fmt.Errorf(
				"aster subscription rejected (%s): %s",
				rawCorrelationID(message.Code), detail,
			)
		}
		return control, true, nil

	case VenueBybit:
		var message struct {
			RequestID     json.RawMessage `json:"req_id"`
			Operation     string          `json:"op"`
			Success       *bool           `json:"success"`
			ReturnCode    *int            `json:"retCode"`
			ReturnMessage string          `json:"ret_msg"`
		}
		if err := decodeJSON(payload, &message); err != nil {
			return subscriptionControl{}, false, err
		}
		if len(bytes.TrimSpace(message.RequestID)) == 0 ||
			(message.Operation != "subscribe" && message.Operation != "unsubscribe") ||
			(message.Success == nil && message.ReturnCode == nil) {
			return subscriptionControl{}, false, nil
		}
		control := subscriptionControl{
			correlation: requestCorrelation(rawCorrelationID(message.RequestID)),
			operation:   message.Operation,
		}
		if (message.Success != nil && !*message.Success) ||
			(message.ReturnCode != nil && *message.ReturnCode != 0) {
			detail := strings.TrimSpace(message.ReturnMessage)
			if detail == "" {
				detail = "request rejected"
			}
			control.err = fmt.Errorf("bybit subscription rejected: %s", detail)
		}
		return control, true, nil

	case VenueOKX, VenueBitget:
		var message struct {
			Event string          `json:"event"`
			Code  json.RawMessage `json:"code"`
			Msg   string          `json:"msg"`
			Arg   struct {
				Symbol string `json:"instId"`
			} `json:"arg"`
		}
		if err := decodeJSON(payload, &message); err != nil {
			return subscriptionControl{}, false, err
		}
		if message.Event != "subscribe" && message.Event != "unsubscribe" &&
			message.Event != "error" {
			return subscriptionControl{}, false, nil
		}
		if strings.TrimSpace(message.Arg.Symbol) == "" {
			return subscriptionControl{}, false, nil
		}
		control := subscriptionControl{
			operation: message.Event,
			symbol:    canonicalSymbol(message.Arg.Symbol),
		}
		code := rawCorrelationID(message.Code)
		if message.Event != "error" {
			control.correlation = symbolCorrelation(message.Event, message.Arg.Symbol)
		}
		if message.Event == "error" || (code != "" && code != "0") {
			detail := strings.TrimSpace(message.Msg)
			if detail == "" {
				detail = "request rejected"
			}
			control.err = fmt.Errorf(
				"%s subscription rejected (%s): %s", venue, code, detail,
			)
		}
		return control, true, nil

	case VenueGate:
		var message struct {
			ID    json.RawMessage `json:"id"`
			Event string          `json:"event"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := decodeJSON(payload, &message); err != nil {
			return subscriptionControl{}, false, err
		}
		if len(bytes.TrimSpace(message.ID)) == 0 ||
			(message.Event != "subscribe" && message.Event != "unsubscribe") {
			return subscriptionControl{}, false, nil
		}
		control := subscriptionControl{
			correlation: requestCorrelation(rawCorrelationID(message.ID)),
			operation:   message.Event,
		}
		if message.Error != nil {
			control.err = fmt.Errorf(
				"gate subscription rejected (%d): %s",
				message.Error.Code, message.Error.Message,
			)
		}
		return control, true, nil
	case VenueHyperliquid:
		var envelope struct {
			Channel string          `json:"channel"`
			Data    json.RawMessage `json:"data"`
		}
		if err := decodeJSON(payload, &envelope); err != nil {
			return subscriptionControl{}, false, err
		}
		if envelope.Channel == "pong" {
			return subscriptionControl{}, true, nil
		}
		if envelope.Channel == "error" {
			detail, _ := rawString(envelope.Data)
			if strings.TrimSpace(detail) == "" {
				detail = "request rejected"
			}
			return subscriptionControl{
				err: fmt.Errorf("hyperliquid subscription rejected: %s", detail),
			}, true, nil
		}
		if envelope.Channel != "subscriptionResponse" {
			return subscriptionControl{}, false, nil
		}
		var data struct {
			Method       string `json:"method"`
			Subscription struct {
				Type string `json:"type"`
				Coin string `json:"coin"`
			} `json:"subscription"`
		}
		if err := decodeJSON(envelope.Data, &data); err != nil {
			return subscriptionControl{}, false, err
		}
		if (data.Method != "subscribe" && data.Method != "unsubscribe") ||
			data.Subscription.Type != "bbo" ||
			strings.TrimSpace(data.Subscription.Coin) == "" {
			return subscriptionControl{}, false, nil
		}
		return subscriptionControl{
			correlation: symbolCorrelation(
				data.Method, data.Subscription.Coin,
			),
			operation: data.Method,
			symbol:    canonicalSymbol(data.Subscription.Coin),
		}, true, nil
	case VenueLighter:
		var message struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
			Message string `json:"message"`
		}
		if err := decodeJSON(payload, &message); err != nil {
			return subscriptionControl{}, false, err
		}
		switch message.Type {
		case "connected", "pong":
			return subscriptionControl{}, true, nil
		case "subscribed/order_book":
			id, ok := lighterChannelID(message.Channel)
			if !ok {
				return subscriptionControl{}, false, nil
			}
			return subscriptionControl{
				correlation: lighterCorrelation("subscribe", id),
				operation:   "subscribe",
				deliver:     true,
			}, true, nil
		case "unsubscribed":
			id, ok := lighterChannelID(message.Channel)
			if !ok {
				return subscriptionControl{}, false, nil
			}
			return subscriptionControl{
				correlation: lighterCorrelation("unsubscribe", id),
				operation:   "unsubscribe",
			}, true, nil
		case "error":
			id, ok := lighterChannelID(message.Channel)
			if !ok {
				detail := strings.TrimSpace(message.Message)
				if detail == "" {
					detail = "request rejected"
				}
				return subscriptionControl{
					err: fmt.Errorf("lighter subscription rejected: %s", detail),
				}, true, nil
			}
			detail := strings.TrimSpace(message.Message)
			if detail == "" {
				detail = "request rejected"
			}
			return subscriptionControl{
				correlation: lighterCorrelation("subscribe", id),
				operation:   "subscribe",
				err:         fmt.Errorf("lighter subscription rejected: %s", detail),
			}, true, nil
		default:
			return subscriptionControl{}, false, nil
		}
	default:
		return subscriptionControl{}, false, fmt.Errorf(
			"%w: websocket control venue %q", ErrUnsupportedKey, venue,
		)
	}
}

func lighterChannelID(channel string) (string, bool) {
	normalized := strings.TrimSpace(channel)
	for _, prefix := range []string{"order_book:", "order_book/"} {
		if strings.HasPrefix(normalized, prefix) && len(normalized) > len(prefix) {
			return normalized[len(prefix):], true
		}
	}
	return "", false
}

func lighterCorrelation(operation, marketID string) string {
	return operation + ":lighter:" + marketID
}

func rawCorrelationID(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

func requestCorrelation(id string) string {
	return "request:" + id
}

func symbolCorrelation(operation, symbol string) string {
	return operation + ":" + canonicalSymbol(symbol)
}
