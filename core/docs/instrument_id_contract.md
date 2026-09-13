# MDS/OMS instrument identity contract

## Scope

`instrument_id` is the deterministic 64-bit hot-path key shared by MDS records,
OMS requests, persisted data, and strategy state. It is not a venue order
identifier.

## Canonical identity

An instrument is uniquely identified by:

```
(venue, product_type, canonical_symbol)
```

The frozen byte serialization is
`canonical_venue|canonical_product|canonical_symbol`. Venue and product are the
protocol-defined lowercase names, the separator is `|`, and the symbol is the
MDS-normalized canonical symbol. FNV-1a 64 is applied to the UTF-8 bytes;
`std::hash`, locale transformations, display aliases, and abbreviations are
forbidden.

Polymarket rolling aliases are logical selectors, not stable physical
instruments. `polymarket|binary_option|btc5mup` is the required logical
spelling; `poly` and other abbreviations are invalid. Every resolved market
window and outcome is assigned a distinct physical `instrument_id` by
`resolve_instance(venue, product, logical_symbol, slug, outcome)`. A rollover
therefore creates a new ID instead of rebinding the old ID.

The `InstrumentCatalog` for that physical ID separates the logical and venue
identities: `canonical_symbol` is the logical alias, `market_slug` is the Gamma
market slug, and the 80-byte `venue_symbol` contains the current outcome's
decimal token ID losslessly. The 32-byte `Instrument::venue_symbol` is not a
token carrier. Publication remains ordered as
`InstrumentCatalog -> InstrumentUpdate -> BBO/OrderBook`.

`instrument_id == 0` is invalid. A zero hash is deterministically retried with
domain suffixes `|1`, `|2`, and so on. InstrumentManager keeps a startup
`id -> canonical key` index and rejects:

- one canonical identity mapped to multiple IDs;
- one ID mapped to multiple canonical identities;
- a runtime update that changes an existing mapping.

There is no allocation table, override table, backup, or machine-local state.
The same canonical key produces the same ID across machines and time. A hash
collision is a hard startup failure; automatic remapping and linear probing are
forbidden because they would make IDs depend on the subscribed universe.

## Shared wire metadata

`utils::md::Instrument` is the fixed wire payload for
`MessageType::InstrumentUpdate`. Its layout is frozen. It carries the common
hot-path metadata: venue, product type, price/quantity scales, tick and lot
sizes, contract multiplier, expiry, symbols, and canonical instrument key.

Adding venue-specific fields to this structure is a wire-schema change and is
not permitted without a schema version migration. Consumers must verify that
the payload's `instrument_id` matches the enclosing event header.

## OMS venue metadata

OMS owns an execution directory keyed by `instrument_id`. Startup instruments
are registered before submissions begin and rolling physical instruments are
registered asynchronously from catalog updates. Common
price/quantity constraints remain in `utils::md::Instrument`; venue-specific
routing and authentication metadata belongs in the side table.

The Polymarket entry contains at least:

- `condition_id` as the canonical 32-byte value;
- `token_id` as a lossless 256-bit value, never a floating-point or 64-bit
  conversion;
- YES/NO outcome;
- neg-risk flag and signature type;
- minimum order size and any venue constraint not represented by the common
  lot size;
- taker-delay policy.

For rolling markets, exact token IDs discovered through Gamma REST are
published in `InstrumentCatalog::venue_symbol`, parsed into the binary
256-bit routing snapshot, and registered under the new physical ID.
`BTC5MUP` and `BTC5MDOWN` remain logical selectors and must not be sent to the
venue as routing symbols.

The entry is immutable while orders for the instrument may exist. Market status,
allowance, balances, and other changing risk inputs are separate runtime state,
not registry identity. A missing side-table entry makes the instrument
untradeable but does not invalidate its MDS data.

## Startup validation

Before publishing market data or accepting orders:

1. Parse the shared manifest and register every common `Instrument`.
2. Reject duplicate IDs, duplicate canonical identities, invalid scales, and
   non-positive tick/lot sizes.
3. Attach and validate venue side-table entries.
4. Compare the manifest version and identity mapping across MDS and OMS.
5. Publish or accept an `instrument_id` only after all checks pass.
