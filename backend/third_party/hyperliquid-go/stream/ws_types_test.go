package stream

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWSMessage_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		msg      WSMessage
		expected string
	}{
		{
			name: "simple_message",
			msg: WSMessage{
				Channel: "trades",
				Data:    json.RawMessage(`{"coin":"BTC","side":"A","px":"50000","sz":"0.1"}`),
			},
			expected: `{"channel":"trades","data":{"coin":"BTC","side":"A","px":"50000","sz":"0.1"}}`,
		},
		{
			name: "orderbook_message",
			msg: WSMessage{
				Channel: "l2Book",
				Data: json.RawMessage(
					`{"coin":"ETH","levels":[["A",[["3000","1.5"]]],["B",[["2999","2.0"]]]],"time":1234567890}`,
				),
			},
			expected: `{"channel":"l2Book","data":{"coin":"ETH","levels":[["A",[["3000","1.5"]]],["B",[["2999","2.0"]]]],"time":1234567890}}`,
		},
		{
			name: "empty_data",
			msg: WSMessage{
				Channel: "ping",
				Data:    json.RawMessage(`{}`),
			},
			expected: `{"channel":"ping","data":{}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test marshaling
			jsonData, err := json.Marshal(tt.msg)
			require.NoError(t, err, "marshaling should not fail")
			assert.JSONEq(t, tt.expected, string(jsonData), "marshaled JSON should match expected")

			// Test round-trip
			var unmarshaled WSMessage
			err = json.Unmarshal(jsonData, &unmarshaled)
			require.NoError(t, err, "unmarshaling should not fail")
			assert.Equal(t, tt.msg, unmarshaled, "round-trip should preserve data")
		})
	}
}

func TestSubscription_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		sub      SubscriptionFilter
		expected string
	}{
		{
			name: "trades_subscription",
			sub: SubscriptionFilter{
				Type: "trades",
				Coin: "BTC",
			},
			expected: `{"type":"trades","coin":"BTC"}`,
		},
		{
			name: "orderbook_subscription",
			sub: SubscriptionFilter{
				Type: "l2Book",
				Coin: "ETH",
			},
			expected: `{"type":"l2Book","coin":"ETH"}`,
		},
		{
			name: "user_events_subscription",
			sub: SubscriptionFilter{
				Type: "userEvents",
				User: "0x1234567890abcdef1234567890abcdef12345678",
			},
			expected: `{"type":"userEvents","user":"0x1234567890abcdef1234567890abcdef12345678"}`,
		},
		{
			name: "candles_subscription",
			sub: SubscriptionFilter{
				Type:     "candle",
				Coin:     "BTC",
				Interval: "1m",
			},
			expected: `{"type":"candle","coin":"BTC","interval":"1m"}`,
		},
		{
			name: "subscription_with_all_fields",
			sub: SubscriptionFilter{
				Type:     "userFills",
				Coin:     "ETH",
				User:     "0x1234567890abcdef1234567890abcdef12345678",
				Interval: "5m",
			},
			expected: `{"type":"userFills","coin":"ETH","user":"0x1234567890abcdef1234567890abcdef12345678","interval":"5m"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test marshaling
			jsonData, err := json.Marshal(tt.sub)
			require.NoError(t, err, "marshaling should not fail")
			assert.JSONEq(t, tt.expected, string(jsonData), "marshaled JSON should match expected")

			// Test round-trip
			var unmarshaled SubscriptionFilter
			err = json.Unmarshal(jsonData, &unmarshaled)
			require.NoError(t, err, "unmarshaling should not fail")
			assert.Equal(t, tt.sub, unmarshaled, "round-trip should preserve data")
		})
	}
}

func TestWsCommand_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		cmd      WsCommand
		expected string
	}{
		{
			name: "subscribe_command",
			cmd: WsCommand{
				Method: "subscribe",
				Subscription: &SubscriptionFilter{
					Type: "trades",
					Coin: "BTC",
				},
			},
			expected: `{"method":"subscribe","subscription":{"type":"trades","coin":"BTC"}}`,
		},
		{
			name: "unsubscribe_command",
			cmd: WsCommand{
				Method: "unsubscribe",
				Subscription: &SubscriptionFilter{
					Type: "l2Book",
					Coin: "ETH",
				},
			},
			expected: `{"method":"unsubscribe","subscription":{"type":"l2Book","coin":"ETH"}}`,
		},
		{
			name: "ping_command",
			cmd: WsCommand{
				Method:       "ping",
				Subscription: nil,
			},
			expected: `{"method":"ping"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test marshaling
			jsonData, err := json.Marshal(tt.cmd)
			require.NoError(t, err, "marshaling should not fail")
			assert.JSONEq(t, tt.expected, string(jsonData), "marshaled JSON should match expected")

			// Test round-trip
			var unmarshaled WsCommand
			err = json.Unmarshal(jsonData, &unmarshaled)
			require.NoError(t, err, "unmarshaling should not fail")
			assert.Equal(t, tt.cmd, unmarshaled, "round-trip should preserve data")
		})
	}
}

func TestSubscription_Key(t *testing.T) {
	tests := []struct {
		name     string
		sub      SubscriptionFilter
		expected subKey
	}{
		{
			name: "trades_key",
			sub: SubscriptionFilter{
				Type: "trades",
				Coin: "BTC",
			},
			expected: subKey{
				typ:  "trades",
				coin: "BTC",
			},
		},
		{
			name: "user_events_key",
			sub: SubscriptionFilter{
				Type: "userEvents",
				User: "0x1234567890abcdef1234567890abcdef12345678",
			},
			expected: subKey{
				typ:  "userEvents",
				user: "0x1234567890abcdef1234567890abcdef12345678",
			},
		},
		{
			name: "candles_key",
			sub: SubscriptionFilter{
				Type:     "candle",
				Coin:     "ETH",
				Interval: "1h",
			},
			expected: subKey{
				typ:      "candle",
				coin:     "ETH",
				interval: "1h",
			},
		},
		{
			name: "complete_key",
			sub: SubscriptionFilter{
				Type:     "userFills",
				Coin:     "BTC",
				User:     "0x1234567890abcdef1234567890abcdef12345678",
				Interval: "5m",
			},
			expected: subKey{
				typ:      "userFills",
				coin:     "BTC",
				user:     "0x1234567890abcdef1234567890abcdef12345678",
				interval: "5m",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := tt.sub.key()
			assert.Equal(t, tt.expected, key, "subscription key should match expected")
		})
	}
}

func TestWSMessage_UnmarshalJSON_RealWorldExamples(t *testing.T) {
	tests := []struct {
		name     string
		jsonData string
		wantErr  bool
	}{
		{
			name:     "trades_message",
			jsonData: `{"channel":"trades","data":[{"coin":"BTC","side":"A","px":"50000.0","sz":"0.1","time":1234567890}]}`,
			wantErr:  false,
		},
		{
			name:     "l2book_message",
			jsonData: `{"channel":"l2Book","data":{"coin":"ETH","levels":[["A",[["3000.0","1.5"]]],["B",[["2999.0","2.0"]]]],"time":1234567890}}`,
			wantErr:  false,
		},
		{
			name:     "user_events_message",
			jsonData: `{"channel":"userEvents","data":{"fills":[{"coin":"BTC","px":"50000.0","sz":"0.1","side":"A","time":1234567890}]}}`,
			wantErr:  false,
		},
		{
			name:     "malformed_json",
			jsonData: `{"channel":"invalid"`,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg WSMessage
			err := json.Unmarshal([]byte(tt.jsonData), &msg)

			if tt.wantErr {
				assert.Error(t, err, "unmarshaling should fail for malformed JSON")
			} else {
				require.NoError(t, err, "unmarshaling should not fail")
				assert.NotEmpty(t, msg.Channel, "channel should not be empty")
				assert.NotNil(t, msg.Data, "data should not be nil")
			}
		})
	}
}
