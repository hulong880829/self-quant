package info

import (
	"encoding/json"
	"fmt"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

// AssetInfo is the per-asset row inside a Meta universe.
type AssetInfo struct {
	Name          string `json:"name"`
	SzDecimals    int    `json:"szDecimals"`
	MaxLeverage   int    `json:"maxLeverage"`
	MarginTableId int    `json:"marginTableId"`
	OnlyIsolated  bool   `json:"onlyIsolated"`
	IsDelisted    bool   `json:"isDelisted"`
}

// Meta is the perp universe metadata returned by /info {"type":"meta"}.
type Meta struct {
	Universe        []AssetInfo   `json:"universe"`
	MarginTables    []MarginTable `json:"marginTables"`
	CollateralToken int           `json:"collateralToken"`
}

// SpotAssetInfo is one entry in SpotMeta.Universe.
type SpotAssetInfo struct {
	Name        string `json:"name"`
	Tokens      []int  `json:"tokens"`
	Index       int    `json:"index"`
	IsCanonical bool   `json:"isCanonical"`
}

// EvmContract describes the EVM-side companion contract for a spot token.
type EvmContract struct {
	Address             string `json:"address"`
	EvmExtraWeiDecimals int    `json:"evm_extra_wei_decimals"`
}

// SpotTokenInfo describes a single spot token in the spot universe.
type SpotTokenInfo struct {
	Name        string       `json:"name"`
	SzDecimals  int          `json:"szDecimals"`
	WeiDecimals int          `json:"weiDecimals"`
	Index       int          `json:"index"`
	TokenID     string       `json:"tokenId"`
	IsCanonical bool         `json:"isCanonical"`
	EvmContract *EvmContract `json:"evmContract"`
	FullName    *string      `json:"fullName"`
}

// SpotMeta is the spot universe metadata returned by /info
// {"type":"spotMeta"}.
type SpotMeta struct {
	Universe []SpotAssetInfo `json:"universe"`
	Tokens   []SpotTokenInfo `json:"tokens"`
}

// OutcomeSideSpec describes one side (YES or NO) of a binary HIP-4 outcome.
type OutcomeSideSpec struct {
	Name string `json:"name"` // "Yes" or "No"
}

// OutcomeInfo describes a single binary prediction market.
//
// The Description field is a pipe-delimited string of key:value pairs,
// e.g. "class:priceBinary|underlying:BTC|expiry:20260507-0600|targetPrice:81287|period:1d".
type OutcomeInfo struct {
	Outcome     int               `json:"outcome"`     // numeric outcome ID
	Name        string            `json:"name"`        // e.g. "Recurring"
	Description string            `json:"description"` // structured metadata (see above)
	SideSpecs   []OutcomeSideSpec `json:"sideSpecs"`   // [YES, NO] in that order
	// QuoteToken is the spot token the market is collateralized and
	// settled in (e.g. "USDC"). HIP-4 launched quoting in USDH; the venue
	// has since migrated outcome markets to USDC as USDH is sunset, so a
	// Split mints / a buy pays in this token, not a hardcoded stablecoin.
	// Empty when the venue omits the field (older snapshots).
	QuoteToken string `json:"quoteToken"`
}

// Question groups several binary outcomes into a multi-bucket market.
// A price-bucket question over thresholds [T1, T2, ..., Tn] is split
// into n+1 child outcomes referenced by NamedOutcomes, each tradable
// on its own YES/NO sides. FallbackOutcome catches edge cases the
// named buckets do not cover (e.g. an oracle outage). The Description
// is the same pipe-delimited "k:v|k:v" format as OutcomeInfo and
// usually carries class, underlying, expiry, priceThresholds, period.
type Question struct {
	Question             int    `json:"question"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	FallbackOutcome      int    `json:"fallbackOutcome"`
	NamedOutcomes        []int  `json:"namedOutcomes"`
	SettledNamedOutcomes []int  `json:"settledNamedOutcomes"`
}

// OutcomeMeta is the response to POST /info {"type":"outcomeMeta"}.
// Outcomes lists every tradable binary YES/NO market; Questions groups
// them — multi-bucket markets (e.g. BTC price ranges) appear here.
type OutcomeMeta struct {
	Outcomes  []OutcomeInfo `json:"outcomes"`
	Questions []Question    `json:"questions"`
}

// SpotAssetCtx is the spot asset context payload returned alongside
// SpotMeta in spotMetaAndAssetCtxs.
type SpotAssetCtx struct {
	DayNtlVlm         string  `json:"dayNtlVlm"`
	MarkPx            string  `json:"markPx"`
	MidPx             *string `json:"midPx"`
	PrevDayPx         string  `json:"prevDayPx"`
	CirculatingSupply string  `json:"circulatingSupply"`
	Coin              string  `json:"coin"`
}

// AssetCtx represents perpetual asset context data including mark price, funding, open interest, etc.
type AssetCtx struct {
	DayNtlVlm    string   `json:"dayNtlVlm"`
	Funding      string   `json:"funding"`
	ImpactPxs    []string `json:"impactPxs"`
	MarkPx       string   `json:"markPx"`
	MidPx        string   `json:"midPx"`
	OpenInterest string   `json:"openInterest"`
	OraclePx     string   `json:"oraclePx"`
	Premium      string   `json:"premium"`
	PrevDayPx    string   `json:"prevDayPx"`
}

// MarginTier represents a single margin tier
type MarginTier struct {
	LowerBound  string `json:"lowerBound"`
	MaxLeverage int    `json:"maxLeverage"`
}

// MarginTable represents a margin table with description and tiers.
type MarginTable struct {
	ID          int
	Description string       `json:"description"`
	MarginTiers []MarginTier `json:"marginTiers"`
}

// UnmarshalJSON decodes Hyperliquid's positional
// [id, {description, marginTiers}] tuple form for a margin table. The id
// lives at index 0 and the table body at index 1, so a MarginTable is
// not a plain JSON object on the wire. Implementing this makes Meta
// default-unmarshalable everywhere (e.g. the webData2 frame), not just
// via the bespoke unpacking in parseMetaResponse.
func (m *MarginTable) UnmarshalJSON(b []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(b, &tuple); err != nil {
		return fmt.Errorf("marginTable: expected [id, table] tuple: %w", err)
	}
	if len(tuple) != 2 {
		return fmt.Errorf("marginTable: expected 2 elements, got %d", len(tuple))
	}
	if err := json.Unmarshal(tuple[0], &m.ID); err != nil {
		return fmt.Errorf("marginTable id: %w", err)
	}
	var body struct {
		Description string       `json:"description"`
		MarginTiers []MarginTier `json:"marginTiers"`
	}
	if err := json.Unmarshal(tuple[1], &body); err != nil {
		return fmt.Errorf("marginTable body: %w", err)
	}
	m.Description, m.MarginTiers = body.Description, body.MarginTiers
	return nil
}

// MetaAndAssetCtxsResponse represents the response from the metaAndAssetCtxs endpoint
// The API returns an array with two elements: [meta, assetCtxs]
type MetaAndAssetCtxsResponse struct {
	Meta      Meta       `json:"universe"`
	AssetCtxs []AssetCtx `json:"assetCtxs"`
}

// MetaAndAssetCtxsRawResponse represents the raw array response from the API
type MetaAndAssetCtxsRawResponse [2]interface{}

// PerpDexSchemaInput is the per-dex registration payload for HIP-3 perp
// deploys.
type PerpDexSchemaInput struct {
	FullName        string  `json:"fullName"`
	CollateralToken int     `json:"collateralToken"`
	OracleUpdater   *string `json:"oracleUpdater"`
}

// PerpDex represents a perpetual DEX
type PerpDex struct {
	Name                     string     `json:"name"`
	FullName                 string     `json:"fullName"`
	Deployer                 string     `json:"deployer"`
	OracleUpdater            *string    `json:"oracleUpdater"`
	FeeRecipient             *string    `json:"feeRecipient"`
	AssetToStreamingOiCap    [][]string `json:"assetToStreamingOiCap"`    // Array of [coin, cap] tuples
	AssetToFundingMultiplier [][]string `json:"assetToFundingMultiplier"` // Array of [coin, multiplier] tuples
}

// PerpDexLimits represents limits for a builder-deployed perp DEX
type PerpDexLimits struct {
	TotalOiCap     string     `json:"totalOiCap"`
	OiSzCapPerPerp string     `json:"oiSzCapPerPerp"`
	MaxTransferNtl string     `json:"maxTransferNtl"`
	CoinToOiCap    [][]string `json:"coinToOiCap"` // Array of [coin, cap] tuples
}

// PerpDexStatus represents status for a builder-deployed perp DEX
type PerpDexStatus struct {
	TotalNetDeposit string `json:"totalNetDeposit"`
}

// PerpDeployAuctionStatus represents the status of a perp deploy auction
type PerpDeployAuctionStatus struct {
	StartTimeSeconds int64   `json:"startTimeSeconds"`
	DurationSeconds  int64   `json:"durationSeconds"`
	StartGas         string  `json:"startGas"`
	CurrentGas       string  `json:"currentGas"`
	EndGas           *string `json:"endGas"`
}

// Meta retrieves perpetuals metadata. If dex is provided and non-empty,
// the snapshot is pinned to that HIP-3 dex; otherwise the default dex is
// returned.
func (c *Client) Meta(dex ...string) (*Meta, error) {
	payload := map[string]any{
		"type": "meta",
	}
	if len(dex) > 0 && dex[0] != "" {
		payload["dex"] = dex[0]
	}

	resp, err := c.client.Post("/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch meta: %w", err)
	}

	return parseMetaResponse(resp)
}

func parseMetaResponse(resp []byte) (*Meta, error) {
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(resp, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal meta response: %w", err)
	}

	var universe []AssetInfo
	if err := json.Unmarshal(meta["universe"], &universe); err != nil {
		return nil, fmt.Errorf("failed to unmarshal universe: %w", err)
	}

	var collateralToken int
	if ct, ok := meta["collateralToken"]; ok {
		if err := json.Unmarshal(ct, &collateralToken); err != nil {
			return nil, fmt.Errorf("failed to unmarshal collateralToken: %w", err)
		}
	}

	var marginTables []MarginTable
	if mt, ok := meta["marginTables"]; ok {
		var marginTablesRaw [][]any
		if err := json.Unmarshal(mt, &marginTablesRaw); err != nil {
			return nil, fmt.Errorf("failed to unmarshal margin tables: %w", err)
		}

		marginTables = make([]MarginTable, len(marginTablesRaw))
		for idx, marginTable := range marginTablesRaw {
			if len(marginTable) < 2 {
				continue
			}
			id := int(marginTable[0].(float64))
			tableBytes, err := json.Marshal(marginTable[1])
			if err != nil {
				return nil, fmt.Errorf("failed to marshal margin table data: %w", err)
			}

			var marginTableData map[string]any
			if err := json.Unmarshal(tableBytes, &marginTableData); err != nil {
				return nil, fmt.Errorf("failed to unmarshal margin table data: %w", err)
			}

			marginTiersBytes, err := json.Marshal(marginTableData["marginTiers"])
			if err != nil {
				return nil, fmt.Errorf("failed to marshal margin tiers: %w", err)
			}

			var marginTiers []MarginTier
			if err := json.Unmarshal(marginTiersBytes, &marginTiers); err != nil {
				return nil, fmt.Errorf("failed to unmarshal margin tiers: %w", err)
			}

			desc := ""
			if d, ok := marginTableData["description"].(string); ok {
				desc = d
			}

			marginTables[idx] = MarginTable{
				ID:          id,
				Description: desc,
				MarginTiers: marginTiers,
			}
		}
	}

	return &Meta{
		Universe:        universe,
		MarginTables:    marginTables,
		CollateralToken: collateralToken,
	}, nil
}

// SpotMeta retrieves the canonical spot metadata.
func (c *Client) SpotMeta() (*SpotMeta, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "spotMeta",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch spot meta: %w", err)
	}

	var spotMeta SpotMeta
	if err := json.Unmarshal(resp, &spotMeta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spot meta response: %w", err)
	}

	return &spotMeta, nil
}

// OutcomeMeta retrieves metadata for HIP-4 binary outcome (prediction)
// markets. Returns the list of active outcomes, each with its sides
// (YES/NO). May return an empty slice if no markets are live on the
// target environment.
func (c *Client) OutcomeMeta() (*OutcomeMeta, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "outcomeMeta",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch outcome meta: %w", err)
	}

	var outcomeMeta OutcomeMeta
	if err := json.Unmarshal(resp, &outcomeMeta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal outcome meta response: %w", err)
	}

	return &outcomeMeta, nil
}

// PerpDexs returns the list of available perpetual dexes. Each element is
// either nil (the default dex) or a PerpDex object. The first element is
// always nil, representing the default dex.
func (c *Client) PerpDexs() (types.MixedArray, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "perpDexs",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch perp dexs: %w", err)
	}

	var result types.MixedArray
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal perp dexs: %w", err)
	}
	return result, nil
}
