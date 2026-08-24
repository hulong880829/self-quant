package marketdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func DefaultParsers() map[string]Parser {
	return map[string]Parser{
		VenueBinance: parseBinance,
		VenueOKX:     parseOKX,
		VenueBybit:   parseBybit,
		VenueBitget:  parseBitget,
		VenueGate:    parseGate,
	}
}

func parseBinance(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Data struct {
			Bids      [][]string      `json:"bids"`
			Asks      [][]string      `json:"asks"`
			ShortBids [][]string      `json:"b"`
			ShortAsks [][]string      `json:"a"`
			Time      json.RawMessage `json:"E"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	bids, asks := message.Data.Bids, message.Data.Asks
	if len(bids) == 0 && len(asks) == 0 {
		bids, asks = message.Data.ShortBids, message.Data.ShortAsks
	}
	if len(bids) == 0 || len(asks) == 0 ||
		len(bids[0]) == 0 || len(asks[0]) == 0 {
		return BBO{}, false, nil
	}
	return makeBBO(
		key, bids[0][0], asks[0][0],
		message.Data.Time, received,
	)
}

func parseOKX(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Argument struct {
			Instrument string `json:"instId"`
			Channel    string `json:"channel"`
		} `json:"arg"`
		Data []struct {
			Bids [][]string      `json:"bids"`
			Asks [][]string      `json:"asks"`
			Time json.RawMessage `json:"ts"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Argument.Channel != "books5" ||
		!strings.EqualFold(message.Argument.Instrument, key.Symbol) ||
		len(message.Data) == 0 {
		return BBO{}, false, nil
	}
	item := message.Data[0]
	if len(item.Bids) == 0 || len(item.Asks) == 0 ||
		len(item.Bids[0]) == 0 || len(item.Asks[0]) == 0 {
		return BBO{}, false, nil
	}
	return makeBBO(key, item.Bids[0][0], item.Asks[0][0], item.Time, received)
}

func parseBybit(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Topic string          `json:"topic"`
		Type  string          `json:"type"`
		Time  json.RawMessage `json:"ts"`
		Data  struct {
			Symbol string     `json:"s"`
			Bids   [][]string `json:"b"`
			Asks   [][]string `json:"a"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Type != "snapshot" || len(message.Data.Bids) == 0 || len(message.Data.Asks) == 0 ||
		len(message.Data.Bids[0]) == 0 || len(message.Data.Asks[0]) == 0 {
		return BBO{}, false, nil
	}
	if message.Data.Symbol != "" && !strings.EqualFold(message.Data.Symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	return makeBBO(key, message.Data.Bids[0][0], message.Data.Asks[0][0], message.Time, received)
}

func parseBitget(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Action   string `json:"action"`
		Argument struct {
			Instrument string `json:"instId"`
			Channel    string `json:"channel"`
		} `json:"arg"`
		Data []struct {
			Bids [][]string      `json:"bids"`
			Asks [][]string      `json:"asks"`
			Time json.RawMessage `json:"ts"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Argument.Instrument != "" &&
		!strings.EqualFold(message.Argument.Instrument, key.Symbol) {
		return BBO{}, false, nil
	}
	if message.Argument.Channel != "books15" || message.Action != "snapshot" {
		return BBO{}, false, nil
	}
	for _, item := range message.Data {
		if len(item.Bids) > 0 && len(item.Asks) > 0 &&
			len(item.Bids[0]) > 0 && len(item.Asks[0]) > 0 {
			return makeBBO(key, item.Bids[0][0], item.Asks[0][0], item.Time, received)
		}
	}
	return BBO{}, false, nil
}

func parseGate(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Channel string          `json:"channel"`
		Event   string          `json:"event"`
		Time    json.RawMessage `json:"time_ms"`
		Result  json.RawMessage `json:"result"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	expectedChannel := "spot.order_book"
	expectedEvent := "update"
	if key.Product == ProductPerpetual {
		expectedChannel = "futures.order_book"
		expectedEvent = "all"
	}
	if message.Channel != expectedChannel || message.Event != expectedEvent ||
		len(bytes.TrimSpace(message.Result)) == 0 {
		return BBO{}, false, nil
	}
	var result map[string]json.RawMessage
	if err := decodeJSON(message.Result, &result); err != nil {
		return BBO{}, false, err
	}
	symbol, err := rawString(result["s"])
	if err != nil {
		return BBO{}, false, err
	}
	if symbol == "" {
		symbol, err = rawString(result["contract"])
		if err != nil {
			return BBO{}, false, err
		}
	}
	if !strings.EqualFold(symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	bid, bidOK, err := firstGateLevelPrice(result["bids"])
	if err != nil {
		return BBO{}, false, err
	}
	ask, askOK, err := firstGateLevelPrice(result["asks"])
	if err != nil {
		return BBO{}, false, err
	}
	if !bidOK || !askOK {
		return BBO{}, false, nil
	}
	return makeBBO(key, bid, ask, message.Time, received)
}

func firstGateLevelPrice(raw json.RawMessage) (string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", false, nil
	}
	var levels []json.RawMessage
	if err := decodeJSON(raw, &levels); err != nil {
		return "", false, err
	}
	if len(levels) == 0 {
		return "", false, nil
	}
	var tuple []json.RawMessage
	if err := decodeJSON(levels[0], &tuple); err == nil {
		if len(tuple) == 0 {
			return "", false, nil
		}
		price, err := rawString(tuple[0])
		return price, price != "", err
	}
	var level map[string]json.RawMessage
	if err := decodeJSON(levels[0], &level); err != nil {
		return "", false, err
	}
	price, err := rawString(level["p"])
	return price, price != "", err
}

func rawString(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", err
	}
	return number.String(), nil
}

func decodeJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func makeBBO(
	key Key,
	bid string,
	ask string,
	rawTimestamp json.RawMessage,
	received time.Time,
) (BBO, bool, error) {
	venueTime, err := millisecondsTime(rawTimestamp)
	if err != nil {
		return BBO{}, false, err
	}
	return BBO{
		Key:              key,
		BidPrice:         bid,
		AskPrice:         ask,
		VenueTimestamp:   venueTime,
		ReceiveTimestamp: received,
	}, true, nil
}

func millisecondsTime(raw json.RawMessage) (time.Time, error) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return time.Time{}, nil
	}
	milliseconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse venue timestamp %q: %w", text, err)
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}
