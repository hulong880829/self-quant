# Integration testing

The default `go test ./...` runs unit tests only — no network. The integration suite lives under `tests/integration/` and is gated behind a build tag so accidental `go test` runs never hit the exchange.

## Running the suite

```bash
go test -tags=integration ./tests/integration/...
```

To run a single scenario:

```bash
go test -tags=integration -run TestPlaceALORoundTrip ./tests/integration/...
```

Add `-v` for verbose output and `-count=1` to disable the test cache.

## Environment variables

The suite loads a `.env` from the project root via [godotenv](https://github.com/joho/godotenv) before each test.

| Variable              | Required | Default                                           | Purpose                                              |
|-----------------------|----------|---------------------------------------------------|------------------------------------------------------|
| `HL_PRIVATE_KEY`      | yes      | —                                                 | Hex-encoded private key for the test wallet.         |
| `HL_ACCOUNT_ADDRESS`  | no       | derived from `HL_PRIVATE_KEY`                     | Owner address (set when using an agent flow).        |
| `HL_BASE_URL`         | no       | `https://api.hyperliquid-testnet.xyz`             | Endpoint to target. **Always default to testnet.**   |
| `HL_TEST_COIN`        | no       | `ETH`                                             | Coin used by placement scenarios.                    |
| `HL_TEST_SIZE`        | no       | `0.01`                                            | Size used by placement scenarios.                    |
| `HL_BUILDER_ADDR`     | no       | —                                                 | Builder address for `WithBuilder` scenarios.         |
| `HL_BUILDER_FEE_BPS`  | no       | —                                                 | Fee bps for `WithBuilder` scenarios.                 |
| `HL_SKIP_TRANSFER`    | no       | —                                                 | Set `true` to skip transfer / withdraw scenarios.    |
| `HL_SKIP_WS`          | no       | —                                                 | Set `true` to skip websocket scenarios.              |
| `HL_HIP3_DEX`         | no       | —                                                 | HIP-3 builder-deployed perp dex (e.g. `flx`). Unset = HIP-3 suite skips. |
| `HL_HIP3_COIN`        | no       | first asset in dex universe                       | Coin on the HIP-3 dex for placement scenarios.       |
| `HL_HIP4_OUTCOME`     | no       | first outcome with a live mid                     | Friendly (`<question>:<side>`) or canonical (`#<enc>`) outcome name. Empty environment = HIP-4 suite skips. |

Recommended `.env`:

```bash
HL_BASE_URL=https://api.hyperliquid-testnet.xyz
HL_PRIVATE_KEY=0x<your-test-wallet-key>
HL_ACCOUNT_ADDRESS=0x<your-owner-address>
HL_TEST_COIN=ETH
HL_TEST_SIZE=0.01
```

Never commit `.env`. The repo `.gitignore` already excludes it.

## Scenarios

The integration files in `tests/integration/` cover, at minimum, these scenarios. Each is one test function:

1. **Place ALO round-trip** — place an ALO well off the book, list open orders, cancel.
2. **Place IOC market fill** — place an IOC that fills, query fills.
3. **Bracketed GTC** — place a GTC with `WithBracket(tp, sl)`, cancel the parent, verify TP/SL are also cancelled.
4. **Trigger stop-market** — place a stop-market via `PlaceTrigger`, cancel.
5. **SetLeverage round-trip** — switch leverage Cross ↔ Isolated, verify the new state.
6. **USD transfer to self** — call `Trade.Transfer.SendUSD` to the same wallet, verify the ledger entry.
7. **Approve agent + place from agent** — call `ApproveAgent`, build a new client with the agent key + `WithAccount(owner)`, place an order, cancel it.
8. **Stream trades, 5 s** — `Subscribe(stream.Trades(coin), ...)`, count messages over 5 s, assert > 0.
9. **Stream PostInfo** — call `PostInfo`, compare the payload with the REST `Info.Book` snapshot.
10. **Stream PostAction** — place an order and cancel it entirely over the WS.
11. **ClosePosition end-to-end** — open a tiny position, call `ClosePosition`, verify no position remains.
12. **PlaceMany batch** — two far-from-mid ALOs submitted in one signed action.
13. **Cloid round-trip** — `WithCloid` → `Info.OrderByCloid` → `CancelByCloid`.
14. **ModifyByCloid** — resize a resting order identified by cloid.
15. **AdjustMargin (isolated)** — open isolated position, top up margin, confirm via `UserState`.
16. **SubAccount create + list** — `Trade.SubAccount.Create`, confirm via `Info.SubAccounts`.
17. **Withdraw wire-shape** — `Trade.Withdraw(0.01, self)`; accept success OR minimum-threshold rejection.
18. **Stream reconnect** — close session, fresh client, re-subscribe to two filters.
19. **Idempotent cancel** — cancel a dead order; second cancel returns a typed error.
20. **OrderByCloid not found** — query a never-placed cloid; assert no panic and no live order.

### HIP-3 — builder-deployed perpetuals

Each test skips wholesale when `HL_HIP3_DEX` is unset.

21. **HIP-3 meta + PerpDexs** — `Info.PerpDexs`, `Info.Meta(dex)`.
22. **HIP-3 PlaceALO** — place + cancel an ALO on the builder dex.
23. **HIP-3 MoveTo/From Dex** — round-trip 1 USDC between default wallet and dex.
24. **HIP-3 AllMidsOn** — subscribe to the dex-pinned all-mids stream.

### HIP-4 — binary outcome markets

Each test skips wholesale when `Info.OutcomeMeta` is empty or fails.

25. **HIP-4 outcome meta** — log shape of the first outcome.
26. **HIP-4 asset lookup** — canonical and friendly names resolve to the same id (≥ 100,000,000).
27. **HIP-4 Book + Mid** — live outcome, mid in (0, 1].
28. **HIP-4 place + cancel** — integer-size ALO on an outcome at half the mid.
29. **HIP-4 fractional size rejected** — SDK validator catches size=0.5 against MinSize=1.
30. **HIP-4 trades subscription** — trades feed via the canonical `#<enc>` name.

## Troubleshooting

- **`429 Too Many Requests`** — the test suite does not throttle. Re-run with `-parallel 1` or insert your own delays in custom scenarios.
- **`insufficient balance`** — top the test wallet up on testnet via the [Hyperliquid testnet faucet](https://app.hyperliquid-testnet.xyz/).
- **`validation: refresh user state: ...`** — the SDK could not fetch `info.UserState` for your address. Confirm `HL_ACCOUNT_ADDRESS` matches the wallet that traded last, or pass `trade.SkipValidation()` if you're intentionally trading from a fresh address.
- **`signature mismatch`** — the chain you're targeting (`HL_BASE_URL`) does not match what `WithMainnet()` / `WithTestnet()` selected. If you set `WithBaseURL` directly, `isMainnet` is inferred from the URL — keep it consistent.
- **Test occasionally cancels itself with "context deadline exceeded"** — the Stream's initial dial uses the test's `context.Context`. Widen the timeout or split the test into smaller per-call contexts.

## Writing new scenarios

The shared helper `loadIntegrationEnv(t)` returns a typed config struct (`coin`, `size`, `base URL`, derived addresses). Build scenarios as ordinary `*testing.T` functions tagged with `//go:build integration`. Avoid hardcoding mainnet URLs.

A minimal scenario template:

```go
//go:build integration

package integration

import (
    "context"
    "testing"
    "time"

    hl "github.com/Simon-Busch/hyperliquid-go"
)

func TestMyScenario(t *testing.T) {
    env := loadIntegrationEnv(t)

    c, err := hl.New(
        hl.WithBaseURL(env.BaseURL),
        hl.WithPrivateKey(env.PrivateKey),
        hl.WithAccount(env.Account),
    )
    if err != nil { t.Fatal(err) }

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    if err := c.Stream.Connect(ctx); err != nil { t.Fatal(err) }
    defer c.Stream.Close()

    // ... scenario body ...
}
```
