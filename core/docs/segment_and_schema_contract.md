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
Characters outside `[a-z0-9_-]` become `_`. The only supported streams are
`ticker` and `orderbook`. The complete name must not exceed 240 bytes.

Examples:

```text
/selfquant.mds.spot.btcusdt.ticker.1
/selfquant.mds.spot.btcusdt.orderbook.1
/tenant.alpha.usdm.ethusdt.ticker.1
```

With `PosixShm`, the leading slash is the POSIX object name (normally visible
under `/dev/shm` without that slash). With `Hugetlbfs`, the implementation joins
the configured mount with a filename sanitized to `[A-Za-z0-9_-]`; unsupported
characters, including `/` and `.`, become `_`.

## Independent version axes

The outer shared-ring layout and inner market-data record have independent
versions:

- `mds::transport::kRingSchemaMajor == 4` is stored in `SegmentHeader`.
  Attach fails when this layout major is incompatible.
- Wire records use magic `0x444d5153` (`SQMD`), schema major `1`, and schema
  minor `1`. The final number in the segment name is this wire major, not the
  ring major.

A wire minor release may only append fields. Consumers use `record_len` and
must tolerate a longer record. A wire major release is incompatible and uses a
different segment name, allowing old and new publishers to coexist.

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
