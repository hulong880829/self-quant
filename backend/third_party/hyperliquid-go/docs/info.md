# Info reference (`c.Info`)

`c.Info` is the read-only surface. Every method here issues a single POST to `/info`, parses the response, and returns a typed value. No signing, no caching beyond what the HTTP client does, no rate-limit handling — wrap calls in your own backoff if you hammer the endpoint.

`c.Info` is always non-`nil` on a `Client` built by `hyperliquid.New`. Its underlying type is `*info.Client` from the [`info`](https://pkg.go.dev/github.com/Simon-Busch/hyperliquid-go/info) subpackage; advanced callers can construct one directly via `info.New(baseURL, skipWS, meta, spotMeta, perpDexs, perpDexName)`. Response types (`UserState`, `Fill`, `Meta`, `L2Book`, …) live in the same subpackage.

## Contents

- [Market data](#market-data)
- [Account state](#account-state)
- [Orders and fills](#orders-and-fills)
- [Funding](#funding)
- [Staking](#staking)
- [Metadata](#metadata)
- [Account directory](#account-directory)

---

## Market data

### `Mid` {#mid}

Return the current mid price for `coin`, parsed from the wire's string-encoded format.

```go
func (c *info.Client) Mid(coin string) (float64, error)
```

**Example**

```go
mid, err := c.Info.Mid("ETH")
```

### `AllMids`, `AllMidsOn` {#allmids}

Return mids for every coin in a single snapshot. `AllMids` accepts an optional dex argument; `AllMidsOn` is the explicit single-dex variant.

```go
func (c *info.Client) AllMids(dex ...string) (map[string]string, error)
func (c *info.Client) AllMidsOn(dex string) (map[string]string, error)
```

**Example**

```go
all, err   := c.Info.AllMids()         // default dex
flx, err   := c.Info.AllMidsOn("flx")  // HIP-3 dex
```

### `Book` {#book}

Return the current L2 order book for `coin`.

```go
func (c *info.Client) Book(coin string) (*info.L2Book, error)
```

`info.L2Book.Levels` is a `[2][]info.Level` — index 0 = bids, index 1 = asks.

### `Candles` {#candles}

Return historical candles for `coin` at `interval` between `start` and `end` (Unix millis).

```go
func (c *info.Client) Candles(coin, interval string, start, end int64) ([]info.Candle, error)
```

**Example**

```go
now  := time.Now().UnixMilli()
last := now - 24*3600*1000
candles, err := c.Info.Candles("ETH", "1h", last, now)
```

### `MetaAndAssetCtxs`, `SpotMetaAndAssetCtxs` {#metaandassetctxs}

Fetch perp (or spot) metadata together with the per-asset context snapshot in one call.

```go
func (c *info.Client) MetaAndAssetCtxs() (*info.MetaAndAssetCtxsResponse, error)
func (c *info.Client) SpotMetaAndAssetCtxs() (map[string]any, error)
```

`SpotMetaAndAssetCtxs` returns a raw map for now — the typed envelope is a follow-up.

---

## Account state

### `UserState` {#userstate}

Return the caller's perpetuals account summary (positions, margin, withdrawable).

```go
func (c *info.Client) UserState(address string, dex ...string) (*info.UserState, error)
```

### `SpotBalances` {#spotbalances}

Return the caller's spot clearinghouse state.

```go
func (c *info.Client) SpotBalances(addr string) (*info.SpotClearinghouseState, error)
```

### `Positions`, `Position` {#positions}

`Positions` returns every open position for `addr`. `Position` returns the single position on `coin`, or `(nil, nil)` if none.

```go
func (c *info.Client) Positions(addr string, dex ...string) ([]info.Position, error)
func (c *info.Client) Position(addr, coin string) (*info.Position, error)
```

**Example**

```go
pos, err := c.Info.Position("0xabc...", "ETH")
if err != nil  { log.Fatal(err) }
if pos == nil  { fmt.Println("no ETH position") }
```

### `Fees` {#fees}

Return the fee snapshot for `addr`.

```go
func (c *info.Client) Fees(addr string) (*info.UserFees, error)
```

### `Asset`, `AssetID` {#asset}

`Asset` returns the per-coin metadata snapshot used by `validate()`: asset id, size decimals, tick size, minimum size, and the asset class (perp / spot / HIP-3 / outcome). `AssetID` returns just the numeric id.

```go
func (c *info.Client) Asset(coin string) (info.AssetMeta, error)
func (c *info.Client) AssetID(coin string) int
```

**Example**

```go
meta, _ := c.Info.Asset("ETH")
fmt.Printf("tick=%v min=%v dec=%d\n", meta.TickSize, meta.MinSize, meta.SzDecimals)
```

---

## Orders and fills

### `OpenOrders`, `FrontendOpenOrders` {#openorders}

`OpenOrders` returns the wire-shape open orders. `FrontendOpenOrders` returns the richer UI-shape variant which includes TP/SL leg metadata.

```go
func (c *info.Client) OpenOrders(address string, dex ...string) ([]info.OpenOrder, error)
func (c *info.Client) FrontendOpenOrders(address string, dex ...string) ([]info.FrontendOpenOrder, error)
```

### `Fills`, `FillsBetween` {#fills}

`Fills` returns all fills for `addr`. `FillsBetween` filters by time range (Unix millis); `end == nil` means "until now".

```go
func (c *info.Client) Fills(addr string) ([]info.Fill, error)
func (c *info.Client) FillsBetween(addr string, start int64, end *int64) ([]info.Fill, error)
```

### `Order`, `OrderByCloid`, `Fill` {#order}

Look up a specific order or fill by id.

```go
func (c *info.Client) Order(addr string, oid int64) (*info.OrderStatusResponse, error)
func (c *info.Client) OrderByCloid(addr, cloid string) (*info.OpenOrder, error)
func (c *info.Client) Fill(addr string, oid int64) (*info.Fill, error)
```

---

## Funding

### `Funding`, `UserFunding` {#funding}

`Funding` returns the historical funding rates for `coin`. `UserFunding` returns the per-user funding payments for `addr`. Both accept a `*int64` end timestamp (`nil` = now).

```go
func (c *info.Client) Funding(coin string, start int64, end *int64) ([]info.FundingHistory, error)
func (c *info.Client) UserFunding(addr string, start int64, end *int64) ([]info.UserFundingHistory, error)
```

---

## Staking

Accessible via `c.Info.Stake`.

### `Stake.Summary`, `Stake.Delegations`, `Stake.Rewards` {#staking}

```go
func (g *info.StakeGroup) Summary(addr string) (*info.StakingSummary, error)
func (g *info.StakeGroup) Delegations(addr string) ([]info.StakingDelegation, error)
func (g *info.StakeGroup) Rewards(addr string) ([]info.StakingReward, error)
```

---

## Metadata

### `Meta`, `SpotMeta`, `OutcomeMeta`, `PerpDexs` {#metadata}

Raw metadata endpoints. `OutcomeMeta` covers HIP-4 binary-prediction markets.

```go
func (c *info.Client) Meta(dex ...string) (*info.Meta, error)
func (c *info.Client) SpotMeta() (*info.SpotMeta, error)
func (c *info.Client) OutcomeMeta() (*info.OutcomeMeta, error)
func (c *info.Client) PerpDexs() (types.MixedArray, error)
```

`c.Info.Meta` accepts an optional dex name; pass it to fetch a HIP-3 dex's universe.

---

## Account directory

### `SubAccounts`, `Referral`, `MultiSigSigners` {#account-directory}

```go
func (c *info.Client) SubAccounts(addr string) ([]info.SubAccount, error)
func (c *info.Client) Referral(addr string) (*info.ReferralState, error)
func (c *info.Client) MultiSigSigners(multiSigAddr string) ([]info.MultiSigSigner, error)
```
