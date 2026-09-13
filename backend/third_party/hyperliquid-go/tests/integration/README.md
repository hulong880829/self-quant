# Integration tests

Network-dependent scenarios for the Hyperliquid Go SDK. Every test in
this directory is gated behind the `integration` build tag so the
default `go test ./...` run never reaches the network.

## Running

```sh
go test -tags=integration -count=1 ./tests/integration/...
```

To list scenarios without running them:

```sh
go test -tags=integration -list "Test.*" ./tests/integration/...
```

The same suite runs against testnet (default) and mainnet — point
`HL_BASE_URL` at the environment you want:

```sh
# testnet (default)
HL_BASE_URL=https://api.hyperliquid-testnet.xyz

# mainnet
HL_BASE_URL=https://api.hyperliquid.xyz
```

Scenarios use the small order size from `HL_TEST_SIZE` (default `0.01`)
so mainnet runs stay cheap. Read-only scenarios cost nothing.

## Configuration

Configuration is read from a `.env` file. The loader resolves `.env`,
then `../.env`, then `../../.env` (so the file can live next to the
test directory, the repo root, or one level above).

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `HL_PRIVATE_KEY` | yes | — | 32-byte hex (with or without `0x`); signs every action. |
| `HL_ACCOUNT_ADDRESS` | no | derived from key | Account address Trader acts on behalf of (agent flow). |
| `HL_BASE_URL` | no | `https://api.hyperliquid-testnet.xyz` | REST endpoint. |
| `HL_TEST_COIN` | no | `ETH` | Coin used for placement scenarios. |
| `HL_TEST_NOTIONAL` | no | `10` | Target USD notional per test order. Size is derived from the current mid and snapped down to the coin's size step. Set to `0` to fall back to `HL_TEST_SIZE`. |
| `HL_TEST_SIZE` | no | `0.01` | Fixed coin-unit fallback. Used only when `HL_TEST_NOTIONAL=0`. |
| `HL_BUILDER_ADDR` | no | — | Builder fee referral target. |
| `HL_BUILDER_FEE_BPS` | no | `1` | Builder fee in basis points. |
| `HL_SKIP_TRANSFER` | no | — | Set `true` to skip transfer scenarios. |
| `HL_SKIP_WS` | no | — | Set `true` to skip websocket scenarios. |
| `HL_HIP3_DEX` | no | — | Name of the HIP-3 builder-deployed perp dex to target (e.g. `flx`). When unset, the entire HIP-3 suite skips cleanly. |
| `HL_HIP3_COIN` | no | first asset on the dex | Coin on the HIP-3 dex used by placement scenarios. Skips if no usable coin found. |
| `HL_HIP4_OUTCOME` | no | first outcome with a live mid | Friendly (`<question>:<side>`) or canonical (`#<enc>`) name of the HIP-4 outcome to target. The entire HIP-4 suite skips when no outcomes are registered on the target environment. |

Tests skip when their preconditions are unmet (account empty, coin
missing from metadata, WS disabled, etc.) — they do not fail on
configuration gaps.

## Scenarios

| Scenario | What it does |
|---|---|
| `TestPlaceALO_QueryAndCancel` | Place a resting ALO, find it in OpenOrders, cancel. |
| `TestCancelAll` | Place two resting ALOs, call CancelAll, assert empty. |
| `TestPlaceIOC_Market` | IOC aggressively above mid, query the fill. |
| `TestPlaceMarket` | Market buy, check Position, then close. |
| `TestPlaceGTC_WithBracket` | GTC entry plus TP/SL bracket, cancel parent. |
| `TestPlaceTrigger_Cancel` | Far-away stop-market, cancel by oid. |
| `TestModify_PriceAndSize` | Resting ALO → Modify price + size, verify open order. |
| `TestClosePosition_AutoDirection` | Open long via market, close via auto-direction. |
| `TestSetLeverage` | Cross/5x then Isolated/3x. |
| `TestTransfer_USDToSelf` | Send 1.0 USDC to self, balance unchanged. |
| `TestApproveAgent_AndPlace` | Approve a fresh agent, place + cancel through it. |
| `TestStream_TradesReceived` | Subscribe to trades feed, expect ≥1 message. |
| `TestStream_PostInfo_MatchesREST` | PostInfo Meta over WS, compare to REST. |
| `TestStream_PostAction` | Place an ALO via Stream.PostAction, REST cancel. |
| `TestValidation_LongShortHardErrors` | Open a long, expect Buy reduce-only to fail. |
| `TestPlaceMany_Batch` | Two far-from-mid ALOs placed in one signed batch, both rest. |
| `TestPlace_WithCloid_Roundtrip` | Place with `WithCloid`, look up by cloid, cancel by cloid. |
| `TestModifyByCloid` | Resize a resting order via `ModifyByCloid`. |
| `TestAdjustMargin_IsolatedMode` | Open isolated position, top up margin via `AdjustMargin`. |
| `TestSubAccount_CreateDepositList` | Create a sub-account, confirm it surfaces in `Info.SubAccounts`. |
| `TestWithdraw_WireOnly` | Submit a tiny `Trade.Withdraw`; assert success OR a minimum-threshold rejection. |
| `TestStream_Reconnect` | Close a streaming session, build a fresh client, resubscribe. |
| `TestCancel_IdempotentOnDeadOrder` | Cancel twice; second cancel returns a clean typed error. |
| `TestInfo_OrderByCloid_NotFound` | Lookup a never-placed cloid; no panic, no live order returned. |
| `TestHIP3_MetaAndPerpDexs` | Read `Info.PerpDexs` and `Info.Meta(dex)` for the configured HIP-3 dex. |
| `TestHIP3_PlaceALO` | Place + cancel a far-from-mid ALO on the HIP-3 dex. |
| `TestHIP3_MoveToFromDex` | Round-trip 1 USDC between the default wallet and the HIP-3 dex. |
| `TestHIP3_AllMidsOn` | Subscribe to the dex-pinned `AllMidsOn` stream. |
| `TestHIP4_OutcomeMeta` | Read `Info.OutcomeMeta`; log shape of the first outcome. |
| `TestHIP4_AssetLookupBothNames` | Canonical and friendly outcome names resolve to the same asset id. |
| `TestHIP4_BookAndMid` | Book + Mid query for a live outcome; mid sits in (0, 1]. |
| `TestHIP4_PlaceCancelInteger` | Place + cancel an integer-size ALO on a HIP-4 outcome. |
| `TestHIP4_FractionalSizeRejected` | SDK validator rejects a 0.5 size on an integer-quantised outcome. |
| `TestHIP4_TradesSubscription` | Subscribe to the trades feed of an outcome via the canonical name. |

The HIP-3 and HIP-4 sub-suites skip wholesale when the relevant env var
(`HL_HIP3_DEX`) is empty or the target environment exposes no live
outcomes. None of the new scenarios assume a particular feature is
present.
