# Signing reference

The SDK ships its own EIP-712 signing path so that signed actions can be assembled without a Python bridge. Most callers never use these helpers directly — every method on `c.Trade` signs its action internally. The exported functions live in the [`signing`](https://pkg.go.dev/github.com/Simon-Busch/hyperliquid-go/signing) subpackage and exist for advanced scenarios: posting actions over the WebSocket via `c.Stream.PostAction`, signing actions to be relayed by another process, or driving the signing path in tests.

Two EIP-712 domains are in play:

- **L1 phantom-agent domain**: used for trading-side actions (orders, cancels, transfers, leverage updates, etc.). The action map is msgpack-hashed first, then wrapped in an `Agent` envelope and signed against the cached domain separator. `signing.SignL1Action` does all of this.
- **HyperliquidSignTransaction domain** (chainId `0x66eee` = 421614): used for user-signed actions whose payload is already typed-data (USD class transfer, spot transfer, withdraw, agent approval, etc.). `signing.SignUserSignedAction` is the entry point.

Internal helpers (`signInner`, `actionHash`, `constructPhantomAgent`, `l1Payload`, `hashStructLenient`, `convertStr16ToStr8`, the cached domain separator) live in `internal/eip712` and are not part of the public API. The exported wire-action structs (`signing.OrderWire`, `signing.CancelAction`, `signing.TWAPOrderAction`, …) live alongside the signers in `signing`.

## Contents

- [`SignL1Action`](#signl1action)
- [`SignUserSignedAction`](#signusersignedaction)
- [`FloatToUsdInt`](#floattoUsdint)
- [`GetTimestampMs`](#gettimestampms)
- [`SignatureResult`](#signatureresult)

---

## `SignL1Action` {#signl1action}

Sign an L1 action via the phantom-agent EIP-712 scheme.

```go
func signing.SignL1Action(
    privateKey *ecdsa.PrivateKey,
    action any,
    vaultAddress string,
    timestamp int64,
    expiresAfter *int64,
    isMainnet bool,
) (signing.SignatureResult, error)
```

**When to call**: when you want to sign a trading action yourself for transport via `c.Stream.PostAction`, or to inspect/persist the signature before sending.

**Parameters**

- `action`: the action value. Anything msgpack-encodable — typed structs from the [`signing`](https://pkg.go.dev/github.com/Simon-Busch/hyperliquid-go/signing) subpackage (`signing.OrderAction`, `signing.CancelAction`, …) or a `map[string]any`.
- `vaultAddress`: the vault on whose behalf the action is signed. Empty string for the signer's own account.
- `timestamp`: Unix-ms nonce. Use `signing.GetTimestampMs()`.
- `expiresAfter`: optional expiry on the wire. Pass `nil` to omit the field.
- `isMainnet`: selects the `Mainnet`/`Testnet` source string in the phantom agent.

**Example — post a signed action over WS**

```go
import (
    "github.com/Simon-Busch/hyperliquid-go/signing"
)

ts := signing.GetTimestampMs()
action := map[string]any{ "type": "scheduleCancel", "time": nil }

sig, err := signing.SignL1Action(pk, action, "", ts, nil, true)
if err != nil { log.Fatal(err) }

raw, err := c.Stream.PostAction(action, sig, ts, "", 5*time.Second)
```

---

## `SignUserSignedAction` {#signusersignedaction}

Sign a user-signed action using the `HyperliquidSignTransaction` domain.

```go
func signing.SignUserSignedAction(
    privateKey *ecdsa.PrivateKey,
    action map[string]any,
    payloadTypes []apitypes.Type,
    primaryType string,
    isMainnet bool,
) (signing.SignatureResult, error)
```

**When to call**: when signing a user-signed action (USD class transfer, spot transfer, agent approval, etc.) for off-band transport. The same scheme is used internally by `c.Trade.Transfer.SpotToPerp`, `c.Trade.ApproveAgent`, and similar.

The function mutates `action` by injecting `hyperliquidChain` (`"Mainnet"` or `"Testnet"`) and `signatureChainId` (`"0x66eee"`) so the JSON body sent to `/exchange` matches what was signed.

**Parameters**

- `action`: the action map; receives the two mutations above.
- `payloadTypes`: the per-action EIP-712 type list. The wire schemas are defined inside the `signing` subpackage (`usdClassTransferSignTypes`, `spotTransferSignTypes`, etc.) — currently unexported. Callers that need to drive this directly will replicate the schema; see `trade/transfer.go` for a working example.
- `primaryType`: the EIP-712 primary-type string, e.g. `"HyperliquidTransaction:UsdClassTransfer"`.
- `isMainnet`: selects the `hyperliquidChain` field.

**Related**: see methods on `*trade.Client` and on `c.Trade.Transfer` for representative call-sites.

---

## `FloatToUsdInt` {#floattoUsdint}

Convert a decimal USD amount to the integer form expected by Hyperliquid (six decimals of precision for USDC).

```go
func signing.FloatToUsdInt(value float64) int
```

**Example**

```go
amount := signing.FloatToUsdInt(125.5) // 125500000
```

---

## `GetTimestampMs` {#gettimestampms}

Return the current Unix time in milliseconds.

```go
func signing.GetTimestampMs() int64
```

Use this when constructing your own nonces for `signing.SignL1Action` / `signing.SignUserSignedAction`.

---

## `signing.SignatureResult` {#signatureresult}

The `{r, s, v}` triple produced by every signing call. Type aliased from `internal/eip712.SignatureResult`.

```go
type signing.SignatureResult = eip712.SignatureResult
```

Fields are `R` (hex-encoded), `S` (hex-encoded), `V` (int, already adjusted by +27 to match Ethereum recovery encoding).
