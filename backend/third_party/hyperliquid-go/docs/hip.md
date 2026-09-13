# HIPs — spot deploy, builder dexes, outcome markets

A single page covering the SDK's surface for the three HIP families:

- **HIP-2** — deploy a spot token and its market.
- **HIP-3** — interact with builder-deployed perp dexes (third-party-operated perp universes).
- **HIP-4** — trade binary outcome (prediction) markets and mint/burn outcome shares.

Per-method reference for everything below lives in [trading.md](./trading.md) and [info.md](./info.md). This page is the cross-cutting walkthrough.

## HIP-3 — builder-deployed perp dexes

Builder dexes (e.g. `flx`) are perp universes deployed by third parties. The SDK exposes them through three orthogonal pieces:

1. **`WithBuilderDex("flx")`** pins the client. Every signing path, asset-id lookup, and validation step targets that dex.
2. **`c.Info.AllMidsOn`, `c.Info.Meta(dex)`, `c.Info.PerpDexs`** read the dex's universe and prices without pinning.
3. **`c.Trade.Transfer.MoveToDex` / `MoveFromDex`** move USDC between the default perp wallet and a builder dex.

### Pin a client to a builder dex

```go
c, err := hl.New(
    hl.WithTestnet(),
    hl.WithPrivateKey(pk),
    hl.WithBuilderDex("flx"),
)
if err != nil { log.Fatal(err) }

mids, err := c.Info.AllMidsOn("flx")
if err != nil { log.Fatal(err) }
fmt.Println("FLX universe mids:", mids)

res, err := c.Trade.PlaceALO("FLX-PERP", types.Buy, 1, mids["FLX-PERP"]*0.99)
```

### Query a dex without pinning

```go
dexes, err := c.Info.PerpDexs()             // every builder dex
meta, err  := c.Info.Meta("flx")            // FLX universe
mids, err  := c.Info.AllMidsOn("flx")       // FLX mids only
```

### Move balance into / out of a builder dex

```go
// 1 USDC: default perp -> "flx" dex
_, err := c.Trade.Transfer.MoveToDex("flx", "USDC", 1.0)

// 1 USDC: "flx" dex -> default perp
_, err = c.Trade.Transfer.MoveFromDex("flx", "USDC", 1.0)
```

### Stream a HIP-3 dex

```go
sub, err := c.Stream.Subscribe(stream.AllMidsOn("flx"), func(m stream.WSMessage) {
    fmt.Println("flx mids:", string(m.Data))
})
```

## HIP-4 — binary outcome (prediction) markets

HIP-4 outcomes look like regular perp assets to the placement verbs: name, side, size, price. Three things differ:

- **Names.** Outcomes resolve by **friendly name** (`<question>:<side>`, e.g. `"BTC > 100k by Dec 31:Yes"`) or **canonical id** (`#<enc>`). Both resolve to the same numeric asset id (≥ 100,000,000). Use whichever you have on hand.
- **Sizes are integers.** The `MinSize` for every outcome is 1, and the size step is 1. `PlaceALO("...:Yes", types.Buy, 0.5, …)` is rejected by `validate()` before signing.
- **Prices are probabilities in (0, 1].** A buy at 0.5 means "I'm paying 50¢ for a $1 payout if this resolves true."

### Discover outcomes

```go
meta, err := c.Info.OutcomeMeta()
for _, o := range meta.Outcomes {
    fmt.Printf("%s  id=%d  desc=%q\n", o.Name, o.AssetID, o.Description)
}

// Multi-bucket questions and their human-readable labels
for _, q := range meta.Questions {
    labels := meta.QuestionLabels(&q)
    fmt.Println(q.Name, "->", labels)
}
```

`Question.BucketLabels` derives `["Below T1", "T1 to T2", ..., "Above Tn"]` for `class:priceBucket` questions whose threshold count matches the named-outcome count. For categorical markets (Champions League winner, Fed rate decision), `OutcomeMeta.QuestionLabels` falls back to `OutcomeInfo.Name`.

### Place an outcome order

```go
mid, err := c.Info.Mid("BTC > 100k by Dec 31:Yes")
if err != nil { log.Fatal(err) }

// 1 share, post-only, 5% under mid.
res, err := c.Trade.PlaceALO(
    "BTC > 100k by Dec 31:Yes",
    types.Buy,
    1,
    mid*0.95,
)
```

The canonical name works identically:

```go
res, err := c.Trade.PlaceALO("#abcd1234", types.Buy, 1, 0.45)
```

### Stream outcome trades

Subscriptions take the **canonical** name (the human-readable form is rejected by the WS gateway):

```go
sub, err := c.Stream.Subscribe(stream.Trades("#abcd1234"), handler)
```

### Mint / burn outcome shares against the quote token

The HIP-4 conversion verbs live on `c.Trade.Outcome`. They never touch the order book — they mint or burn outcome shares against the market's **quote-token** collateral.

> **Collateral token.** HIP-4 launched quoting in USDH. The venue has since migrated outcome markets to **USDC** as USDH is sunset, so the amounts below are denominated in whatever token the market reports as `OutcomeInfo.QuoteToken` (USDC on current markets). The wire action carries no collateral field — the token is implicit in the market — so the SDK calls are unchanged across the migration. Read `OutcomeInfo.QuoteToken` to confirm what a given market settles in.

| Verb              | Effect                                                                            |
|-------------------|-----------------------------------------------------------------------------------|
| `Split`           | `X quote -> X Yes + X No` of one outcome.                                         |
| `Merge`           | `X Yes + X No` of one outcome `-> X quote`. Pass `nil` amount for max.            |
| `MergeQuestion`   | `X Yes` of every named outcome of a question `-> X quote`. Pass `nil` for max.    |
| `Negate`          | `X No` of one bucket `-> X Yes` of every OTHER bucket in the same question.      |

```go
// Mint 10 Yes + 10 No of outcome #abcd1234 from 10 units of quote token (USDC).
_, err := c.Trade.Outcome.Split(outcomeID, 10)

// Burn the max holdable Yes/No pair back to the quote token.
_, err = c.Trade.Outcome.Merge(outcomeID, nil)
```

`outcomeID` is the `uint64` returned in `OutcomeInfo.AssetID` (or by `Info.AssetID("name")`).

## HIP-2 — spot deploy

HIP-2 deploys a spot token and registers its market. The full lifecycle:

1. `SpotDeployRegisterToken` — register a token name + decimals.
2. `SpotDeployUserGenesis` — seed initial balances.
3. `SpotDeployGenesis` — initialise the deployer.
4. `SpotDeployRegisterSpot` — register the spot market.
5. `SpotDeployRegisterHyperliquidity` — bootstrap hyperliquidity for the new market.
6. `SpotDeploySetDeployerTradingFeeShare` — configure the deployer fee share.
7. (Optional) `SpotDeployEnableFreezePrivilege` / `SpotDeployFreezeUser` / `SpotDeployRevokeFreezePrivilege` — moderation controls.

Each verb is a single signed action; see [trading.md → HIP-2 / HIP-3 deploy](./trading.md#deploy) for the exact signatures and payload shapes.

## Cross-references

- Per-method reference: [trading.md](./trading.md), [info.md](./info.md), [stream.md](./stream.md).
- Integration scenarios that exercise HIP-3 / HIP-4 end-to-end: [integration-testing.md](./integration-testing.md#hip-3--builder-deployed-perpetuals).
- Validator error codes (`size_step_mismatch`, `asset_unknown`, …) surface uniformly across HIP-2/3/4 placements: [errors.md](./errors.md).
