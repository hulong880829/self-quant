// Package info exposes the read-only query surface of the Hyperliquid
// REST API. Construct a Client indirectly via the top-level
// hyperliquid.New (or directly via info.New for read-only callers).
package info

import (
	"fmt"

	"github.com/Simon-Busch/hyperliquid-go/internal/transport"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

const (
	// spotAssetIndexOffset is the offset added to spot asset indices.
	spotAssetIndexOffset = 10000
	// builderPerpAssetBase is the base offset for builder-deployed perp
	// asset ids. See the Asset IDs docs:
	// asset = 100000 + perpDexIndex*10000 + indexInMeta.
	builderPerpAssetBase = 100000
	// outcomeAssetBase is the base offset for HIP-4 outcome (binary
	// prediction) market asset ids. See Asset IDs docs:
	// asset = 100_000_000 + (10*outcome + side).
	outcomeAssetBase = 100000000
)

// Client is the read-only query surface. Construct it indirectly via the
// top-level hyperliquid.New, or directly via info.New.
type Client struct {
	client         *transport.Client
	coinToAsset    map[string]int
	nameToCoin     map[string]string
	assetToDecimal map[int]int
	perpDexName    string       // For HIP-3 builder-deployed perps (e.g., "flx")
	outcomeMeta    *OutcomeMeta // Cached at construction so multi-bucket lookups don't re-fetch

	// Stake exposes the staking-info sub-group.
	Stake *StakeGroup
}

// Transport returns the underlying HTTP client used by this Info Client.
// Exposed so the root Trader can share the same connection pool.
func (c *Client) Transport() *transport.Client { return c.client }

// CoinToAssetMap returns the live map of coin-name to numeric asset id.
// The map is owned by the Client; callers must not mutate it.
func (c *Client) CoinToAssetMap() map[string]int { return c.coinToAsset }

// NameToCoinMap returns the live map of friendly name to canonical coin
// name. The map is owned by the Client; callers must not mutate it.
func (c *Client) NameToCoinMap() map[string]string { return c.nameToCoin }

// AssetToDecimalMap returns the live map of asset id to size decimals.
// The map is owned by the Client; callers must not mutate it.
func (c *Client) AssetToDecimalMap() map[int]int { return c.assetToDecimal }

// SzDecimals returns the size-decimals registered for asset, or zero if
// unknown.
func (c *Client) SzDecimals(asset int) int { return c.assetToDecimal[asset] }

// OutcomeMetaCached returns the OutcomeMeta snapshot captured during New,
// or nil if the call failed at construction. Useful for callers that want
// to navigate questions / buckets without paying another HTTP round-trip.
func (c *Client) OutcomeMetaCached() *OutcomeMeta { return c.outcomeMeta }

// PerpDexName returns the configured builder perp dex name (e.g. "flx"),
// or empty string for the default dex.
func (c *Client) PerpDexName() string { return c.perpDexName }

// postTimeRangeRequest makes a POST request with time-range parameters.
func (c *Client) postTimeRangeRequest(
	requestType, user string,
	startTime int64,
	endTime *int64,
	extraParams map[string]any,
) ([]byte, error) {
	payload := map[string]any{
		"type":      requestType,
		"startTime": startTime,
	}
	if user != "" {
		payload["user"] = user
	}
	if endTime != nil {
		payload["endTime"] = *endTime
	}
	for k, v := range extraParams {
		payload[k] = v
	}

	resp, err := c.client.Post("/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", requestType, err)
	}
	return resp, nil
}

// New creates a new Client. perpDexName is optional — pass an empty string
// for the default perp dex, or a builder dex name (e.g. "flx") for HIP-3
// builder-deployed perps.
func New(baseURL string, skipWS bool, meta *Meta, spotMeta *SpotMeta, perpDexs *types.MixedArray, perpDexName string) *Client {
	c := &Client{
		client:         transport.New(baseURL, nil),
		coinToAsset:    make(map[string]int),
		nameToCoin:     make(map[string]string),
		assetToDecimal: make(map[int]int),
		perpDexName:    perpDexName,
	}
	c.Stake = &StakeGroup{i: c}

	if meta == nil {
		var err error
		// When the client is pinned to a HIP-3 builder dex, the asset
		// registration below iterates the meta universe under the
		// assumption that it lists coins for THAT dex. Fetching the
		// default-dex meta would silently register the wrong universe
		// and break every subsequent AssetID / validate lookup.
		if c.perpDexName != "" {
			meta, err = c.Meta(c.perpDexName)
		} else {
			meta, err = c.Meta()
		}
		if err != nil {
			panic(err)
		}
	}

	if spotMeta == nil {
		var err error
		spotMeta, err = c.SpotMeta()
		if err != nil {
			panic(err)
		}
	}

	// Map perp assets
	if c.perpDexName != "" {
		// Builder-deployed perp: compute full asset id as documented.
		if perpDexs == nil {
			var err error
			perpDexsNew, err := c.PerpDexs()
			perpDexs = &perpDexsNew
			if err != nil {
				panic(err)
			}
		}
		perpDexIndex := -1
		for i, mv := range *perpDexs {
			if mv.Type() != "object" {
				continue
			}
			var pd PerpDex
			if err := mv.Parse(&pd); err == nil && pd.Name == c.perpDexName {
				perpDexIndex = i
				break
			}
		}
		if perpDexIndex < 0 {
			panic(fmt.Errorf("unknown perp dex %q (not present in /info perpDexs)", c.perpDexName))
		}
		base := builderPerpAssetBase + perpDexIndex*10000
		for idxInMeta, assetInfo := range meta.Universe {
			assetID := base + idxInMeta
			c.coinToAsset[assetInfo.Name] = assetID
			c.nameToCoin[assetInfo.Name] = assetInfo.Name
			c.assetToDecimal[assetID] = assetInfo.SzDecimals
		}
	} else {
		// Default perp dex: asset id is just index in meta universe.
		for asset, assetInfo := range meta.Universe {
			c.coinToAsset[assetInfo.Name] = asset
			c.nameToCoin[assetInfo.Name] = assetInfo.Name
			c.assetToDecimal[asset] = assetInfo.SzDecimals
		}
	}

	// Map spot assets starting at 10000. spotInfo.Tokens[0] is the index
	// of the base token in spotMeta.Tokens; Hyperliquid occasionally
	// returns spot entries whose base-token index is past the end of the
	// Tokens array (placeholder slots / paginated tail). In that case we
	// register the asset id but skip the szDecimals lookup rather than
	// panic — callers that need the decimals will get 0 and can fall
	// back to a metadata refresh.
	for _, spotInfo := range spotMeta.Universe {
		asset := spotInfo.Index + spotAssetIndexOffset
		c.coinToAsset[spotInfo.Name] = asset
		c.nameToCoin[spotInfo.Name] = spotInfo.Name
		if len(spotInfo.Tokens) > 0 {
			baseIdx := spotInfo.Tokens[0]
			if baseIdx >= 0 && baseIdx < len(spotMeta.Tokens) {
				c.assetToDecimal[asset] = spotMeta.Tokens[baseIdx].SzDecimals
			}
		}
	}

	// Map HIP-4 outcome assets starting at 100_000_000.
	// Failure here is non-fatal: outcomeMeta may be empty or missing on
	// some environments, and the SDK should still work for perp/spot users.
	if outcomeMeta, err := c.OutcomeMeta(); err == nil {
		c.outcomeMeta = outcomeMeta
		for _, oc := range outcomeMeta.Outcomes {
			// For named buckets of a Question, build a richer friendly
			// name like "<question>:<bucket label>:<Yes|No>" — three
			// price-bucket child outcomes can otherwise share the same
			// generic friendly name ("Recurring Named Outcome:Yes")
			// and collide in coinToAsset.
			bucketLabel := outcomeMeta.BucketLabel(oc.Outcome)
			parentQ := outcomeMeta.FindQuestion(oc.Outcome)

			for sideIdx, spec := range oc.SideSpecs {
				enc := 10*oc.Outcome + sideIdx
				asset := outcomeAssetBase + enc
				// Canonical name is "#<enc>" — empirically verified as the
				// form the exchange echoes in l2Book/allMids responses.
				// "+<enc>" form does NOT work for L2 queries despite being
				// mentioned in the docs, so we don't register it.
				canonical := fmt.Sprintf("#%d", enc)

				// Register the plain friendly form first for backwards
				// compatibility, then a bucket-aware form when the
				// outcome belongs to a multi-bucket question.
				friendly := fmt.Sprintf("%s:%s", oc.Name, spec.Name)
				c.coinToAsset[friendly] = asset
				c.nameToCoin[friendly] = canonical

				if parentQ != nil && bucketLabel != "" {
					bucketFriendly := fmt.Sprintf("%s:%s:%s", parentQ.Name, bucketLabel, spec.Name)
					c.coinToAsset[bucketFriendly] = asset
					c.nameToCoin[bucketFriendly] = canonical
				}

				c.coinToAsset[canonical] = asset
				c.nameToCoin[canonical] = canonical
				// szDecimals=0: HIP-4 contracts are integer-quantised. The L2
				// wire format shows sizes like "18.0" but the exchange rejects
				// fractional sizes ("Order has invalid size"). outcomeMeta
				// does not expose szDecimals; revisit if exchange behavior
				// changes.
				c.assetToDecimal[asset] = 0
			}
		}
	}

	_ = skipWS // reserved for compatibility with the legacy signature
	return c
}

// NewForTest builds a Client bypassing the network. Test-only — pass a
// stub transport (typically pointed at httptest.Server.URL) and the
// pre-populated asset tables you want to exercise.
func NewForTest(client *transport.Client, coinToAsset map[string]int, nameToCoin map[string]string, assetToDecimal map[int]int) *Client {
	c := &Client{
		client:         client,
		coinToAsset:    coinToAsset,
		nameToCoin:     nameToCoin,
		assetToDecimal: assetToDecimal,
	}
	if c.coinToAsset == nil {
		c.coinToAsset = map[string]int{}
	}
	if c.nameToCoin == nil {
		c.nameToCoin = map[string]string{}
	}
	if c.assetToDecimal == nil {
		c.assetToDecimal = map[int]int{}
	}
	c.Stake = &StakeGroup{i: c}
	return c
}
