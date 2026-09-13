package trader

import "testing"

func TestIsAuthoritativeTerminalResult(t *testing.T) {
	cases := []struct {
		name   string
		result VenueResult
		stream bool
		merged string
		want   bool
	}{
		{name: "wss filled", result: VenueResult{Status: "filled"}, stream: true, merged: "filled", want: true},
		{name: "wss canceled", result: VenueResult{Status: "canceled"}, stream: true, merged: "canceled", want: true},
		{name: "wss rejected", result: VenueResult{Status: "rejected"}, stream: true, merged: "rejected", want: true},
		{name: "wss expired", result: VenueResult{Status: "expired"}, stream: true, merged: "expired", want: true},
		{name: "rest filled", result: VenueResult{Status: "filled"}, merged: "filled", want: true},
		{
			name:   "hl ioc canceled",
			result: VenueResult{Status: "canceled", LocalCommandAck: true},
			merged: "canceled", want: true,
		},
		{
			name:   "hl rejected",
			result: VenueResult{Status: "rejected", LocalCommandAck: true},
			merged: "rejected", want: true,
		},
		{
			name:   "hl placement filled",
			result: VenueResult{Status: "filled", LocalCommandAck: true},
			merged: "filled", want: true,
		},
		{
			name:   "command only cancel ack",
			result: VenueResult{Status: "pending", LocalCommandAck: true},
			merged: "pending", want: false,
		},
		{name: "unknown", result: VenueResult{Status: "unknown"}, merged: "unknown", want: false},
		{name: "timeout pending", result: VenueResult{Status: "pending"}, merged: "pending", want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := isAuthoritativeTerminalResult(test.result, test.stream, test.merged)
			if got != test.want {
				t.Fatalf("got %v want %v", got, test.want)
			}
		})
	}
}
