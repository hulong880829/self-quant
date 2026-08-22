package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

var supportedAssets = []string{"BTC", "ETH", "SOL", "XRP", "DOGE", "HYPE", "BNB"}

type GammaClient struct {
	baseURL string
	http    *http.Client
}

func NewGammaClient(baseURL string, timeout time.Duration) *GammaClient {
	return &GammaClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

type gammaMarket struct {
	ID              string          `json:"id"`
	Question        string          `json:"question"`
	ConditionID     string          `json:"conditionId"`
	Slug            string          `json:"slug"`
	StartDate       string          `json:"startDate"`
	EventStartTime  string          `json:"eventStartTime"`
	EndDate         string          `json:"endDate"`
	Outcomes        json.RawMessage `json:"outcomes"`
	ClobTokenIDs    json.RawMessage `json:"clobTokenIds"`
	OutcomePrices   json.RawMessage `json:"outcomePrices"`
	MinimumTickSize any             `json:"orderPriceMinTickSize"`
	NegRisk         bool            `json:"negRisk"`
	Active          bool            `json:"active"`
	Closed          bool            `json:"closed"`
	UpdatedAt       string          `json:"updatedAt"`
}

type gammaEvent struct {
	Markets       []gammaMarket `json:"markets"`
	EventMetadata struct {
		PriceToBeat float64 `json:"priceToBeat"`
	} `json:"eventMetadata"`
}

func (c *GammaClient) ListCryptoMarkets(ctx context.Context) ([]Market, error) {
	now := time.Now().UTC()
	type interval struct {
		name     string
		duration time.Duration
	}
	intervals := []interval{
		{"5m", 5 * time.Minute},
		{"15m", 15 * time.Minute},
		{"4h", 4 * time.Hour},
	}
	var mu sync.Mutex
	result := make([]Market, 0, len(supportedAssets)*12)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, asset := range supportedAssets {
		asset := strings.ToLower(asset)
		for _, item := range intervals {
			item := item
			start := now.Truncate(item.duration)
			for offset := -1; offset <= 1; offset++ {
				windowStart := start.Add(time.Duration(offset) * item.duration)
				slug := fmt.Sprintf(
					"%s-updown-%s-%d", asset, item.name, windowStart.Unix(),
				)
				group.Go(func() error {
					markets, err := c.fetchEventMarkets(groupCtx, slug)
					if err == nil && len(markets) > 0 {
						mu.Lock()
						result = append(result, markets...)
						mu.Unlock()
					}
					return nil
				})
			}
		}
	}
	for _, asset := range supportedAssets {
		asset := asset
		for offset := -1; offset <= 1; offset++ {
			windowStart := hourlyWindowStart(now, offset)
			slugs := hourlySlugCandidates(asset, windowStart)
			group.Go(func() error {
				markets := c.fetchFirstAvailableEventMarkets(groupCtx, slugs)
				mu.Lock()
				result = append(result, markets...)
				mu.Unlock()
				return nil
			})
		}
	}
	for _, asset := range supportedAssets {
		asset := asset
		group.Go(func() error {
			markets, err := c.searchAssetMarkets(groupCtx, asset)
			if err == nil && len(markets) > 0 {
				mu.Lock()
				result = append(result, markets...)
				mu.Unlock()
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	unique := make(map[string]Market, len(result))
	for _, market := range result {
		if market.WindowEnd.Before(now.Add(-time.Minute)) {
			continue
		}
		unique[market.ID] = market
	}
	result = result[:0]
	for _, market := range unique {
		result = append(result, market)
	}
	return result, nil
}

func (c *GammaClient) searchAssetMarkets(ctx context.Context, asset string) ([]Market, error) {
	query := url.Values{
		"q":              {asset + " up or down"},
		"limit_per_type": {"25"},
		"events_status":  {"active"},
	}
	var response struct {
		Events []gammaEvent `json:"events"`
	}
	if err := c.getJSON(ctx, "/public-search?"+query.Encode(), &response); err != nil {
		return nil, err
	}
	return normalizeEventMarkets(response.Events), nil
}

func (c *GammaClient) fetchEventMarkets(ctx context.Context, slug string) ([]Market, error) {
	var events []gammaEvent
	if err := c.getJSON(
		ctx, "/events?"+url.Values{"slug": {slug}}.Encode(), &events,
	); err != nil {
		return nil, err
	}
	return normalizeEventMarkets(events), nil
}

func (c *GammaClient) fetchFirstAvailableEventMarkets(
	ctx context.Context,
	slugs []string,
) []Market {
	for _, slug := range slugs {
		markets, err := c.fetchEventMarkets(ctx, slug)
		if err == nil && len(markets) > 0 {
			return markets
		}
	}
	return nil
}

func normalizeEventMarkets(events []gammaEvent) []Market {
	result := make([]Market, 0, len(events))
	for _, event := range events {
		openPrice := formatGammaOpenPrice(event.EventMetadata.PriceToBeat)
		for _, source := range event.Markets {
			if market, ok := normalizeGammaMarket(source); ok {
				market.GammaOpenPrice = openPrice
				result = append(result, market)
			}
		}
	}
	return result
}

func formatGammaOpenPrice(value float64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func (c *GammaClient) getJSON(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "selfquant-polymarket/1.0")
	response, err := doWithRetry(ctx, c.http, func() (*http.Request, error) {
		return request.Clone(ctx), nil
	}, 3)
	if err != nil {
		return fmt.Errorf("gamma request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("gamma returned %d", response.StatusCode)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode gamma response: %w", err)
	}
	return nil
}

func (c *GammaClient) listMarketsLegacy(ctx context.Context) ([]Market, error) {
	query := url.Values{
		"active":    {"true"},
		"closed":    {"false"},
		"limit":     {"500"},
		"order":     {"endDate"},
		"ascending": {"false"},
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.baseURL+"/markets?"+query.Encode(), nil,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "selfquant-polymarket/1.0")
	response, err := doWithRetry(ctx, c.http, func() (*http.Request, error) {
		return request.Clone(ctx), nil
	}, 3)
	if err != nil {
		return nil, fmt.Errorf("gamma list markets: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("gamma list markets returned %d", response.StatusCode)
	}
	var raw []gammaMarket
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode gamma markets: %w", err)
	}
	result := make([]Market, 0)
	for _, source := range raw {
		market, ok := normalizeGammaMarket(source)
		if ok {
			result = append(result, market)
		}
	}
	return result, nil
}

func normalizeGammaMarket(source gammaMarket) (Market, bool) {
	haystack := strings.ToUpper(source.Question + " " + source.Slug)
	asset := parseAsset(haystack)
	period := parsePeriodFromSlug(source.Slug)
	if period == "" {
		period = parsePeriod(haystack)
	}
	if asset == "" || period == "" {
		return Market{}, false
	}
	var outcomes, tokenIDs []string
	if err := decodeStringArray(source.Outcomes, &outcomes); err != nil {
		return Market{}, false
	}
	if err := decodeStringArray(source.ClobTokenIDs, &tokenIDs); err != nil {
		return Market{}, false
	}
	if len(outcomes) != len(tokenIDs) || len(outcomes) < 2 {
		return Market{}, false
	}
	upID, downID := "", ""
	for index, outcome := range outcomes {
		switch strings.ToLower(strings.TrimSpace(outcome)) {
		case "up", "yes":
			upID = tokenIDs[index]
		case "down", "no":
			downID = tokenIDs[index]
		}
	}
	if upID == "" || downID == "" {
		return Market{}, false
	}
	start, startErr := time.Parse(time.RFC3339, source.StartDate)
	end, endErr := time.Parse(time.RFC3339, source.EndDate)
	if source.EventStartTime != "" {
		if eventStart, err := time.Parse(time.RFC3339, source.EventStartTime); err == nil {
			start, startErr = eventStart, nil
		}
	}
	if slugStart, slugEnd, ok := intervalWindowFromSlug(source.Slug, period); ok {
		start, end = slugStart, slugEnd
		startErr, endErr = nil, nil
	}
	if startErr != nil || endErr != nil {
		return Market{}, false
	}
	updated := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339, source.UpdatedAt); err == nil {
		updated = parsed
	}
	tickSize := "0.01"
	switch value := source.MinimumTickSize.(type) {
	case string:
		tickSize = value
	case float64:
		tickSize = strconv.FormatFloat(value, 'f', -1, 64)
	}
	return Market{
		ID: source.ID, ConditionID: source.ConditionID, Slug: source.Slug,
		Asset: asset, Period: period, Title: source.Question,
		WindowStart: start, WindowEnd: end, UpTokenID: upID, DownTokenID: downID,
		TickSize: tickSize, NegativeRisk: source.NegRisk,
		Active: source.Active && !source.Closed, SourceUpdatedAt: updated,
	}, true
}

func intervalWindowFromSlug(slug, period string) (time.Time, time.Time, bool) {
	parts := strings.Split(slug, "-")
	if len(parts) == 0 {
		return time.Time{}, time.Time{}, false
	}
	seconds, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	durations := map[string]time.Duration{
		"5m": 5 * time.Minute, "15m": 15 * time.Minute,
		"1h": time.Hour, "4h": 4 * time.Hour,
	}
	duration, ok := durations[period]
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	start := time.Unix(seconds, 0).UTC()
	return start, start.Add(duration), true
}

func decodeStringArray(raw json.RawMessage, target *[]string) error {
	if len(raw) == 0 {
		return fmt.Errorf("empty array")
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return err
		}
		return json.Unmarshal([]byte(encoded), target)
	}
	return json.Unmarshal(raw, target)
}

func parsePeriodFromSlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	patterns := []struct {
		needle string
		value  string
	}{
		{"updown-15m-", "15m"},
		{"-15m-", "15m"},
		{"updown-5m-", "5m"},
		{"-5m-", "5m"},
		{"updown-1h-", "1h"},
		{"-1h-", "1h"},
		{"updown-4h-", "4h"},
		{"-4h-", "4h"},
	}
	for _, pattern := range patterns {
		if strings.Contains(slug, pattern.needle) {
			return pattern.value
		}
	}
	if isHumanHourlySlug(slug) {
		return "1h"
	}
	return ""
}

func parsePeriod(value string) string {
	candidates := []struct {
		needles []string
		value   string
	}{
		{[]string{"15 MIN", "15-MIN", "15M", "-15M-"}, "15m"},
		{[]string{"5 MIN", "5-MIN", "5M", "-5M-"}, "5m"},
		{[]string{"1 HOUR", "HOURLY", "1H", "每小时"}, "1h"},
		{[]string{"4 HOUR", "4H", "-4H-"}, "4h"},
		{[]string{"DAILY", "1 DAY", "1D"}, "1d"},
		{[]string{"WEEKLY", "1 WEEK", "1W"}, "1w"},
	}
	for _, candidate := range candidates {
		for _, needle := range candidate.needles {
			if strings.Contains(value, needle) {
				return candidate.value
			}
		}
	}
	return ""
}
