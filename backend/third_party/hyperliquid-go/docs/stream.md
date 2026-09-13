# Stream reference (`c.Stream`)

`c.Stream` is the WebSocket surface. It maintains one connection, multiplexes subscriptions, services in-flight POST requests, and reconnects with exponential backoff. The Stream is not connected by `hyperliquid.New` — call `Connect(ctx)` once before subscribing or posting. On reconnect every active subscription is re-issued.

`c.Stream` is `nil` if the `Client` was built with `WithSkipStream(true)`. The handle's concrete type is `*stream.Client` from the [`stream`](https://pkg.go.dev/github.com/Simon-Busch/hyperliquid-go/stream) subpackage; advanced callers can construct one directly via `stream.New(baseURL)`. Subscription constructors (`stream.Trades`, `stream.Book`, …), `stream.WSMessage`, `stream.Subscription`, and the `stream.Logger` interface all live there.

## Contents

- [Lifecycle](#lifecycle)
- [Subscribe and Close](#subscribe)
- [Market subscription constructors](#market-subscriptions)
- [User subscription constructors](#user-subscriptions)
- [POST over WS](#post-over-ws)
- [Reconnect behaviour](#reconnect)
- [Logging](#logging)

---

## Lifecycle

### `Connect` {#lifecycle}

Open the WebSocket. Safe to call multiple times; subsequent calls return immediately if already connected. Returns an error if the URL cannot be dialled or if resubscription of any previously-registered subscription fails.

```go
func (w *stream.Client) Connect(ctx context.Context) error
```

`ctx` bounds the initial dial. After the connection is established it is *not* used to cancel the read/ping pumps — those are owned by the Stream and cancelled by `Close`.

### `Close`

Tear down the connection, stop reconnect timers, and drain pending POST requests.

```go
func (w *stream.Client) Close() error
```

`Close` is idempotent (a `sync.Once` guards the shutdown).

**Example**

```go
if err := c.Stream.Connect(ctx); err != nil { log.Fatal(err) }
defer c.Stream.Close()
```

---

## Subscribe and Close {#subscribe}

### `Subscribe`

Register a callback for the supplied subscription filter. Returns a `*Subscription` handle whose `Close()` method deregisters the callback and emits an unsubscribe frame once the last listener for that filter is gone.

```go
func (w *stream.Client) Subscribe(filter stream.SubscriptionFilter, callback func(stream.WSMessage)) (*stream.Subscription, error)
```

**Example**

```go
sub, err := c.Stream.Subscribe(stream.Trades("ETH"), func(m stream.WSMessage) {
    fmt.Printf("%s -> %s\n", m.Channel, string(m.Data))
})
defer sub.Close()
```

### `Subscription.Close`

Tear down the callback. `Close` is idempotent: a second call returns `nil` without sending another unsubscribe frame.

```go
func (s *stream.Subscription) Close() error
```

### Subscription constructors

The `stream.Trades`, `stream.Book`, etc. functions return a `stream.SubscriptionFilter` value that is passed straight into `Subscribe`. The field shape (`type`, `coin`, `user`, `interval`, `dex`) matches the wire envelope expected by the Hyperliquid websocket API.

### `stream.WSMessage`

Every subscription callback receives a `stream.WSMessage`. The payload is a raw `json.RawMessage` so callers can decode lazily into the appropriate type from the [`info`](./info.md) subpackage (`info.L2Book`, `info.Candle`, …) or the per-channel structs in `stream`.

```go
type WSMessage struct {
    Channel string          `json:"channel"`
    Data    json.RawMessage `json:"data"`
}
```

---

## Market subscription constructors {#market-subscriptions}

All constructors are top-level functions in the `stream` package returning a `stream.SubscriptionFilter` value.

| Constructor                      | WS channel            | Payload type                         |
|----------------------------------|-----------------------|--------------------------------------|
| `stream.Trades(coin)`            | `trades`              | array of trade records               |
| `stream.Book(coin)`              | `l2Book`              | `info.L2Book`                        |
| `stream.BBO(coin)`               | `bbo`                 | best bid/offer snapshot              |
| `stream.Candles(coin, "1m")`     | `candle`              | `info.Candle`                        |
| `stream.AllMids()`               | `allMids`             | `map[string]string`                  |
| `stream.AllMidsOn(dex)`          | `allMids` (with dex)  | `map[string]string` for that dex     |
| `stream.ActiveAssetCtx(coin)`    | `activeAssetCtx`      | `info.AssetCtx`                      |
| `stream.ActiveAssetData(addr, coin)` | `activeAssetData` | per-user asset context              |

**Example — book stream**

```go
sub, err := c.Stream.Subscribe(stream.Book("ETH"), func(m stream.WSMessage) {
    var b info.L2Book
    _ = json.Unmarshal(m.Data, &b)
})
defer sub.Close()
```

---

## User subscription constructors {#user-subscriptions}

Each takes an account address.

| Constructor                    | WS channel                       | Notes                                      |
|--------------------------------|----------------------------------|--------------------------------------------|
| `stream.UserEvents(addr)`      | `userEvents`                     | Quirk: upstream channel literally `userEvents`. |
| `stream.UserFills(addr)`       | `userFills`                      | All fills for the account.                 |
| `stream.OrderUpdates(addr)`    | `orderUpdates`                   | Lifecycle events for resting orders.       |
| `stream.UserFundings(addr)`    | `userFundings`                   | Funding payments stream.                   |
| `stream.UserLedger(addr)`      | `userNonFundingLedgerUpdates`    | Ledger entries excluding funding.          |
| `stream.WebData(addr)`         | `webData2`                       | UI-shaped snapshot (formerly WebData2).    |
| `stream.Notifications(addr)`   | `notification`                   | Per-user notifications.                    |
| `stream.UserTwapFills(addr)`   | `userTwapSliceFills`             | TWAP slice fills.                          |
| `stream.UserTwapHistory(addr)` | `userTwapHistory`                | TWAP order history.                        |

**Example — order updates**

```go
sub, err := c.Stream.Subscribe(stream.OrderUpdates(addr), func(m stream.WSMessage) {
    fmt.Println("order update:", string(m.Data))
})
defer sub.Close()
```

---

## POST over WS {#post-over-ws}

The Stream also services REST-style requests. This is useful when the caller wants tighter latency or to keep all traffic on one socket.

### `PostInfo`

Send an info request and wait up to `timeout` (`0` means 30s). Returns the raw payload bytes.

```go
func (w *stream.Client) PostInfo(payload map[string]any, timeout time.Duration) (json.RawMessage, error)
```

**Example**

```go
data, err := c.Stream.PostInfo(map[string]any{
    "type": "l2Book",
    "coin": "ETH",
}, 5*time.Second)
```

### `PostAction`

Send a pre-signed action. `vaultAddress` is forwarded verbatim — pass `""` to set the wire field to `null`. When `timeout == 0` the call waits up to 30s.

```go
func (w *stream.Client) PostAction(action any, signature signing.SignatureResult, nonce int64, vaultAddress string, timeout time.Duration) (json.RawMessage, error)
```

The action must already be signed via `signing.SignL1Action` or `signing.SignUserSignedAction` — see [signing.md](./signing.md).

### `Post`

Lower-level than `PostInfo` / `PostAction`. Prefer those.

```go
func (w *stream.Client) Post(requestType string, payload any, timeout time.Duration) (*stream.WsPostResponseData, error)
```

---

## Reconnect behaviour {#reconnect}

The Stream owns its reconnect state machine:

- On disconnect (read error, server close, ping timeout) the read pump exits and `handleDisconnect` schedules a reconnect.
- The initial wait is 1 s (`WithReconnectWait` overrides); each failed attempt doubles up to an internal one-minute ceiling.
- `WithMaxReconnectAttempts(n)` caps total retries. `0` (the default) means retry forever.
- On successful reconnect, every callback registered via `Subscribe` is re-issued before the call returns.

Tune from the constructor:

```go
c, _ := hyperliquid.New(
    hyperliquid.WithTestnet(),
    hyperliquid.WithMaxReconnectAttempts(5),
    hyperliquid.WithReconnectWait(2*time.Second),
)
```

## Logging {#logging}

`stream.Client` accepts a logger via `Stream.SetLogger(l)`. It is also wired by `hyperliquid.New` from `WithLogger(...)`. The default is a no-op. The Stream uses `Warnf` for transient errors (read/write/ping failures) and otherwise stays silent.

```go
type stream.Logger interface {
    Debugf(format string, args ...any)
    Infof(format string, args ...any)
    Warnf(format string, args ...any)
    Errorf(format string, args ...any)
}
```

**Related**: [signing.md](./signing.md) for signing actions you intend to post over WS.
