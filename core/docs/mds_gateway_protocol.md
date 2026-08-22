# MDS WebSocket Gateway Protocol

The gateway serves sampled aggregate state to frontends and medium/low-frequency
strategies. Latency-sensitive strategies should continue to consume the shared
memory rings directly.

## Connection and control

- Endpoint: `ws://<host>:<port>/v1/market-data`.
- Transport is intentionally plaintext for trusted private networks. Every
  listener requires a bearer token from the configured environment variable;
  the token is also visible to anyone able to capture network traffic. Do not
  expose this endpoint directly to the public Internet or an untrusted LAN.
- Client frames must follow RFC 6455 masking rules.
- Subscribe with
  `{"op":"subscribe","topic":"<exact configured segment name>"}`.
- Unsubscribe with the same shape and `op` set to `unsubscribe`.
- The server replies with a small JSON acknowledgement or error. Each client is
  limited to the configured number of subscriptions.

## Binary market frame

All integers are little-endian. The 52-byte header is:

| Offset | Type | Meaning |
| --- | --- | --- |
| 0 | u32 | magic `SQGW` |
| 4 | u16 | protocol major, currently 1 |
| 6 | u16 | protocol minor, currently 0 |
| 8 | u8 | kind: 1 AggBbo, 2 AggOrderBook, 3 reset |
| 9 | u8 | payload format: 1 SQMD AggBbo, 2 compact book, 0 none |
| 10 | u16 | topic id (index in configured segment order) |
| 12 | u16 | flags; bit 0 means reset |
| 14 | u16 | header bytes, currently 52 |
| 16 | u32 | payload bytes |
| 20 | u64 | SHM ring epoch |
| 28 | u64 | SHM ring sequence |
| 36 | u64 | gateway topic generation |
| 44 | u64 | source receive wall-clock nanoseconds |

AggBbo payload format 1 is the exact fixed-size SQMD `AggBboRecord`. Compact
AggOrderBook payload format 2 contains header exchange timestamp, book
generation and flags; base/quote assets; venue slots and scales; member/active
masks; bid/ask counts; then only the configured valid levels (maximum 50 per
side). Each level stores price, quantity, eight per-venue quantities,
venue mask, and contributor count without C++ padding.

The gateway publish interval is configurable down to 10 ms and is independent
of the recorder's 200 ms minimum sampling interval. It sends the latest full
image, not a replay log. A slow client has at most one frame in flight and
intermediate images are coalesced. On ring epoch, sequence, or identity recovery the server
sends a reset frame; the next market frame is a new complete image. Reconnecting
clients must subscribe again and receive the current image.
