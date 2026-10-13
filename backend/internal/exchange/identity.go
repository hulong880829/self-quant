package exchange

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	constituentStatusOK       = "ok"
	constituentStatusEmpty    = "empty"
	constituentStatusError    = "error"
	constituentStatusConflict = "conflict"
)

// ConstituentRow is one official index member or cross-venue contract edge.
type ConstituentRow struct {
	Exchange string `json:"exchange"`
	Symbol   string `json:"symbol"`
}

// ConstituentEvidence is the result of one constituents request.
// Status error keeps the previous listing. Empty and conflict do not.
type ConstituentEvidence struct {
	Status string           `json:"status"`
	Rows   []ConstituentRow `json:"rows"`
}

type listingID struct {
	symbol, region, currency string
}

func (id listingID) empty() bool {
	return id.symbol == "" || id.region == "" || id.currency == ""
}

func (id listingID) same(other listingID) bool {
	return id.symbol == other.symbol && id.region == other.region && id.currency == other.currency
}

type claimBook struct {
	values map[string]map[string]listingID
	bad    map[string]bool
}

func (b *claimBook) add(key, source string, id listingID) {
	key = strings.ToUpper(strings.TrimSpace(key))
	source = strings.ToLower(strings.TrimSpace(source))
	if key == "" || source == "" || id.empty() || b.bad[key] {
		return
	}
	if b.values == nil {
		b.values = map[string]map[string]listingID{}
		b.bad = map[string]bool{}
	}
	if b.values[key] == nil {
		b.values[key] = map[string]listingID{}
	}
	if existing, ok := b.values[key][source]; ok && !existing.same(id) {
		b.bad[key] = true
		delete(b.values, key)
		return
	}
	b.values[key][source] = id
}

func (b claimBook) agree(keys []string) (listingID, bool) {
	sources := map[string]listingID{}
	var chosen listingID
	for _, key := range keys {
		key = strings.ToUpper(strings.TrimSpace(key))
		if b.bad[key] {
			return listingID{}, false
		}
		for source, id := range b.values[key] {
			if previous, ok := sources[source]; ok && !previous.same(id) {
				return listingID{}, false
			}
			if !chosen.empty() && !chosen.same(id) {
				return listingID{}, false
			}
			chosen = id
			sources[source] = id
		}
	}
	if len(sources) < 2 || chosen.empty() {
		return listingID{}, false
	}
	return chosen, true
}

// ApplyTradfiIdentities fills listing identity after every venue catalog is staged.
// targets are the instruments that will be saved. sources include those catalogs
// plus the previous catalog of a venue whose refresh failed. previous is the last
// saved identity used only when a constituents request fails.
func ApplyTradfiIdentities(targets, sources, previous []Instrument) []Instrument {
	prior := map[string]Instrument{}
	for _, item := range previous {
		prior[instrumentKey(item)] = item
	}
	claims, edges := collectTradfiEvidence(sources)
	result := make([]Instrument, len(targets))
	for index, item := range targets {
		result[index] = resolveTradfiIdentity(item, claims, edges, prior[instrumentKey(item)])
	}
	return result
}

func instrumentKey(item Instrument) string {
	return item.Exchange + "|" + item.ExchangeSymbol
}

func collectTradfiEvidence(items []Instrument) (claimBook, map[string]listingID) {
	var claims claimBook
	edges := map[string]listingID{}
	for _, item := range items {
		if id, ok := directListing(item.Underlying); ok {
			addListingClaims(&claims, item.Exchange, item.BaseAsset, id)
		}
		evidence, ok := readConstituents(item.Metadata)
		if !ok || evidence.Status != constituentStatusOK {
			continue
		}
		natives := nativeListings(evidence.Rows)
		if len(natives) == 1 {
			addListingClaims(&claims, item.Exchange, item.BaseAsset, natives[0])
		}
		if len(natives) != 1 {
			continue
		}
		for _, row := range evidence.Rows {
			venue, contract, ok := constituentEdge(row.Exchange, row.Symbol)
			if !ok {
				continue
			}
			edges[venue+"|"+contract] = natives[0]
		}
	}
	return claims, edges
}

func addListingClaims(book *claimBook, source, base string, id listingID) {
	book.add(base, source, id)
	if !strings.EqualFold(id.symbol, base) {
		book.add(id.symbol, source, id)
	}
}

func resolveTradfiIdentity(item Instrument, claims claimBook, edges map[string]listingID, previous Instrument) Instrument {
	if !identityEligible(item.Underlying) {
		return item
	}
	evidence, hasEvidence := readConstituents(item.Metadata)
	if hasEvidence && evidence.Status == constituentStatusError && previous.Underlying.Routable() {
		item.SetUnderlying(previous.Underlying)
		return rejectAnnotationConflict(item)
	}
	if hasEvidence && evidence.Status == constituentStatusConflict {
		item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.conflict"))
		return item
	}
	var own listingID
	var ownOK bool
	if hasEvidence && evidence.Status == constituentStatusOK {
		natives := nativeListings(evidence.Rows)
		if len(natives) > 1 {
			item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.conflict"))
			return item
		}
		if len(natives) == 1 {
			own, ownOK = natives[0], true
		}
	}
	edge, edgeOK := explicitEdge(item, edges)
	if edgeOK && !allowsExplicitEdge(item) {
		edgeOK = false
	}
	if ownOK && edgeOK && !own.same(edge) {
		item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.conflict"))
		return item
	}
	chosen, ok := listingID{}, false
	switch {
	case ownOK:
		chosen, ok = own, true
	case edgeOK:
		chosen, ok = edge, true
	default:
		if agreed, agreedOK := claims.agree(candidateKeys(item.BaseAsset)); agreedOK {
			chosen, ok = agreed, true
		}
	}
	if !ok {
		return item
	}
	if current, currentOK := directListing(item.Underlying); currentOK && !current.same(chosen) {
		item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.conflict"))
		return item
	}
	if previous.Underlying.Routable() {
		if prior, priorOK := directListing(previous.Underlying); priorOK && !prior.same(chosen) {
			item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.conflict"))
			return item
		}
	}
	item.SetUnderlying(applyListing(item, chosen, evidenceSource(item, ownOK, edgeOK)))
	return rejectAnnotationConflict(item)
}

func applyListing(item Instrument, id listingID, source string) UnderlyingReference {
	quote := item.QuoteAsset
	if quote == "" {
		quote = item.Underlying.PerpQuoteCurrency
	}
	ref := RegionalStock(id.symbol, id.region, id.currency, quote, item.Underlying.FullName, source)
	ref.PerpPriceCurrency = item.Underlying.PerpPriceCurrency
	ref.DollarConverted = item.Underlying.DollarConverted
	if ref.PerpPriceCurrency == "" && strings.EqualFold(id.region, "US") {
		ref.PerpPriceCurrency = "USD"
	}
	if ref.DollarConverted && ref.PerpPriceCurrency == "" {
		ref.PerpPriceCurrency = "USD"
	}
	return ref.Normalized()
}

func evidenceSource(item Instrument, own, edge bool) string {
	switch {
	case own:
		return item.Exchange + ".index_constituents"
	case edge:
		return "identity.explicit_edge"
	default:
		return "identity.two_sources"
	}
}

func identityEligible(ref UnderlyingReference) bool {
	switch ref.AssetClass {
	case AssetClassStock, AssetClassETF, AssetClassTradfiUnclassified:
		return !ref.PreMarket
	default:
		return false
	}
}

func directListing(ref UnderlyingReference) (listingID, bool) {
	if !ref.Routable() {
		return listingID{}, false
	}
	if ref.AssetClass != AssetClassStock && ref.AssetClass != AssetClassETF {
		return listingID{}, false
	}
	id := listingID{
		symbol:   strings.ToUpper(strings.TrimSpace(ref.Symbol)),
		region:   strings.ToUpper(strings.TrimSpace(ref.Region)),
		currency: strings.ToUpper(strings.TrimSpace(ref.UnderlyingCurrency)),
	}
	if id.currency == "" {
		id.currency = strings.ToUpper(strings.TrimSpace(ref.NativeListingCurrency))
	}
	if id.region == "" && id.currency == "USD" {
		id.region = "US"
	}
	if id.empty() {
		return listingID{}, false
	}
	return id, true
}

func explicitEdge(item Instrument, edges map[string]listingID) (listingID, bool) {
	keys := []string{contractKey(item.ExchangeSymbol), contractKey(item.BaseAsset + item.QuoteAsset)}
	for _, key := range keys {
		if id, ok := edges[item.Exchange+"|"+key]; ok && !id.empty() {
			return id, true
		}
	}
	return listingID{}, false
}

func allowsExplicitEdge(item Instrument) bool {
	if item.Underlying.AssetClass != AssetClassStock && item.Underlying.AssetClass != AssetClassETF {
		return false
	}
	if item.Exchange != "bitget" {
		return true
	}
	symbolType, rwa := bitgetStockFlags(item.Metadata)
	return strings.EqualFold(symbolType, "stock") && rwa
}

func bitgetStockFlags(raw json.RawMessage) (string, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", false
	}
	var symbolType string
	for _, key := range []string{"symbolType", "SymbolType"} {
		if value, ok := doc[key]; ok {
			_ = json.Unmarshal(value, &symbolType)
		}
	}
	var rwa flexibleBool
	if value, ok := doc["isRwa"]; ok {
		_ = json.Unmarshal(value, &rwa)
	}
	return symbolType, bool(rwa)
}

func candidateKeys(symbol string) []string {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil
	}
	keys := []string{symbol}
	if listedFXPair(symbol) {
		return keys
	}
	for _, suffix := range []string{"USD1", "USDT", "USDC", "USD"} {
		if strings.HasSuffix(symbol, suffix) && len(symbol) > len(suffix) {
			keys = append(keys, strings.TrimSuffix(symbol, suffix))
			break
		}
	}
	return keys
}

func listedFXPair(symbol string) bool {
	base, quote, ok := SplitFXPair(symbol)
	if !ok {
		return false
	}
	return currencyCode(base) && currencyCode(quote)
}

func currencyCode(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "USD", "EUR", "JPY", "GBP", "AUD", "CAD", "CHF", "NZD", "CNH", "HKD", "SGD", "KRW", "CNY", "USDT", "USDC":
		return true
	default:
		return false
	}
}

func contractKey(symbol string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(symbol), "_", ""))
}

func constituentEdge(exchangeName, symbol string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(exchangeName)) {
	case "bitgetfutures", "bitget":
		return "bitget", contractKey(symbol), true
	case "okxfutures", "okx":
		return "okx", contractKey(symbol), true
	case "binancefutures", "binance":
		return "binance", contractKey(symbol), true
	case "bybitfutures", "bybit":
		return "bybit", contractKey(symbol), true
	case "gatefutures", "gate":
		return "gate", contractKey(symbol), true
	case "hyperliquid":
		return "hyperliquid", contractKey(symbol), true
	case "aster", "asterfutures":
		return "aster", contractKey(symbol), true
	case "lighter":
		return "lighter", contractKey(symbol), true
	default:
		return "", "", false
	}
}

var nativeSuffix = regexp.MustCompile(`(?i)^([0-9]+)[._](HKD|HK|KRW|KR|CNY|CNH|CN|JPY|JP|USD|US)$`)

func parseNativeListing(symbol string) (listingID, bool) {
	raw := strings.ToUpper(strings.TrimSpace(symbol))
	raw = strings.ReplaceAll(raw, "-", "_")
	raw = strings.ReplaceAll(raw, ".", "_")
	if match := nativeSuffix.FindStringSubmatch(raw); match != nil {
		region, currency := suffixRegion(match[2])
		code, ok := nativeEquityCode(region, match[1])
		if !ok {
			return listingID{}, false
		}
		return listingID{symbol: code, region: region, currency: currency}, true
	}
	for _, currency := range []string{"HKD", "KRW", "CNY", "CNH", "JPY"} {
		if !strings.HasSuffix(raw, currency) {
			continue
		}
		region, listed := suffixRegion(currency)
		code, ok := nativeEquityCode(region, strings.TrimSuffix(raw, currency))
		if !ok {
			return listingID{}, false
		}
		return listingID{symbol: code, region: region, currency: listed}, true
	}
	return listingID{}, false
}

func nativeEquityCode(region, raw string) (string, bool) {
	if strings.EqualFold(region, "KR") {
		if _, ok := numericCode(raw); !ok {
			return "", false
		}
		return raw, true
	}
	return NormalizeEquityTicker(region, raw)
}

func suffixRegion(suffix string) (string, string) {
	switch strings.ToUpper(suffix) {
	case "HK", "HKD":
		return "HK", "HKD"
	case "KR", "KRW":
		return "KR", "KRW"
	case "CN", "CNY", "CNH":
		return "CN", "CNY"
	case "JP", "JPY":
		return "JP", "JPY"
	default:
		return "US", "USD"
	}
}

func nativeListings(rows []ConstituentRow) []listingID {
	seen := map[listingID]struct{}{}
	var result []listingID
	for _, row := range rows {
		id, ok := parseNativeListing(row.Symbol)
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func unknownIdentity(symbol, source string) UnderlyingReference {
	return UnderlyingReference{AssetClass: AssetClassUnknown, Symbol: strings.ToUpper(strings.TrimSpace(symbol)), Source: source}
}

func readConstituents(raw json.RawMessage) (ConstituentEvidence, bool) {
	if len(raw) == 0 {
		return ConstituentEvidence{}, false
	}
	var doc struct {
		Constituents *ConstituentEvidence `json:"constituents"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Constituents == nil {
		return ConstituentEvidence{}, false
	}
	return *doc.Constituents, true
}

func writeConstituents(raw json.RawMessage, evidence ConstituentEvidence) json.RawMessage {
	doc := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &doc)
	}
	doc["constituents"] = evidence
	encoded, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return encoded
}

func rejectAnnotationConflict(item Instrument) Instrument {
	id, ok := directListing(item.Underlying)
	if !ok {
		return item
	}
	var doc struct {
		Annotation hyperliquidAnnotation `json:"annotation"`
	}
	if err := json.Unmarshal(item.Metadata, &doc); err != nil {
		return item
	}
	for _, keyword := range doc.Annotation.Keywords {
		implied, impliedOK := keywordListing(keyword)
		if !impliedOK {
			continue
		}
		if !implied.same(id) {
			item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.annotation_conflict"))
			return item
		}
	}
	if implied, ok := displayListing(doc.Annotation.DisplayName); ok && !implied.same(id) {
		item.SetUnderlying(unknownIdentity(item.BaseAsset, "identity.annotation_conflict"))
	}
	return item
}

func keywordListing(keyword string) (listingID, bool) {
	text := strings.ToLower(strings.TrimSpace(keyword))
	if !strings.HasPrefix(text, "hk") {
		return listingID{}, false
	}
	code, ok := NormalizeEquityTicker("HK", strings.TrimPrefix(text, "hk"))
	if !ok {
		return listingID{}, false
	}
	return listingID{symbol: code, region: "HK", currency: "HKD"}, true
}

func displayListing(name string) (listingID, bool) {
	for _, region := range []string{"HK", "KR", "CN"} {
		code, ok := NormalizeEquityTicker(region, name)
		if !ok {
			continue
		}
		currency := "HKD"
		if region == "KR" {
			currency = "KRW"
		}
		if region == "CN" {
			currency = "CNY"
		}
		return listingID{symbol: code, region: region, currency: currency}, true
	}
	return listingID{}, false
}
