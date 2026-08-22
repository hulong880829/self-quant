package orderstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type Parser func(Key, []byte, time.Time) ([]Update, bool, error)

func DefaultParsers() map[string]Parser {
	return map[string]Parser{
		VenueBinance: parseBinance,
		VenueOKX:     parseOKX,
		VenueBybit:   parseBybit,
		VenueBitget:  parseBitget,
		VenueGate:    parseGate,
	}
}

func privateStreamControlError(payload []byte) error {
	var message map[string]json.RawMessage
	if json.Unmarshal(payload, &message) != nil {
		return nil
	}
	event := strings.ToLower(firstNonEmpty(
		rawString(message["event"]), rawString(message["e"]),
	))
	if event == "listenkeyexpired" || event == "error" {
		return fmt.Errorf("%s: %s", event, rawString(message["msg"]))
	}
	code := rawString(message["code"])
	if code != "" && code != "0" &&
		(event == "login" || event == "subscribe" || event == "error") {
		return fmt.Errorf("%s failed with code %s: %s", event, code, rawString(message["msg"]))
	}
	success := rawString(message["success"])
	if success == "false" {
		return fmt.Errorf("%s failed", firstNonEmpty(event, "operation"))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseBinance(key Key, payload []byte, received time.Time) ([]Update, bool, error) {
	var root map[string]json.RawMessage
	if err := decode(payload, &root); err != nil {
		return nil, false, err
	}
	event := rawString(root["e"])
	order := root
	if event == "ORDER_TRADE_UPDATE" {
		if err := json.Unmarshal(root["o"], &order); err != nil {
			return nil, false, fmt.Errorf("decode Binance order: %w", err)
		}
	}
	if event != "executionReport" && event != "ORDER_TRADE_UPDATE" {
		return nil, false, nil
	}
	update := baseUpdate(key, received)
	update.ClientOrderID = rawString(order["c"])
	update.VenueOrderID = rawString(order["i"])
	update.Status = normalizeStatus(rawString(order["X"]))
	update.CumulativeFilled = rawString(order["z"])
	update.AveragePrice = rawString(order["ap"])
	update.LastFilled = rawString(order["l"])
	update.LastPrice = rawString(order["L"])
	if update.AveragePrice == "" {
		update.AveragePrice = quotient(rawString(order["Z"]), update.CumulativeFilled)
	}
	update.TradeID = rawString(order["t"])
	update.EventTime = rawTime(first(order["T"], root["E"]), received)
	update.Sequence = rawInt64(first(root["u"], root["E"]))
	update.Type = UpdateOrder
	if update.TradeID != "" && update.TradeID != "-1" {
		update.Type = UpdateTrade
	}
	return []Update{update}, true, nil
}

func parseOKX(key Key, payload []byte, received time.Time) ([]Update, bool, error) {
	var message struct {
		Argument struct {
			Channel string `json:"channel"`
		} `json:"arg"`
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := decode(payload, &message); err != nil {
		return nil, false, err
	}
	if message.Argument.Channel != "orders" {
		return nil, false, nil
	}
	updates := make([]Update, 0, len(message.Data))
	for _, item := range message.Data {
		update := baseUpdate(key, received)
		update.Type = UpdateOrder
		update.ClientOrderID = rawString(item["clOrdId"])
		update.VenueOrderID = rawString(item["ordId"])
		update.Status = normalizeStatus(rawString(item["state"]))
		update.CumulativeFilled = rawString(item["accFillSz"])
		update.AveragePrice = rawString(item["avgPx"])
		update.LastFilled = rawString(item["fillSz"])
		update.LastPrice = rawString(item["fillPx"])
		update.TradeID = rawString(item["tradeId"])
		update.EventTime = rawTime(first(item["uTime"], item["fillTime"]), received)
		update.Sequence = rawInt64(item["seqId"])
		if update.TradeID != "" {
			update.Type = UpdateTrade
		}
		updates = append(updates, update)
	}
	return updates, len(updates) > 0, nil
}

func parseBybit(key Key, payload []byte, received time.Time) ([]Update, bool, error) {
	var message struct {
		Topic        string                       `json:"topic"`
		CreationTime json.RawMessage              `json:"creationTime"`
		ID           json.RawMessage              `json:"id"`
		Data         []map[string]json.RawMessage `json:"data"`
	}
	if err := decode(payload, &message); err != nil {
		return nil, false, err
	}
	if message.Topic != "order" && message.Topic != "execution" &&
		message.Topic != "execution.fast" {
		return nil, false, nil
	}
	updates := make([]Update, 0, len(message.Data))
	for _, item := range message.Data {
		update := baseUpdate(key, received)
		update.Type = UpdateOrder
		update.ClientOrderID = rawString(item["orderLinkId"])
		update.VenueOrderID = rawString(item["orderId"])
		update.Status = normalizeStatus(rawString(item["orderStatus"]))
		update.CumulativeFilled = rawString(item["cumExecQty"])
		update.AveragePrice = rawString(item["avgPrice"])
		update.LastFilled = rawString(item["execQty"])
		update.LastPrice = rawString(item["execPrice"])
		update.TradeID = rawString(item["execId"])
		update.EventTime = rawTime(first(item["execTime"], message.CreationTime), received)
		update.Sequence = rawInt64(first(item["seq"], message.ID))
		if message.Topic == "execution" || message.Topic == "execution.fast" {
			update.Type = UpdateTrade
		}
		updates = append(updates, update)
	}
	return updates, len(updates) > 0, nil
}

func parseBitget(key Key, payload []byte, received time.Time) ([]Update, bool, error) {
	var message struct {
		Argument struct {
			Topic string `json:"topic"`
		} `json:"arg"`
		Data json.RawMessage `json:"data"`
	}
	if err := decode(payload, &message); err != nil {
		return nil, false, err
	}
	if message.Argument.Topic != "order" && message.Argument.Topic != "fill" &&
		message.Argument.Topic != "fast-fill" {
		return nil, false, nil
	}
	var items []map[string]json.RawMessage
	if len(bytes.TrimSpace(message.Data)) == 0 {
		return nil, false, nil
	}
	if bytes.TrimSpace(message.Data)[0] == '[' {
		if err := json.Unmarshal(message.Data, &items); err != nil {
			return nil, false, fmt.Errorf("decode Bitget results: %w", err)
		}
	} else {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(message.Data, &item); err != nil {
			return nil, false, fmt.Errorf("decode Bitget result: %w", err)
		}
		items = append(items, item)
	}
	updates := make([]Update, 0, len(items))
	for _, item := range items {
		update := baseUpdate(key, received)
		update.Type = UpdateOrder
		update.ClientOrderID = rawString(item["clientOid"])
		update.VenueOrderID = rawString(item["orderId"])
		update.Status = normalizeStatus(rawString(item["orderStatus"]))
		update.CumulativeFilled = rawString(item["cumExecQty"])
		update.AveragePrice = rawString(item["avgPrice"])
		update.LastFilled = rawString(item["execQty"])
		update.LastPrice = rawString(item["execPrice"])
		update.TradeID = rawString(item["execId"])
		update.EventTime = rawTime(first(item["updatedTime"], item["execTime"]), received)
		update.Sequence = rawInt64(first(item["updatedTime"], item["execTime"]))
		if message.Argument.Topic == "fill" || message.Argument.Topic == "fast-fill" {
			update.Type = UpdateTrade
		}
		updates = append(updates, update)
	}
	return updates, len(updates) > 0, nil
}

func parseGate(key Key, payload []byte, received time.Time) ([]Update, bool, error) {
	var message struct {
		Channel string          `json:"channel"`
		TimeMS  json.RawMessage `json:"time_ms"`
		Result  json.RawMessage `json:"result"`
	}
	if err := decode(payload, &message); err != nil {
		return nil, false, err
	}
	if !strings.HasSuffix(message.Channel, ".orders") && !strings.HasSuffix(message.Channel, ".usertrades") {
		return nil, false, nil
	}
	var items []map[string]json.RawMessage
	if len(bytes.TrimSpace(message.Result)) == 0 {
		return nil, false, nil
	}
	if message.Result[0] == '[' {
		if err := json.Unmarshal(message.Result, &items); err != nil {
			return nil, false, fmt.Errorf("decode Gate results: %w", err)
		}
	} else {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(message.Result, &item); err != nil {
			return nil, false, fmt.Errorf("decode Gate result: %w", err)
		}
		items = append(items, item)
	}
	updates := make([]Update, 0, len(items))
	for _, item := range items {
		update := baseUpdate(key, received)
		update.Type = UpdateOrder
		update.ClientOrderID = strings.TrimPrefix(
			rawString(first(item["text"], item["client_order_id"])), "t-",
		)
		update.VenueOrderID = rawString(first(item["order_id"], item["id"]))
		update.Status = normalizeStatus(rawString(first(item["finish_as"], item["status"])))
		update.CumulativeFilled = rawString(item["filled_total"])
		update.AveragePrice = rawString(first(item["avg_deal_price"], item["fill_price"], item["price"]))
		update.LastFilled = rawString(first(item["amount"], item["size"], item["filled_amount"]))
		update.LastPrice = rawString(item["price"])
		update.EventTime = rawTime(first(item["create_time_ms"], item["time_ms"], message.TimeMS), received)
		update.Sequence = rawInt64(first(item["sequence"], item["id"]))
		if key.Product == ProductPerpetual && update.CumulativeFilled == "" {
			size := parseDecimal(rawString(item["size"]))
			left := parseDecimal(rawString(item["left"]))
			total := size.Abs()
			filled := size.Sub(left).Abs()
			update.CumulativeFilled = filled.String()
			if update.Status == StatusUnknown {
				switch {
				case total.IsPositive() && filled.GreaterThanOrEqual(total):
					update.Status = StatusFilled
				case strings.EqualFold(rawString(item["status"]), "finished"):
					update.Status = StatusCanceled
				case filled.IsPositive():
					update.Status = StatusPartiallyFilled
				default:
					update.Status = StatusNew
				}
			}
		}
		if strings.HasSuffix(message.Channel, ".usertrades") {
			update.Type = UpdateTrade
			update.TradeID = rawString(first(item["trade_id"], item["id"]))
		}
		updates = append(updates, update)
	}
	return updates, len(updates) > 0, nil
}

func parseDecimal(value string) decimal.Decimal {
	result, _ := decimal.NewFromString(strings.TrimSpace(value))
	return result
}

func baseUpdate(key Key, received time.Time) Update {
	return Update{
		Account: key.Account, Venue: key.Venue, Product: key.Product,
		Status: StatusUnknown, EventTime: received,
	}
}

func normalizeStatus(value string) Status {
	switch strings.ToLower(strings.ReplaceAll(value, "-", "_")) {
	case "new", "live", "open", "created":
		return StatusNew
	case "partially_filled", "partial_fill", "partiallyfilled":
		return StatusPartiallyFilled
	case "filled", "full_fill", "closed", "fullyfilled":
		return StatusFilled
	case "canceled", "cancelled", "cancel":
		return StatusCanceled
	case "rejected", "reject":
		return StatusRejected
	case "expired", "deactivated":
		return StatusExpired
	default:
		return StatusUnknown
	}
}

func decode(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return strings.Trim(string(raw), `"`)
}

func rawInt64(raw json.RawMessage) int64 {
	value, _ := strconv.ParseInt(rawString(raw), 10, 64)
	return value
}

func rawTime(raw json.RawMessage, fallback time.Time) time.Time {
	value := rawInt64(raw)
	if value == 0 {
		return fallback
	}
	if value < 10_000_000_000 {
		return time.Unix(value, 0).UTC()
	}
	return time.UnixMilli(value).UTC()
}

func first(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		text := bytes.TrimSpace(value)
		if len(text) > 0 && !bytes.Equal(text, []byte("null")) && !bytes.Equal(text, []byte(`""`)) {
			return value
		}
	}
	return nil
}

func quotient(numerator, denominator string) string {
	n, nErr := strconv.ParseFloat(numerator, 64)
	d, dErr := strconv.ParseFloat(denominator, 64)
	if nErr != nil || dErr != nil || d == 0 {
		return ""
	}
	return strconv.FormatFloat(n/d, 'f', -1, 64)
}
