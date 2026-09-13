package trader

import (
	"strings"
	"time"
)

func hasVenueEventWatermark(at time.Time) bool {
	return !at.IsZero() && at.Year() >= 2000
}

func terminalVenueStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "filled", "canceled", "rejected", "expired":
		return true
	default:
		return false
	}
}

func isAuthoritativeTerminalResult(result VenueResult, stream bool, mergedStatus string) bool {
	if !terminalVenueStatus(mergedStatus) {
		return false
	}
	if stream {
		return true
	}
	return terminalVenueStatus(result.Status)
}
