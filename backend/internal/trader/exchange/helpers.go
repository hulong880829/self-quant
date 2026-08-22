package exchange

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func unmarshalJSON(raw []byte, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	return nil
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
