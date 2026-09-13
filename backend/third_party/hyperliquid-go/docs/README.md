# go-hyperliquid-0xsi — Reference Documentation

Reference manual for the public surface of `github.com/Simon-Busch/hyperliquid-go`.

The SDK exposes one top-level constructor — `hyperliquid.New(...)` — which returns a `*Client` with three handles:

| Handle     | Responsibility                                           | Page                       |
|------------|----------------------------------------------------------|----------------------------|
| `c.Info`   | Read-only queries (REST GET, no signing).                | [info.md](./info.md)       |
| `c.Trade`  | Signed actions (REST POST, EIP-712-signed).              | [trading.md](./trading.md) |
| `c.Stream` | WebSocket streaming and POST-over-WS.                    | [stream.md](./stream.md)   |

Cross-cutting:

- [quickstart.md](./quickstart.md) — end-to-end first-trade walkthrough.
- [hip.md](./hip.md) — HIP-2 (spot deploy), HIP-3 (builder dex), HIP-4 (outcome markets) end-to-end.
- [signing.md](./signing.md) — the four exported `Sign*` helpers and when to use them directly.
- [errors.md](./errors.md) — `ValidationError` codes, `APIError`, sentinel errors.
- [integration-testing.md](./integration-testing.md) — env vars, scenarios, and how to run the network suite.

The full design spec lives at [docs/spec/api-cleanup.md](./spec/api-cleanup.md) — it is the contract the refactor was written against, not user-facing documentation.

## Subpackages

The SDK is split into focused subpackages. The root `hyperliquid` package is a thin facade — it re-exports `New`, `Client`, the option functions, and `ErrMissingPrivateKey`, and wires the three handles to subpackage clients. Each subpackage is importable on its own:

| Import path                                        | Contents                                                                                              |
|----------------------------------------------------|-------------------------------------------------------------------------------------------------------|
| `github.com/Simon-Busch/hyperliquid-go`            | Facade: `New`, `Client`, `With*` options, `ErrMissingPrivateKey`.                                     |
| `.../info`                                         | Read-only client (`info.New`, `info.Client`) plus every response type (`UserState`, `Fill`, `Meta`, …). |
| `.../trade`                                        | Signed-action client (`trade.New`, `trade.Client`), `PlaceOpt`, the `ALO/IOC/GTC/Market/Trigger` constructors, every `With*` placement option, every response type. |
| `.../stream`                                       | Websocket client (`stream.New`, `stream.Client`), every subscription constructor (`Trades`, `Book`, …), `WSMessage`, `Subscription`, `Logger`. |
| `.../signing`                                      | EIP-712 helpers (`SignL1Action`, `SignUserSignedAction`, `FloatToUsdInt`, `GetTimestampMs`), `SignatureResult`, all wire action structs. |
| `.../types`                                        | Shared domain types: `Side` (`Buy`/`Sell`), `MarginMode`, `TIF`, `OrderSpec`, `Result`, `BatchResult`, `CancelResult`, `BatchCancelResult`, `ValidationError`, `AssetClass`, `MixedArray`, … |

Most users only ever import the root package. The subpackage names appear in the rest of these docs (`types.Side`, `trade.WithBracket`, `stream.Trades`, …) so that the snippets stay valid after the transitional root-package compat aliases are removed.

## Index

### Client and configuration

- [`hyperliquid.New`](./trading.md#new)
- [`WithMainnet`, `WithTestnet`, `WithBaseURL`](./trading.md#client-options)
- [`WithPrivateKey`, `WithAccount`, `WithVault`](./trading.md#client-options)
- [`WithBuilderDex`, `WithMeta`, `WithSkipStream`](./trading.md#client-options)
- [`WithHTTPClient`, `WithLogger`](./trading.md#client-options)

### Trading — placement

- [`Trader.PlaceALO`](./trading.md#placealo)
- [`Trader.PlaceIOC`](./trading.md#placeioc)
- [`Trader.PlaceGTC`](./trading.md#placegtc)
- [`Trader.PlaceMarket`](./trading.md#placemarket)
- [`Trader.PlaceTrigger`](./trading.md#placetrigger)
- [`Trader.PlaceMany`](./trading.md#placemany)
- [`trade.ALO`, `trade.IOC`, `trade.GTC`, `trade.Market`, `trade.Trigger`](./trading.md#orderspec-constructors)

### Trading — placement options

- [`WithTakeProfit`, `WithStopLoss`, `WithBracket`](./trading.md#placement-options)
- [`WithReduceOnly`, `WithCloid`, `WithBuilder`](./trading.md#placement-options)
- [`WithSlippage`, `WithSize`, `WithLimit`](./trading.md#placement-options)
- [`AsMarket`, `AsLimit`](./trading.md#placement-options)
- [`WithTPSize`, `WithSLSize`, `WithTPCloid`, `WithSLCloid`](./trading.md#placement-options)
- [`SkipValidation`](./trading.md#placement-options)

### Trading — modify and cancel

- [`Trader.Modify`](./trading.md#modify)
- [`Trader.ModifyByCloid`](./trading.md#modifybycloid)
- [`Trader.Cancel`](./trading.md#cancel)
- [`Trader.CancelByCloid`](./trading.md#cancelbycloid)
- [`Trader.CancelAll`](./trading.md#cancelall)

### Trading — position management

- [`Trader.ClosePosition`](./trading.md#closeposition)
- [`Trader.SetLeverage`](./trading.md#setleverage)
- [`Trader.AdjustMargin`](./trading.md#adjustmargin)
- [`Trader.ScheduleCancelAll`](./trading.md#schedulecancelall)
- [`Trader.RefreshState`](./trading.md#refreshstate)

### Trading — transfers

- [`Trade.Transfer.SendUSD`](./trading.md#transfer-sendusd)
- [`Trade.Transfer.SendSpot`](./trading.md#transfer-sendspot)
- [`Trade.Transfer.DepositToVault`](./trading.md#transfer-deposittovault)
- [`Trade.Transfer.WithdrawFromVault`](./trading.md#transfer-withdrawfromvault)
- [`Trade.Transfer.PerpToSpot`](./trading.md#transfer-perptospot)
- [`Trade.Transfer.SpotToPerp`](./trading.md#transfer-spottoperp)
- [`Trade.Transfer.MoveToDex`](./trading.md#transfer-movetodex)
- [`Trade.Transfer.MoveFromDex`](./trading.md#transfer-movefromdex)
- [`Trader.Withdraw`](./trading.md#withdraw)

### Trading — sub-accounts, staking, multi-sig

- [`Trade.SubAccount.Create`](./trading.md#subaccount-create)
- [`Trade.SubAccount.DepositUSD`](./trading.md#subaccount-depositusd)
- [`Trade.SubAccount.WithdrawUSD`](./trading.md#subaccount-withdrawusd)
- [`Trade.SubAccount.DepositSpot`](./trading.md#subaccount-depositspot)
- [`Trade.SubAccount.WithdrawSpot`](./trading.md#subaccount-withdrawspot)
- [`Trade.Stake.Delegate`](./trading.md#stake-delegate)
- [`Trade.Stake.Undelegate`](./trading.md#stake-undelegate)
- [`Trade.MultiSig.Convert`](./trading.md#multisig-convert)
- [`Trade.MultiSig.Execute`](./trading.md#multisig-execute)

### Trading — account control

- [`Trader.ApproveAgent`](./trading.md#approveagent)
- [`Trader.ApproveBuilderFee`](./trading.md#approvebuilderfee)
- [`Trader.SetReferrer`](./trading.md#setreferrer)
- [`Trader.UseBigBlocks`](./trading.md#usebigblocks)

### Trading — HIP-2 / HIP-3 deploy

- [`Trader.SpotDeploy*`](./trading.md#spot-deploy)
- [`Trader.PerpDeploy*`](./trading.md#perp-deploy)

### Trading — validators

- [`Trader.CSignerJailSelf`, `CSignerUnjailSelf`, `CSignerInner`](./trading.md#validators)
- [`Trader.CValidatorRegister`, `CValidatorChangeProfile`, `CValidatorUnregister`](./trading.md#validators)

### Info — market data

- [`Info.Mid`](./info.md#mid)
- [`Info.AllMids`, `Info.AllMidsOn`](./info.md#allmids)
- [`Info.Book`](./info.md#book)
- [`Info.Candles`](./info.md#candles)
- [`Info.MetaAndAssetCtxs`, `Info.SpotMetaAndAssetCtxs`](./info.md#metaandassetctxs)

### Info — account state

- [`Info.UserState`](./info.md#userstate)
- [`Info.SpotBalances`](./info.md#spotbalances)
- [`Info.Positions`, `Info.Position`](./info.md#positions)
- [`Info.Fees`](./info.md#fees)
- [`Info.Asset`, `Info.AssetID`](./info.md#asset)

### Info — orders and fills

- [`Info.OpenOrders`, `Info.FrontendOpenOrders`](./info.md#openorders)
- [`Info.Fills`, `Info.FillsBetween`](./info.md#fills)
- [`Info.Order`, `Info.OrderByCloid`, `Info.Fill`](./info.md#order)

### Info — funding, staking, metadata

- [`Info.Funding`, `Info.UserFunding`](./info.md#funding)
- [`Info.Stake.Summary`, `Delegations`, `Rewards`](./info.md#staking)
- [`Info.Meta`, `Info.SpotMeta`, `Info.OutcomeMeta`, `Info.PerpDexs`](./info.md#metadata)
- [`Info.SubAccounts`, `Info.Referral`, `Info.MultiSigSigners`](./info.md#account-directory)

### Stream

- [`Stream.Connect`, `Stream.Close`](./stream.md#lifecycle)
- [`Stream.Subscribe`, `Subscription.Close`](./stream.md#subscribe)
- [`stream.Trades`, `stream.Book`, `stream.BBO`, `stream.Candles`](./stream.md#market-subscriptions)
- [`stream.AllMids`, `stream.AllMidsOn`, `stream.ActiveAssetCtx`, `stream.ActiveAssetData`](./stream.md#market-subscriptions)
- [`stream.UserEvents`, `stream.UserFills`, `stream.OrderUpdates`](./stream.md#user-subscriptions)
- [`stream.UserFundings`, `stream.UserLedger`, `stream.WebData`, `stream.Notifications`](./stream.md#user-subscriptions)
- [`stream.UserTwapFills`, `stream.UserTwapHistory`](./stream.md#user-subscriptions)
- [`Stream.PostInfo`, `Stream.PostAction`, `Stream.Post`](./stream.md#post-over-ws)

### Signing

- [`SignL1Action`](./signing.md#signl1action)
- [`SignUserSignedAction`](./signing.md#signusersignedaction)
- [`FloatToUsdInt`](./signing.md#floattoUsdint)
- [`GetTimestampMs`](./signing.md#gettimestampms)

### Errors

- [`ValidationError`](./errors.md#validationerror)
- [`APIError`](./errors.md#apierror)
- [`ErrMissingPrivateKey`](./errors.md#errmissingprivatekey)
