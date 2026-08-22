package polymarket

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var easternLocation = mustLoadLocation("America/New_York")

var hourlyAssetSlugs = map[string]string{
	"BTC":  "bitcoin",
	"ETH":  "ethereum",
	"SOL":  "solana",
	"XRP":  "xrp",
	"DOGE": "dogecoin",
	"HYPE": "hype",
	"BNB":  "bnb",
}

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("load time zone %q: %v", name, err))
	}
	return location
}

func hourlyWindowStart(now time.Time, offset int) time.Time {
	eastern := now.In(easternLocation)
	start := time.Date(
		eastern.Year(), eastern.Month(), eastern.Day(), eastern.Hour(),
		0, 0, 0, easternLocation,
	)
	return start.Add(time.Duration(offset) * time.Hour)
}

func hourlySlugCandidates(asset string, windowStart time.Time) []string {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	assetSlug, ok := hourlyAssetSlugs[asset]
	if !ok {
		return nil
	}
	eastern := windowStart.In(easternLocation)
	month := strings.ToLower(eastern.Month().String())
	hour := eastern.Hour()
	meridiem := "am"
	if hour >= 12 {
		meridiem = "pm"
	}
	hour %= 12
	if hour == 0 {
		hour = 12
	}
	prefix := fmt.Sprintf(
		"%s-up-or-down-%s-%d", assetSlug, month, eastern.Day(),
	)
	return []string{
		fmt.Sprintf("%s-%d-%d%s-et", prefix, eastern.Year(), hour, meridiem),
		fmt.Sprintf("%s-%d%s-et", prefix, hour, meridiem),
		fmt.Sprintf(
			"%s-updown-1h-%d",
			strings.ToLower(asset),
			eastern.UTC().Unix(),
		),
	}
}

func parseAsset(value string) string {
	upper := strings.ToUpper(value)
	for _, candidate := range supportedAssets {
		if strings.Contains(upper, candidate) {
			return candidate
		}
	}
	lower := strings.ToLower(value)
	for asset, slug := range hourlyAssetSlugs {
		if strings.Contains(lower, slug) {
			return asset
		}
	}
	return ""
}

func isHumanHourlySlug(slug string) bool {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !strings.Contains(slug, "-up-or-down-") || !strings.HasSuffix(slug, "-et") {
		return false
	}
	parts := strings.Split(slug, "-")
	if len(parts) < 2 {
		return false
	}
	hourToken := parts[len(parts)-2]
	hourToken = strings.TrimSuffix(strings.TrimSuffix(hourToken, "am"), "pm")
	hour, err := strconv.Atoi(hourToken)
	return err == nil && hour >= 1 && hour <= 12
}
