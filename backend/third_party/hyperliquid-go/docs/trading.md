# Trading reference (`c.Trade`)

`c.Trade` is the signed-action surface. Every method on `*trade.Client` (and on its sub-groups `Transfer`, `SubAccount`, `Stake`, `MultiSig`) builds an action map, EIP-712-signs it with the private key supplied to `hyperliquid.New`, and POSTs it to `/exchange`.

Placement methods run a single shared `validate()` pipeline before signing. Failures surface as `*types.ValidationError`; server-side rejects surface as `transport.APIError` wrapped in the returned `error`. See [errors.md](./errors.md).

`c.Trade` is `nil` if the `Client` was built without `WithPrivateKey`. The sentinel [`hyperliquid.ErrMissingPrivateKey`](./errors.md#errmissingprivatekey) describes that case.

The handle's concrete type is `*trade.Client` from the [`trade`](https://pkg.go.dev/github.com/Simon-Busch/hyperliquid-go/trade) subpackage. `PlaceOpt`, the placement option functions (`WithBracket`, `WithLimit`, …), the `OrderSpec` constructors (`ALO`, `IOC`, `GTC`, `Market`, `Trigger`), and every response struct (`TransferResponse`, `ApprovalResponse`, …) all live in `trade`. Domain primitives (`types.Side`, `types.OrderSpec`, `types.Result`, …) live in `types`.

## Contents

- [Construction](#new) and [client options](#client-options)
- [Placement](#placement) and [OrderSpec constructors](#orderspec-constructors)
- [Placement options](#placement-options)
- [Modify and cancel](#modify-and-cancel)
- [Position management](#position-management)
- [Transfers](#transfers) and [`Withdraw`](#withdraw)
- [Sub-accounts](#sub-accounts), [Staking](#staking), [Multi-sig](#multi-sig)
- [Account control](#account-control)
- [HIP-2 / HIP-3 deploy](#deploy)
- [Validator operations](#validators)

---

## New

### `hyperliquid.New` {#new}

Builds a `*Client` with configured `Info`, `Trade`, and `Stream` handles.

```go
func New(opts ...Option) (*Client, error)
```

**Example**

```go
c, err := hl.New(
    hl.WithTestnet(),
    hl.WithPrivateKey(pk),
    hl.WithAccount("0xabc..."),
)
```

If no `WithMainnet` / `WithTestnet` / `WithBaseURL` is supplied, mainnet is used. `c.Trade` is populated only if `WithPrivateKey` was set; `c.Stream` is populated unless `WithSkipStream(true)` was supplied.

## Client options {#client-options}

All options have the type `hyperliquid.Option = func(*clientConfig)`.

| Option                      | Effect                                                                 |
|-----------------------------|------------------------------------------------------------------------|
| `WithMainnet()`             | Pin the client to `https://api.hyperliquid.xyz` (default).             |
| `WithTestnet()`             | Pin the client to `https://api.hyperliquid-testnet.xyz`.               |
| `WithBaseURL(url)`          | Override the base URL (for local proxies or alternate endpoints).      |
| `WithPrivateKey(pk)`        | Required for `c.Trade`. Accepts an `*ecdsa.PrivateKey`.                |
| `WithAccount(addr)`         | Owner-address override for agent flows. Defaults to the pk's address.  |
| `WithVault(addr)`           | Sign actions on behalf of a vault address.                             |
| `WithBuilderDex(dex)`       | Pin the client to a HIP-3 builder-deployed dex (e.g. `"flx"`).         |
| `WithHTTPClient(c)`         | Inject a custom `*http.Client` (timeouts, proxies, trace transport).   |
| `WithMeta(meta, spotMeta, perpDexs)` | Use pre-fetched metadata; skips the warm-up calls.            |
| `WithSkipStream(true)`      | Do not construct `c.Stream`. Use for REST-only programs.               |
| `WithLogger(l)`             | Plug a `Logger` into `c.Stream` for reconnect diagnostics.             |

---

## Placement

Required parameters are positional; optional parameters flow through the `PlaceOpt` variadic. All placement methods share a single validate-then-sign pipeline and return a flat `Result`.

### `PlaceALO` {#placealo}

Place an Add-Liquidity-Only limit order. ALO orders that would cross are rejected by the exchange.

```go
func (t *trade.Client) PlaceALO(coin string, side types.Side, size, px float64, opts ...trade.PlaceOpt) (types.Result, error)
```

**Validation**: `coin_required`, `unknown_coin`, `size_below_min`, `size_step_violation`, `price_non_positive`, `significant_figures`, `wrong_side_for_reduce`, plus bracket rules when `WithBracket`/`WithTakeProfit`/`WithStopLoss` are used.

**Example**

```go
res, err := c.Trade.PlaceALO("ETH", types.Buy, 0.01, 1500, trade.WithCloid("0x..."))
```

**Related**: [`trade.ALO`](#orderspec-constructors), [`PlaceGTC`](#placegtc), [placement options](#placement-options).

### `PlaceIOC` {#placeioc}

Place an Immediate-Or-Cancel limit order. Anything that does not fill at submission is cancelled.

```go
func (t *trade.Client) PlaceIOC(coin string, side types.Side, size, px float64, opts ...trade.PlaceOpt) (types.Result, error)
```

**Validation**: same as `PlaceALO`.

**Example**

```go
res, err := c.Trade.PlaceIOC("BTC", types.Sell, 0.001, 70000)
```

### `PlaceGTC` {#placegtc}

Place a Good-Til-Cancelled limit order.

```go
func (t *trade.Client) PlaceGTC(coin string, side types.Side, size, px float64, opts ...trade.PlaceOpt) (types.Result, error)
```

**Validation**: same as `PlaceALO`. Additional bracket rules when `WithBracket`/`WithTakeProfit`/`WithStopLoss` are used: `tp_wrong_side_buy`, `tp_wrong_side_sell`, `sl_wrong_side_buy`, `sl_wrong_side_sell`, `bracket_size_exceeds_entry`.

**Example — bracketed entry**

```go
res, err := c.Trade.PlaceGTC(
    "ETH", types.Buy, 0.01, 1500,
    trade.WithBracket(1600, 1450),
)
```

### `PlaceMarket` {#placemarket}

Submit a market-style order. Internally an IOC at the current mid plus or minus `slippage` (default 5%).

```go
func (t *trade.Client) PlaceMarket(coin string, side types.Side, size float64, opts ...trade.PlaceOpt) (types.Result, error)
```

**Validation**: `coin_required`, `unknown_coin`, `size_below_min`, `size_step_violation`, plus `unsupported_option` if `WithSlippage` is passed to any other method.

**Example**

```go
res, err := c.Trade.PlaceMarket("ETH", types.Buy, 0.01, trade.WithSlippage(0.02))
```

### `PlaceTrigger` {#placetrigger}

Place a stop-market (default) or stop-limit trigger order. Use [`AsMarket`](#placement-options) / [`AsLimit`](#placement-options) to switch.

```go
func (t *trade.Client) PlaceTrigger(coin string, side Side, size, triggerPx float64, opts ...PlaceOpt) (Result, error)
```

The TP/SL discriminator is inferred from `side`: `Buy → "sl"`, `Sell → "tp"`. The trigger price is stored both as `TriggerPx` and as the parent's `Price` so the wire serialization stays consistent.

**Example — stop-loss at 1450**

```go
res, err := c.Trade.PlaceTrigger("ETH", types.Sell, 0.01, 1450, trade.WithReduceOnly())
```

**Example — stop-limit (rest at limit after trigger)**

```go
res, err := c.Trade.PlaceTrigger("ETH", types.Sell, 0.01, 1450, trade.AsLimit(1448), trade.WithReduceOnly())
```

### `PlaceMany` {#placemany}

Place multiple legs with one signature.

```go
func (t *trade.Client) PlaceMany(orders ...types.OrderSpec) (types.BatchResult, error)
```

Each spec is validated individually before any signing happens. The result contains one `Result` per leg, in the same order as the inputs.

**Example**

```go
res, err := c.Trade.PlaceMany(
    trade.GTC("ETH", types.Buy,  0.01, 1500),
    trade.IOC("BTC", types.Sell, 0.001, 70_000),
)
```

## OrderSpec constructors {#orderspec-constructors}

Top-level (package-level) helpers that return an `OrderSpec` for `PlaceMany`. Same option set as the corresponding `Trader.Place*` methods.

```go
func trade.ALO(coin string, side types.Side, size, px float64, opts ...trade.PlaceOpt) types.OrderSpec
func trade.IOC(coin string, side types.Side, size, px float64, opts ...trade.PlaceOpt) types.OrderSpec
func trade.GTC(coin string, side types.Side, size, px float64, opts ...trade.PlaceOpt) types.OrderSpec
func trade.Market(coin string, side types.Side, size float64, opts ...trade.PlaceOpt) types.OrderSpec
func trade.Trigger(coin string, side types.Side, size, triggerPx float64, opts ...trade.PlaceOpt) types.OrderSpec
```

---

## Placement options {#placement-options}

All options have the type `trade.PlaceOpt = func(*types.OrderSpec)`. They never report errors directly; misuse surfaces as a `*types.ValidationError` with `Code == "unsupported_option"` at `place()` time.

| Option                | Effect                                                      | Valid on                        |
|-----------------------|-------------------------------------------------------------|---------------------------------|
| `WithTakeProfit(px)`  | Attach reduce-only TP trigger leg.                          | `PlaceALO/IOC/GTC/Market`, `PlaceMany`. |
| `WithStopLoss(px)`    | Attach reduce-only SL trigger leg.                          | same as above.                  |
| `WithBracket(tp, sl)` | Shortcut for `WithTakeProfit + WithStopLoss`.               | same as above.                  |
| `WithTPSize(sz)`      | Partial TP leg size.                                        | same as above.                  |
| `WithSLSize(sz)`      | Partial SL leg size.                                        | same as above.                  |
| `WithTPCloid(s)`      | Pin a cloid on the TP leg.                                  | same as above.                  |
| `WithSLCloid(s)`      | Pin a cloid on the SL leg.                                  | same as above.                  |
| `WithReduceOnly()`    | Mark the order reduce-only.                                 | all placement verbs.            |
| `WithCloid(s)`        | Pin a client-supplied 32-byte hex order id.                 | all placement verbs.            |
| `WithBuilder(addr, bps)` | Attach a HIP-1 builder-fee referral.                     | all placement verbs.            |
| `WithSlippage(frac)`  | Worst-case fill fraction off mid for market orders.         | `PlaceMarket`, `ClosePosition`. |
| `WithSize(sz)`        | Override the order size (partial-close, modify resize).     | `ClosePosition`, `Modify`.      |
| `WithLimit(px)`       | Limit-style close or new modify price.                      | `ClosePosition`, `Modify`.      |
| `AsMarket()`          | Force trigger to fill as a market.                          | `PlaceTrigger`.                 |
| `AsLimit(px)`         | Force trigger to rest as a limit at px.                     | `PlaceTrigger`.                 |
| `SkipValidation()`    | Bypass the pre-flight `validate()`.                         | any placement verb.             |

`SkipValidation()` is an escape hatch — use only when calling against a network where the SDK cannot fetch metadata, or where the caller has its own validation.

---

## Modify and cancel

### `Modify` {#modify}

Change the price (or size, or both) of a resting order identified by `oid`.

```go
func (t *trade.Client) Modify(oid int64, opts ...trade.PlaceOpt) (types.Result, error)
```

Either `WithLimit(newPx)` or `WithSize(newSz)` (or both) must be supplied. Otherwise the validator returns `Code == "modify_no_change"`.

**Validation**: `modify_target_required` (if neither oid nor cloid resolves), `modify_no_change`.

**Example**

```go
res, err := c.Trade.Modify(oid, trade.WithLimit(1502.5))
```

### `ModifyByCloid` {#modifybycloid}

Identical to `Modify` but addresses the order by its `Cloid`.

```go
func (t *trade.Client) ModifyByCloid(cloid string, opts ...trade.PlaceOpt) (types.Result, error)
```

### `Cancel` {#cancel}

Cancel a single open order by `oid`.

```go
func (t *trade.Client) Cancel(coin string, oid int64) (types.CancelResult, error)
```

### `CancelByCloid` {#cancelbycloid}

Cancel a single open order by its client order id.

```go
func (t *trade.Client) CancelByCloid(coin, cloid string) (types.CancelResult, error)
```

### `CancelAll` {#cancelall}

Cancel every open order across the supplied coins. With no coins supplied it cancels everything across every asset.

```go
func (t *trade.Client) CancelAll(coins ...string) (types.BatchCancelResult, error)
```

**Example**

```go
_, err := c.Trade.CancelAll()        // cancel everything
_, err  = c.Trade.CancelAll("ETH")   // ETH only
```

---

## Position management

### `ClosePosition` {#closeposition}

Flatten the caller's open position on `coin`. Direction is inferred from the cached `UserState`; long positions exit with a sell, shorts with a buy. By default the close is an IOC at mid plus or minus `slippage` (default 5%). Pass `WithLimit(px)` for a limit close or `WithSize(x)` for a partial close.

```go
func (t *trade.Client) ClosePosition(coin string, opts ...trade.PlaceOpt) (types.Result, error)
```

**Validation**: `no_position` (no open position on `coin`), `close_size_exceeds_position` (when `WithSize` exceeds the absolute position size).

**Example**

```go
res, err := c.Trade.ClosePosition("ETH")
```

### `SetLeverage` {#setleverage}

Update the leverage on `coin`. `mode` picks `Cross` (shared collateral) or `Isolated` (per-position).

```go
func (t *trade.Client) SetLeverage(coin string, leverage int, mode types.MarginMode) (*info.UserState, error)
```

**Example**

```go
state, err := c.Trade.SetLeverage("ETH", 5, types.Cross)
```

### `AdjustMargin` {#adjustmargin}

Add or remove isolated-margin collateral on the position in `coin`. Positive amount adds; negative withdraws. `amount` is in decimal USDC.

```go
// Return type is the generic *transport.APIResponse[trade.DefaultResponse]
// envelope (transport is internal); the *DefaultResponse inside reports
// success/failure status verbatim.
func (t *trade.Client) AdjustMargin(coin string, amount float64) (*trade.DefaultResponse, error)
```

(The actual signature returns a transport envelope around `trade.DefaultResponse`; treat the envelope as opaque and inspect the inner response.)

### `ScheduleCancelAll` {#schedulecancelall}

Schedule cancellation of all open orders at a deadline. `nil` clears any scheduled cancel.

```go
func (t *trade.Client) ScheduleCancelAll(deadline *time.Time) (*trade.ScheduleCancelResponse, error)
```

### `RefreshState` {#refreshstate}

Refresh the cached `UserState` snapshot used by position-aware validation. The placement pipeline refreshes implicitly on every call unless `SkipValidation()` is in effect; calling `RefreshState` directly is useful if you want a recent state for your own logic.

```go
func (t *trade.Client) RefreshState(ctx context.Context) error
```

**Related**: [`ValidationError` codes](./errors.md#validationerror).

---

## Transfers {#transfers}

All transfer actions are reachable via `c.Trade.Transfer`. They return `*TransferResponse`.

### `Transfer.SendUSD` {#transfer-sendusd}

Send USDC to another address.

```go
func (g *trade.TransferGroup) SendUSD(toAddr string, amount float64) (*trade.TransferResponse, error)
```

### `Transfer.SendSpot` {#transfer-sendspot}

Send a spot token to another address.

```go
func (g *trade.TransferGroup) SendSpot(toAddr, token string, amount float64) (*trade.TransferResponse, error)
```

### `Transfer.DepositToVault` {#transfer-deposittovault}

Deposit USDC into a vault.

```go
func (g *trade.TransferGroup) DepositToVault(vaultAddr string, amount float64) (*trade.TransferResponse, error)
```

### `Transfer.WithdrawFromVault` {#transfer-withdrawfromvault}

Withdraw USDC from a vault.

```go
func (g *trade.TransferGroup) WithdrawFromVault(vaultAddr string, amount float64) (*trade.TransferResponse, error)
```

### `Transfer.PerpToSpot` {#transfer-perptospot}

Move USDC from the perps wallet to the spot wallet.

```go
func (g *trade.TransferGroup) PerpToSpot(amount float64) (*trade.TransferResponse, error)
```

### `Transfer.SpotToPerp` {#transfer-spottoperp}

Move USDC from the spot wallet to the perps wallet.

```go
func (g *trade.TransferGroup) SpotToPerp(amount float64) (*trade.TransferResponse, error)
```

### `Transfer.MoveToDex` {#transfer-movetodex}

Move balance from the default perp dex into a HIP-3 builder-deployed dex.

```go
func (g *trade.TransferGroup) MoveToDex(dex, token string, amount float64) (*trade.TransferResponse, error)
```

### `Transfer.MoveFromDex` {#transfer-movefromdex}

Move balance back from a HIP-3 builder-deployed dex to the default perp dex.

```go
func (g *trade.TransferGroup) MoveFromDex(dex, token string, amount float64) (*trade.TransferResponse, error)
```

---

## Withdraw {#withdraw}

### `Withdraw`

Withdraw USDC off the Hyperliquid L1 bridge to an external destination.

```go
func (t *trade.Client) Withdraw(amount float64, destination string) (*trade.TransferResponse, error)
```


**Example**

```go
_, err := c.Trade.Withdraw(100.0, "0xabc...")
```

---

## Sub-accounts {#sub-accounts}

Accessible via `c.Trade.SubAccount`.

### `SubAccount.Create` {#subaccount-create}

```go
func (g *trade.SubAccountGroup) Create(name string) (*trade.CreateSubAccountResponse, error)
```

### `SubAccount.DepositUSD` {#subaccount-depositusd}

```go
func (g *trade.SubAccountGroup) DepositUSD(subAddr string, amount float64) (*trade.TransferResponse, error)
```

### `SubAccount.WithdrawUSD` {#subaccount-withdrawusd}

```go
func (g *trade.SubAccountGroup) WithdrawUSD(subAddr string, amount float64) (*trade.TransferResponse, error)
```

### `SubAccount.DepositSpot` {#subaccount-depositspot}

```go
func (g *trade.SubAccountGroup) DepositSpot(subAddr, token string, amount float64) (*trade.TransferResponse, error)
```

### `SubAccount.WithdrawSpot` {#subaccount-withdrawspot}

```go
func (g *trade.SubAccountGroup) WithdrawSpot(subAddr, token string, amount float64) (*trade.TransferResponse, error)
```

---

## Staking {#staking}

Accessible via `c.Trade.Stake`. The `wei` argument is the staked amount in HYPE wei.

### `Stake.Delegate` {#stake-delegate}

```go
func (g *trade.StakeGroup) Delegate(validator string, wei int) (*trade.TransferResponse, error)
```

### `Stake.Undelegate` {#stake-undelegate}

```go
func (g *trade.StakeGroup) Undelegate(validator string, wei int) (*trade.TransferResponse, error)
```

---

## Multi-sig {#multi-sig}

Accessible via `c.Trade.MultiSig`.

### `MultiSig.Convert` {#multisig-convert}

Convert the caller's account to a multi-sig wallet with the given authorized signers and approval threshold.

```go
func (g *trade.MultiSigGroup) Convert(authorized []string, threshold int) (*trade.MultiSigConversionResponse, error)
```

### `MultiSig.Execute` {#multisig-execute}

Execute a previously assembled action with a set of signatures.

```go
func (g *trade.MultiSigGroup) Execute(action map[string]any, signers []string, signatures []string) (*trade.MultiSigResponse, error)
```

---

## Account control

### `ApproveAgent` {#approveagent}

Provision a fresh agent key authorized for trading on behalf of the caller's account. The returned `Agent` carries the agent address and freshly generated private key — keep the key secret.

```go
func (t *trade.Client) ApproveAgent(name string) (trade.Agent, error)
```

**Example**

```go
agent, err := c.Trade.ApproveAgent("my-bot")
// Use agent.PrivateKey + WithAccount(myMainAddress) on subsequent New(...) calls.
```

### `ApproveBuilderFee` {#approvebuilderfee}

Approve a HIP-1 builder to charge a max fee rate (string-encoded, e.g. `"0.001%"`) on the caller's orders.

```go
func (t *trade.Client) ApproveBuilderFee(builder string, maxFeeRate string) (*trade.ApprovalResponse, error)
```

### `SetReferrer` {#setreferrer}

Set the caller's referrer code (once per account, irreversibly).

```go
func (t *trade.Client) SetReferrer(code string) (*trade.SetReferrerResponse, error)
```

### `UseBigBlocks` {#usebigblocks}

Opt the caller's address into "big block" inclusion.

```go
func (t *trade.Client) UseBigBlocks(enable bool) (*trade.ApprovalResponse, error)
```

---

## HIP-2 / HIP-3 deploy {#deploy}

Rare expert operations. All methods return `*SpotDeployResponse` (spot) or `*PerpDeployResponse` (perp).

### Spot deploy {#spot-deploy}

```go
func (t *trade.Client) SpotDeployRegisterToken(tokenName string, szDecimals, weiDecimals, maxGas int, fullName string) (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployUserGenesis(balances map[string]float64) (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployEnableFreezePrivilege() (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployFreezeUser(userAddress string) (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployRevokeFreezePrivilege() (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployGenesis(deployer string, dexName string) (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployRegisterSpot(baseToken, quoteToken string) (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeployRegisterHyperliquidity(name string, tokens []string) (*trade.SpotDeployResponse, error)
func (t *trade.Client) SpotDeploySetDeployerTradingFeeShare(feeShare float64) (*trade.SpotDeployResponse, error)
```

### Perp deploy {#perp-deploy}

```go
func (t *trade.Client) PerpDeployRegisterAsset(asset string, perpDexInput info.PerpDexSchemaInput) (*trade.PerpDeployResponse, error)
func (t *trade.Client) PerpDeploySetOracle(asset string, oracleAddress string) (*trade.SpotDeployResponse, error)
```

---

## Validator operations {#validators}

Validator-only actions; pass-through wrappers around the signed-action endpoint.

```go
func (t *trade.Client) CSignerJailSelf() (*trade.ValidatorResponse, error)
func (t *trade.Client) CSignerUnjailSelf() (*trade.ValidatorResponse, error)
func (t *trade.Client) CSignerInner(innerAction map[string]any) (*trade.ValidatorResponse, error)
func (t *trade.Client) CValidatorRegister(validatorProfile map[string]any) (*trade.ValidatorResponse, error)
func (t *trade.Client) CValidatorChangeProfile(newProfile map[string]any) (*trade.ValidatorResponse, error)
func (t *trade.Client) CValidatorUnregister() (*trade.ValidatorResponse, error)
```
