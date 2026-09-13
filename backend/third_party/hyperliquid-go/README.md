# go-hyperliquid-0xsi

Idiomatic Go SDK for the [Hyperliquid](https://hyperliquid.xyz) exchange.

## What's covered

- Perps trading (placement, modify, cancel, brackets, triggers, close).
- Spot trading and balances.
- HIP-3 builder-deployed perp dexes (via `WithBuilderDex`).
- HIP-4 outcome (binary prediction) markets.
- WebSocket subscriptions (market and per-user feeds) with automatic resubscription on reconnect.
- POST-over-WebSocket for low-latency reads and signed actions.
- Native Go EIP-712 signing — no Python bridge.
- Pre-flight validation against the cached `UserState` and per-asset metadata.

## Status

Active fork of [`sonirico/go-hyperliquid`](https://github.com/sonirico/go-hyperliquid), restructured around the three-handle facade (`Info` / `Trade` / `Stream`) and extended with HIP-3 / HIP-4 support, pre-flight validation, and POST-over-WebSocket. The integration suite runs against testnet — see [docs/integration-testing.md](./docs/integration-testing.md).

## Install

Requires Go 1.23 or newer.

```bash
go get github.com/Simon-Busch/hyperliquid-go@latest
```

Conventionally aliased to `hl`:

```go
import hl "github.com/Simon-Busch/hyperliquid-go"
```

## Quickstart

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/ethereum/go-ethereum/crypto"
    hl "github.com/Simon-Busch/hyperliquid-go"
    "github.com/Simon-Busch/hyperliquid-go/stream"
    "github.com/Simon-Busch/hyperliquid-go/types"
)

func main() {
    pk, err := crypto.HexToECDSA("<hex private key>")
    if err != nil { log.Fatal(err) }

    c, err := hl.New(hl.WithTestnet(), hl.WithPrivateKey(pk))
    if err != nil { log.Fatal(err) }

    mid, err := c.Info.Mid("ETH")
    if err != nil { log.Fatal(err) }
    fmt.Printf("ETH mid: %.2f\n", mid)

    res, err := c.Trade.PlaceALO("ETH", types.Buy, 0.01, mid*0.99)
    if err != nil { log.Fatal(err) }
    fmt.Printf("placed oid=%d\n", res.OID)

    _, _ = c.Trade.CancelAll("ETH")

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := c.Stream.Connect(ctx); err != nil { log.Fatal(err) }
    defer c.Stream.Close()

    sub, err := c.Stream.Subscribe(stream.Trades("ETH"), func(m stream.WSMessage) {
        fmt.Println("trade:", string(m.Data))
    })
    if err != nil { log.Fatal(err) }
    defer sub.Close()
    time.Sleep(5 * time.Second)
}
```

A full walkthrough — `.env` setup, bracketed entries, `ClosePosition`, multi-leg batches — lives in [docs/quickstart.md](./docs/quickstart.md).

## The three services

| Handle     | Responsibility                                                  | Reference                            |
|------------|-----------------------------------------------------------------|--------------------------------------|
| `c.Info`   | Read-only queries — mids, books, orders, fills, metadata.       | [docs/info.md](./docs/info.md)       |
| `c.Trade`  | Signed actions — placement, transfers, leverage, governance.    | [docs/trading.md](./docs/trading.md) |
| `c.Stream` | WebSocket streaming + POST-over-WS.                             | [docs/stream.md](./docs/stream.md)   |

## Common patterns

**Bracketed entry.** `WithBracket(tp, sl)` attaches reduce-only trigger legs that fire when the parent fills; cancelling the parent cancels the children.

```go
import (
    "github.com/Simon-Busch/hyperliquid-go/trade"
    "github.com/Simon-Busch/hyperliquid-go/types"
)

res, err := c.Trade.PlaceGTC(
    "ETH", types.Buy, 0.01, 1500,
    trade.WithBracket(1600, 1450),
)
```

**Close a position with auto-direction.** `ClosePosition` reads the cached state, infers direction, and submits a reduce-only IOC. `trade.WithLimit(px)` switches to a limit close; `trade.WithSize(x)` makes it partial.

```go
res, err := c.Trade.ClosePosition("ETH")
```

**Multi-leg single-signature batch.** Build specs with the package-level constructors in `trade` and submit them all under one signed action.

```go
res, err := c.Trade.PlaceMany(
    trade.GTC("ETH", types.Buy,  0.01, 1500),
    trade.IOC("BTC", types.Sell, 0.001, 70_000),
)
```

**HIP-3 builder-deployed dex.** Pin the client to a builder dex with `WithBuilderDex` — every read, signing path, and validation step targets that dex.

```go
c, err := hl.New(
    hl.WithTestnet(),
    hl.WithPrivateKey(pk),
    hl.WithBuilderDex("flx"),
)
mids, err := c.Info.AllMidsOn("flx")
res, err  := c.Trade.PlaceALO("FLX-PERP", types.Buy, 1, mids["FLX-PERP"]*0.99)
```

**HIP-4 outcome markets.** Outcomes are addressed by their friendly `<question>:<side>` name or canonical `#<enc>` id. `Info.OutcomeMeta` returns the catalogue; orders go through the regular placement verbs with integer sizes.

```go
meta, err := c.Info.OutcomeMeta()
mid, err  := c.Info.Mid("BTC > 100k by Dec 31:Yes")
res, err  := c.Trade.PlaceALO("BTC > 100k by Dec 31:Yes", types.Buy, 1, mid*0.5)
```

See [docs/hip.md](./docs/hip.md) for the full HIP-2 / HIP-3 / HIP-4 walkthrough.

**Streaming with reconnect diagnostics.** `c.Stream` automatically resubscribes after a reconnect. Plug in a `stream.Logger` to surface backoff and resub events.

```go
c, err := hl.New(hl.WithTestnet(), hl.WithLogger(myLogger))
_      = c.Stream.Connect(ctx)
sub, _ := c.Stream.Subscribe(stream.UserEvents(addr), handler)
// On disconnect: backoff -> reconnect -> resubscribe the open sub. No caller action needed.
```

## Validation

Every placement, modify, cancel, and close call runs `validate()` before signing. The validator refreshes the cached `UserState` and checks the spec against the asset metadata: size step, tick alignment, significant figures, reduce-only direction, bracket TP/SL placement, close direction, option/method compatibility. Failures surface as `*hyperliquid.ValidationError` with a stable machine-readable `Code` — see [docs/errors.md](./docs/errors.md) for the full table.

Opt out per call with `trade.SkipValidation()` if you have your own checks or you're trading from a state the SDK cannot see (e.g. an unprovisioned agent address).

## Configuration

`hl.New` accepts the following functional options:

| Option                            | Effect                                                                 |
|-----------------------------------|------------------------------------------------------------------------|
| `WithMainnet()`                   | Pin the client to `https://api.hyperliquid.xyz` (default).             |
| `WithTestnet()`                   | Pin the client to `https://api.hyperliquid-testnet.xyz`.               |
| `WithBaseURL(url)`                | Override the base URL.                                                 |
| `WithPrivateKey(pk)`              | Required for `c.Trade`. Accepts `*ecdsa.PrivateKey`.                   |
| `WithAccount(addr)`               | Owner-address override for agent flows.                                |
| `WithVault(addr)`                 | Sign actions on behalf of a vault address.                             |
| `WithBuilderDex(dex)`             | Pin the client to a HIP-3 builder-deployed dex.                        |
| `WithHTTPClient(c)`               | Inject a custom `*http.Client`.                                        |
| `WithMeta(m, sm, pd)`             | Use pre-fetched metadata; skips the warm-up calls.                     |
| `WithSkipStream(true)`            | Do not construct `c.Stream`.                                           |
| `WithLogger(l)`                   | Plug a `Logger` into `c.Stream` for reconnect diagnostics.             |

## Running the integration suite

The default `go test ./...` is unit-only — no network. To run the live suite:

```bash
go test -tags=integration ./tests/integration/...
```

Required env: `HL_PRIVATE_KEY`. Optional: `HL_ACCOUNT_ADDRESS`, `HL_BASE_URL` (defaults to testnet), `HL_TEST_COIN`, `HL_TEST_SIZE`. Full env table, scenario list, and troubleshooting tips: [docs/integration-testing.md](./docs/integration-testing.md).

## Documentation

- [`docs/quickstart.md`](./docs/quickstart.md) — end-to-end first-trade walkthrough.
- [`docs/README.md`](./docs/README.md) — reference TOC with anchors to every exported function.
- [`docs/trading.md`](./docs/trading.md) — every method on `c.Trade`.
- [`docs/info.md`](./docs/info.md) — every method on `c.Info`.
- [`docs/stream.md`](./docs/stream.md) — `c.Stream` and the subscription constructors.
- [`docs/hip.md`](./docs/hip.md) — HIP-2 (spot deploy), HIP-3 (builder dex), HIP-4 (outcome markets) end-to-end.
- [`docs/signing.md`](./docs/signing.md) — `SignL1Action`, `SignUserSignedAction`, and friends.
- [`docs/errors.md`](./docs/errors.md) — `ValidationError` codes, `APIError`, sentinel errors.
- [`docs/integration-testing.md`](./docs/integration-testing.md) — env, scenarios, troubleshooting.
- godoc on [pkg.go.dev](https://pkg.go.dev/github.com/Simon-Busch/hyperliquid-go).

## Project layout

The SDK is split into a thin facade (the root `hyperliquid` package) and a
handful of focused subpackages. The facade is the recommended entry point;
each subpackage can also be imported directly when callers want a smaller
dependency surface or want to construct one handle without going through
`hyperliquid.New`.

```
hyperliquid/                # facade — re-exports New, Client, options, errors
├── client.go               # New(), Client, c.Info / c.Trade / c.Stream wiring
├── options.go              # WithMainnet/Testnet/PrivateKey/Account/...
├── doc.go                  # package doc
│
├── types/                  # shared domain types
│   ├── side.go             # Side (Buy/Sell), TIF, MarginMode
│   ├── orderspec.go        # OrderSpec value type
│   ├── result.go           # Result, BatchResult, CancelResult, BatchCancelResult
│   ├── order_type.go       # OrderType / OrderTypeWire family
│   ├── grouping.go         # Grouping enum + DefaultSlippage
│   ├── asset_class.go      # AssetClass + ClassifyAsset
│   ├── api_response.go     # APIError, raw APIResponse envelope
│   ├── urls.go             # MainnetAPIURL, TestnetAPIURL
│   ├── errors.go           # ValidationError, ErrMissingPrivateKey
│   └── mixed.go            # MixedArray / MixedValue
│
├── signing/                # EIP-712 signing helpers + wire action structs
│   ├── signing.go          # SignL1Action, SignUserSignedAction, FloatToUsdInt, GetTimestampMs, SignatureResult
│   └── actions.go          # OrderWire, CancelAction, TWAPOrderAction, ...
│
├── info/                   # read-only REST surface
│   ├── info.go             # info.New + Client core
│   ├── market.go           # Mid, AllMids, Book, Candles, MetaAndAssetCtxs
│   ├── account.go          # UserState, SpotBalances, Positions, Asset
│   ├── orders.go           # OpenOrders, Fills, Order, OrderByCloid
│   ├── meta.go             # Meta, SpotMeta, OutcomeMeta, PerpDexs
│   ├── funding.go          # Funding, UserFunding
│   ├── staking.go          # Info.Stake group
│   └── outcome_question.go # HIP-4 outcome metadata helpers (ParseOutcomeDescription, QuestionLabels, ...)
│
├── trade/                  # signed-action surface
│   ├── exchange.go         # trade.Client struct + subgroup wiring
│   ├── trade.go            # internal place() pipeline shared by every placement verb
│   ├── place.go            # PlaceALO/IOC/GTC/Market/Trigger/Many + OrderSpec constructors
│   ├── exchange_orders.go  # Modify, ModifyByCloid, ScheduleCancelAll
│   ├── exchange_orders_cancel.go  # Cancel / CancelByCloid / CancelAll
│   ├── modify_cancel.go    # ClosePosition, SetLeverage, AdjustMargin, RefreshState
│   ├── opts.go             # PlaceOpt + WithBracket/WithLimit/.../SkipValidation
│   ├── transfer.go         # Trade.Transfer group
│   ├── withdraw.go         # Withdraw off-ramp to L1
│   ├── convert.go          # Trade.Convert (USDC <-> USDH)
│   ├── subaccount.go       # Trade.SubAccount group
│   ├── stake.go            # Trade.Stake group
│   ├── multisig.go         # Trade.MultiSig group
│   ├── account.go          # ApproveAgent, ApproveBuilderFee, SetReferrer, UseBigBlocks
│   ├── deploy_spot.go      # HIP-2 spot deploy
│   ├── deploy_perp.go      # HIP-3 perp deploy
│   ├── outcome.go          # HIP-4 split / merge / mergeQuestion / negate
│   ├── validators.go       # CSigner / CValidator pass-throughs
│   ├── validate.go         # single validate() pipeline
│   ├── bracket.go          # bracket-leg builder
│   └── wire.go             # price/size formatting
│
├── stream/                 # websocket surface
│   ├── stream.go           # stream.New + Client core
│   ├── subscriptions.go    # Trades/Book/BBO/Candles/.../UserFills/...
│   ├── ws_types.go         # WSMessage, Subscription, payload structs
│   ├── post.go             # PostInfo, PostAction, Post
│   ├── reconnect.go        # reconnect + automatic resubscribe state machine
│   └── logger.go           # Logger interface
│
└── internal/               # not part of the public API
    ├── eip712/             # EIP-712 hash, sign, phantom agent
    ├── wire/               # price/size rounding + wire-format helpers
    └── transport/          # HTTP client, request plumbing
```

### Importing the subpackages directly

Most users want only the facade:

```go
import hl "github.com/Simon-Busch/hyperliquid-go"
```

Callers who want to compose a single handle without spinning up an entire
`Client` can reach in directly:

```go
import (
    "github.com/Simon-Busch/hyperliquid-go/info"
    "github.com/Simon-Busch/hyperliquid-go/trade"
    "github.com/Simon-Busch/hyperliquid-go/stream"
    "github.com/Simon-Busch/hyperliquid-go/signing"
    "github.com/Simon-Busch/hyperliquid-go/types"
)
```

For example, a read-only client that never signs anything:

```go
i := info.New("https://api.hyperliquid-testnet.xyz", true, nil, nil, nil, "")
state, err := i.UserState("0xabc...")
```

## License

This repository does not currently ship a `LICENSE` file. Upstream [`sonirico/go-hyperliquid`](https://github.com/sonirico/go-hyperliquid) is the reference for licensing terms while one is added.
