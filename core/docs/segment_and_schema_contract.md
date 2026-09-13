# Shared-memory segment and wire contract

This is the consumer-facing contract for discovering and reading self-quant
market-data segments. Other design documents are informative when they conflict
with this page.

## Segment names

Publisher segments use:

```text
<prefix>.<profile>.<symbol>.<stream>.<wire_schema_major>
```

The default prefix is `/selfquant.mds`. A prefix must begin with `/`; trailing
dots are removed, while the remaining caller-supplied namespace is preserved.
Generated `profile`, `symbol`, and `stream` components are ASCII lowercase.
Characters outside `[a-z0-9_-]` become `_`. Supported streams are `ticker`,
`orderbook`, `aggbbo`, and `aggorderbook`. The complete name must not exceed
240 bytes.

Examples:

```text
/selfquant.mds.spot.btcusdt.ticker.2
/selfquant.mds.spot.btcusdt.orderbook.2
/tenant.alpha.usdm.ethusdt.ticker.1
/selfquant.mds.okx_spot.btcusdt.orderbook.2
/selfquant.mds.bybit_perp.ethusdt.ticker.2
```

For compatibility, Binance keeps the historical `spot` and `usdm` profiles.
New venues use `<venue>_<product>` where perpetual is shortened to `perp`
(`okx_spot`, `okx_perp`, `bybit_spot`, `bybit_perp`, `bitget_spot`,
`bitget_perp`, `gate_spot`, `gate_perp`, `hyperliquid_spot`, and
`hyperliquid_perp`). Network connection multiplexing never combines Rings:
each `(venue, product, symbol, stream)` remains independently discoverable.
Hyperliquid main-DEX symbols use asset names rather than protocol-internal
pair aliases: perpetual `BTC` is published as canonical `BTCUSDC`, while a
spot universe entry such as `@107` is published from its token pair (for
example `HYPEUSDC`). The original `BTC` or `@107` remains the venue symbol
used in WebSocket subscriptions.

Aggregate profiles are
`agg_<product>_<quote>_<sorted-unique-venues>`. For example:

```text
/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-hyperliquid-okx.btcusdt.aggbbo.2
/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-hyperliquid-okx.btcusdt.aggorderbook.2
```

AggBbo is a fixed 408-byte record. Its gated sides contain per-slot quantity,
the oldest contributing exchange timestamp and its `timestamp_venue` slot;
raw sides are diagnostic extrema without routing attribution. AggOrderBook is
fixed capacity (eight venues × ten source levels per side) and each merged
level carries `venue_quantity[8]`. `venue_slot_ids` is frozen for an aggregate
generation. Slot-bearing fields must be resolved through this array.

`member_mask` identifies configured/validated members. AggBbo `live_mask` and
AggOrderBook `active_mask` identify members participating after freshness/FX
checks. Crossed aggregate depth is legal and must not be rejected by generic
consumers. AggBbo is the only aggregate type that permits
`kAggSkewEnforced` and `kAggMemberDataError` in `RecordHeader.flags`; all
legacy message types and AggOrderBook continue to require zero flags.

`InstrumentUpdate.venue` is a stable numeric wire value. Existing values are
not reordered; Hyperliquid appends value `8`. Consumers compiled against an
older SDK must treat unknown venue values as unsupported instead of rejecting
or mis-decoding the containing wire record.

With `PosixShm`, the leading slash is the POSIX object name (normally visible
under `/dev/shm` without that slash). With `Hugetlbfs`, the implementation joins
the configured mount with a filename sanitized to `[A-Za-z0-9_-]`; unsupported
characters, including `/` and `.`, become `_`.

### Polymarket rolling segments

Polymarket BTC five-minute binary markets use `Venue::Polymarket`,
`ProductType::BinaryOption`, and the stable MDS-only aliases `BTC5MUP` and
`BTC5MDOWN`. Their fixed Rings are:

```text
/selfquant.mds.polymarket_binaryoption.btc5mup.ticker.2
/selfquant.mds.polymarket_binaryoption.btc5mup.orderbook.2
/selfquant.mds.polymarket_binaryoption.btc5mdown.ticker.2
/selfquant.mds.polymarket_binaryoption.btc5mdown.orderbook.2
```

The configured prefix may replace `/selfquant.mds`; the remaining profile,
alias, stream, and wire-major components do not roll with the venue market.
These Rings contain single-venue data only and are not inputs to or outputs
from cross-venue aggregation.

Gamma REST is used asynchronously only to discover the exact current market
and token IDs. All real-time book/BBO updates come from Polymarket WSS:
`book` snapshots, `price_change`, BBO, tick-size, and lifecycle events. Token
IDs never appear in segment names. The aliases are stable logical-series
identities; each concrete slug/outcome window receives a deterministic,
different physical `instrument_id`, and its catalog carries the routing token.

On rollover, the segment and canonical alias remain stable while
`instrument_id`, `venue_symbol`, condition, expiry and routing metadata change
atomically at the new catalog/Building barrier. `book_generation` remains only
the recovery epoch of one physical instrument: reconnect and resnapshot may
increment it without changing the ID. Consumers discard the previous book
epoch and accept Live data only after the new WSS snapshot is complete. The
existing BBO, Book and catalog wire layouts are unchanged.

Disconnect, subscription loss, sequence ambiguity, or stale-data timeout must
remove the alias from Live publication and mark it non-live/invalid; old BBO or
book state must not be refreshed or replayed as current. Reconnect performs
fresh asynchronous discovery when required, opens a new WSS subscription, and
rebuilds under a new `book_generation` before returning to Live.

## Independent version axes

The outer shared-ring layout and inner market-data record have independent
versions:

- `mds::transport::kRingSchemaMajor == 5` is stored in `SegmentHeader`.
  Attach fails when this layout major is incompatible.
- Wire records use magic `0x444d5153` (`SQMD`), schema major `2`, and schema
  minor `0`. The final number in the segment name is this wire major, not the
  ring major.

A wire minor release may add record types or define reserved-field semantics,
but must not change the size or layout of an existing record type. Consumers
accept newer minor versions and safely skip unknown, validly framed record
types. A wire major release is incompatible and uses a different segment name,
allowing old and new publishers to coexist.

For the coordinated v2 restart, stop every MDS C++ process before removing
crash-stale v1 segments:

```bash
scripts/cleanup_mds_shm_v1.sh --confirm-stopped selfquant.mds
```

The script refuses to unlink segments while an MDS producer, aggregator,
consumer, or `poly-mm` process is running. A v2 process never reuses an
incompatible ring-layout segment.

Consumers validate in this order:

1. expected wire major in the segment name;
2. ring major in `SegmentHeader`;
3. record magic and wire major;
4. minimum supported wire minor and record length.

## Runtime behavior

`RingMode::OverwriteOldest` is the default. Readers that fall behind must
handle `ErrorCode::RecordOverwritten` and call `resync_to_latest()` before
continuing. `RingMode::Lossless` applies producer backpressure; callers must
handle rejected publication/subscription results, including
`SubscriptionRejected`, without silently dropping state transitions.

`utils::queue::SpscRing<T, N>` is a header-only, in-process SPSC queue with
move-only producer and consumer leases. Its slots are ordinary process memory;
it cannot be used for inter-process communication.
`mds::transport::SharedRing` is the cross-process shared-memory transport
described above.

`SharedRing` currently ships in `self_quant::mds`, so consumers linking it also
resolve the MDS package dependencies. A future OMS-only transport package may
extract it as `self_quant::transport`; that split is not part of this contract
version.

## Gateway and recording are separate protocols

The WebSocket Gateway and `.sqrec.zst` files do not change this SHM contract.
Gateway frames use `SQGW` and file containers use `SQRC`; neither may be
presented as a shared-memory `SQMD` record. AggBbo Gateway payloads explicitly
identify the embedded fixed SQMD payload format. Compact AggOrderBook
Gateway/file payloads serialize only effective levels and have their own
version.

Gateway and recorder recovery consume the reader's reset generation. They never
hold a shared-ring read lease, advance a ring cursor, or invoke
`resync_to_latest()`. Therefore enabling either service cannot alter producer,
aggregator, or other reader behavior.
