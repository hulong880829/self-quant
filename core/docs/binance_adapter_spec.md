# Binance Spot / USD-M Adapter 规范

## 1. 实现状态

当前 `mds/exchange/binance` 已实现：

- Spot/USD-M capability 常量；
- simdjson OnDemand 的 `bookTicker` 与 diff-depth JSON 字段解析；
- Binance Spot 官方 `stream_1_0.xml` 的 SBE BestBidAsk/DepthDiff 解码；
- decimal string 到指定 scale 的 `int64_t`；
- Spot `U/u`、USD-M `U/u/pu` 连续性状态机；
- ping/pong 超时状态记录和 23h50m rotation 判断；
- schema/version/template/blockLength 与消息边界校验。

尚未实现：

- DNS/TCP/TLS/WebSocket HTTP upgrade 与 Binance 订阅的完整编排；
- REST HTTP client、exchange info/snapshot parser；
- ping→pong 写回、限频器、重连/轮换；
- snapshot 档位装载、缓存增量返回给 OrderBook、wire 发布；
- combined-stream envelope（`{"stream":...,"data":...}`）解包；
- 生产指标与 capture/replay。

Spot SBE decoder 已 `Available`，但官方 SBE endpoint 建连、Ed25519 API Key header、binary/text frame routing 和 REST snapshot orchestrator 仍属于上述未闭环网络编排；USD-M SBE 仍 `Unavailable`。

因此本规范同时记录官方适配规则和当前代码差距；“应”表示上线要求，不表示已实现。

## 2. 官方地址与频道

### 2.1 Spot

- WS host：`stream.binance.com`
- raw URL：`wss://stream.binance.com:9443/ws/<streamName>`
- combined URL：`wss://stream.binance.com:9443/stream?streams=<stream1>/<stream2>`
- REST host：`api.binance.com`
- exchange info：`GET /api/v3/exchangeInfo?symbol=BTCUSDT`
- depth snapshot：`GET /api/v3/depth?symbol=BTCUSDT&limit=<limit>`，最大返回 5000 档/侧
- BBO channel：`<symbol>@bookTicker`，实时
- diff depth：`<symbol>@depth`（默认 1000ms）或 `<symbol>@depth@100ms`

Spot SBE 使用独立 host `stream-sbe.binance.com[:9443]`：

- Best bid/ask：`<symbol>@bestBidAsk`，SBE `BestBidAskStreamEvent`；
- diff depth：`<symbol>@depth`，SBE `DepthDiffStreamEvent`，20ms；
- market-data event 使用 binary frame，在线订阅响应与 `serverShutdown` 使用 JSON text frame；
- 所有 SBE 时间字段为微秒；
- 官方要求 Ed25519 API Key，并在 WebSocket upgrade 中设置 `X-MBX-APIKEY`；无需 timestamp/signature 或额外权限。

当前 `capability(Profile::Spot).sbe=Available`，但 capability 的单一 `websocket_host` 字段仍返回 JSON host `stream.binance.com`，尚不能自动表达协议专用的 `stream-sbe.binance.com`。

stream symbol 必须小写，如 `btcusdt`；REST symbol 使用大写 `BTCUSDT`。

### 2.2 USD-M

- WS host：`fstream.binance.com`
- raw URL：`wss://fstream.binance.com/ws/<streamName>`
- combined URL：`wss://fstream.binance.com/stream?streams=<stream1>/<stream2>`
- REST host：`fapi.binance.com`
- exchange info：`GET /fapi/v1/exchangeInfo`
- depth snapshot：`GET /fapi/v1/depth?symbol=BTCUSDT&limit=<limit>`，官方支持的最大 limit 为 1000
- BBO channel：`<symbol>@bookTicker`，实时
- diff depth：`<symbol>@depth`、`<symbol>@depth@100ms`、`@250ms` 或 `@500ms`（默认 250ms）

当前 `capability(Profile::UsdM)` 将 `requires_previous_final_id=true`，与 `pu` 连续性要求一致。

## 3. JSON 字段映射

### 3.1 Spot `bookTicker`

官方 payload 核心字段：

- `u` → `BookTicker.update_id`
- `s` → 固定 `BookTicker.symbol[32]`；必须非空且长度 `<32`
- `b`/`B` → bid price/quantity
- `a`/`A` → ask price/quantity

Spot bookTicker 通常不提供 `E`/`T`；当前 parser 将缺失字段置 0，并把 `price_exponent/quantity_exponent` 设为 parser scale 的负值。

### 3.2 USD-M `bookTicker`

- `e="bookTicker"`（当前不校验）
- `E` → `event_time_ms`
- `T` → `transaction_time_ms`
- `u` → `update_id`
- `s` → 固定 `BookTicker.symbol[32]`
- `b/B/a/A` → BBO

同一个 parser 兼容两类字段。JSON 与 SBE book ticker 都填充固定 32 字节、NUL 结尾的 symbol 以及 price/quantity exponent，可安全按 symbol 路由多标的；routing 层仍必须验证 event type 与订阅集合，防止串流污染。

### 3.3 Spot diff depth

- `e="depthUpdate"`、`E`、`s`（当前读取 E/s，但仍不验证 e）
- `U` → `first_update_id`
- `u` → `final_update_id`
- `b` → bid `[price,quantity][]`
- `a` → ask `[price,quantity][]`
- 无 `pu`，当前值置 0；`T` 缺失则置 0

### 3.4 USD-M diff depth

- `e="depthUpdate"`、`E`、`T`、`s`
- `U` → first update ID
- `u` → final update ID
- `pu` → previous stream event final update ID
- `b`/`a` → bid/ask updates

当前 parser 读取 E/T/U/u/pu/b/a/s，但不验证 e。symbol 为空或长度 ≥32 时拒绝。

### 3.5 定点数

价格和数量字符串按 metadata scale 转 `int64_t`：

```text
mantissa = decimal × 10^scale
```

小数超过 scale 时，额外位必须全为 0；溢出或非法字符失败。metadata 必须来自 exchange info filters。JSON bookTicker/depth 都设置 `price_exponent=-price_scale`、`quantity_exponent=-quantity_scale` 并填 symbol；SBE 直接读取消息 exponent 与 symbol。`SymbolMetadata` 当前存在但没有 parser，也没有自动推导 tick/lot mantissa。

## 4. Spot 快照桥接与连续性

对每个 symbol 独立维护连接、缓存、generation 和 book：

1. 打开 diff-depth WS 并开始缓存事件。
2. 获取 REST snapshot；令 `L=lastUpdateId`。
3. 若 `L` 小于缓存首事件 `U`，说明快照过旧，重新取快照。
4. 丢弃所有 `u <= L` 的事件。
5. 第一个处理事件必须满足 `U <= L+1 <= u`。
6. 装载 snapshot 后应用该事件；将 local ID 更新为事件 `u`。
7. Live 后每个事件：
   - `u <= local`：陈旧/重复，丢弃；
   - `U > local+1`：存在 gap，立即失效并重快照；
   - `U <= local+1 <= u`：应用并令 local=`u`。
8. 每档 quantity=0 删除，否则绝对替换数量。

当前 `DepthSynchronizer(Profile::Spot)` 的 bridge/continuous 都采用“覆盖 next ID”判断，符合上述范围规则；缓存上限默认 4096，溢出返回 `Resnapshot`。`inject_snapshot(L)` 后仅为 Bridging；外部装载 snapshot 档位后必须通过 `drain_buffered(context, callback)` 按序真正应用缓存，成功应用至少一条后才进入 Live。

## 5. USD-M 快照桥接与 `pu`

1. 打开 diff-depth WS 并缓存事件。
2. 获取 `/fapi/v1/depth` snapshot，令 `L=lastUpdateId`。
3. 丢弃 `u < L` 的事件。
4. 首个处理事件必须满足 `U <= L <= u`。
5. 装载 snapshot、应用桥接事件，令 local=`u`。
6. 后续事件必须满足 `pu == local`；不满足即 gap，清空状态并重快照。
7. quantity=0 删除，否则替换。

当前 `DepthSynchronizer(Profile::UsdM)` bridge 目标为 L，Live 连续性严格检查 `pu == last_update_id`。它没有检查 symbol/event type，也不维护订单簿。

## 6. 缓存与状态机

状态：

- `WaitingSnapshot`：事件入 deque，返回 Buffer；
- `Bridging`：已有 snapshot ID，但尚未证明缓存已实际应用；
- `Live`：连续事件返回 Apply；
- `Invalid`：后续事件都返回 Resnapshot。

非法 `U>u` 或 buffer 达上限会清空并 Invalid。`reset()` 回 WaitingSnapshot。

`inject_snapshot()` 现在只保存 snapshot ID 并进入 Bridging，不消费缓存、不伪造 Live。调用方必须调用 `drain_buffered(void*, ApplyBuffered)`：

- stale 事件按 Spot/USD-M 规则丢弃；
- 首个非 stale 事件必须 bridge，后续必须 continuous；
- callback 必须把每条事件实际应用到外部 OrderBook 并返回 true，之后 synchronizer 才推进 ID；
- 至少应用一条且排空后返回 BecameLive；缓存为空/全 stale 返回 Buffer 并保持 Bridging；
- callback 为空、gap、非法区间或应用失败均清缓存、置 Invalid、返回 Resnapshot。

缓存回放接口缺口已修复；公网 orchestrator 仍需按该生命周期接线和验证。有 snapshot 前缓存时，禁止绕过 drain 直接把新事件送入 Bridging 状态的 `on_update`。

## 7. 心跳、断线和 24h

### 7.1 Spot 官方行为

- 单连接有效期最长 24h。
- 服务端约每 20 秒发送 WebSocket ping frame。
- 若约 1 分钟内没有收到对应 pong，连接会断开。
- 收到 ping 后应尽快用相同 payload 发 pong。未经请求的 pong 可发送但不能替代对 server ping 的响应。

### 7.2 USD-M 官方行为

- 单连接有效期最长 24h。
- 服务端约每 3 分钟发送 ping。
- 若约 10 分钟内没有收到 pong，连接会断开。
- pong 应复制 ping payload。

### 7.3 adapter 要求

- 网络 parser 已能把 Ping/Pong control frame 交给 callback，但当前没有发送 pong 的 orchestrator。
- `ConnectionHealth::on_ping/on_pong` 只记时间；`pong_due` 的 timeout 由调用方按 profile 配置。
- 在 23h50m `rotation_due=true` 时建立新连接，完成订阅和序列接续后再关闭旧连接。当前只有判断，没有双连接切换。
- 任何断线都使 book 非 Live；重连必须重新 REST snapshot，不能仅凭最后 ID 续接。
- close code/reason、TLS/HTTP 错误、最后序列和重连 backoff 必须记录。

## 8. 限频

### 8.1 Spot

- 每连接最多 1024 streams。
- 每秒最多 5 条发往服务端的消息；WebSocket PING、PONG 和 JSON control 都计入。
- 单 IP 每 5 分钟最多 300 次连接尝试。
- SUBSCRIBE/UNSUBSCRIBE/LIST 等请求 `id` 用于关联响应。

### 8.2 USD-M

- 每连接最多 1024 streams。
- 每秒最多 10 条发往服务端的消息。
- 订阅控制和 pong 必须共享发送预算；心跳优先。

当前 `VenueProfile` 默认 `max_streams_per_connection=200` 和 `messages_per_second=5`，但没有 token bucket 实现。REST request weight 随 endpoint/limit 而变，adapter 应解析响应 rate-limit headers、按官方最新表配置并处理 429；禁止无限立即重试。

## 9. WebSocket/TLS 要求

- TLS 必须验证系统 CA 与 host；SNI 使用 capability host。
- HTTP upgrade 必须验证 101、`Upgrade/Connection`、`Sec-WebSocket-Accept`；当前仓库没有 upgrade parser。
- 客户端发出的 frame 必须 mask；服务端 frame 不应 mask。
- 明确拒绝 `permessage-deflate`，当前 `validate_websocket_extensions` 已覆盖。
- 支持 fragmented data message 和中间 control frame；当前 parser 有单元测试。
- raw stream payload 可直接送 JSON parser；combined stream 必须先取 `data`，当前 parser 不能直接解析 envelope。
- 限制单消息 ≤配置 parser capacity（默认 1 MiB），超限断开并告警。

## 10. SBE 现状

### 10.1 已实现的 Spot decoder

实现直接依据官方 [`stream_1_0.xml`](https://github.com/binance/binance-spot-api-docs/blob/master/sbe/schemas/stream_1_0.xml)，不把 native C++ struct overlay 到消息：

- schema id=1、version=0，均精确匹配；
- 8 字节 little-endian message header：blockLength/templateId/schemaId/version；
- `BestBidAskStreamEvent` template 10001，root 最小 blockLength=50；
- `DepthDiffStreamEvent` template 10003，root 最小 blockLength=26；
- depth repeating group dimension header=4 字节，entry 最小 blockLength=16，每侧最多 5000；
- template/schema/version 错误、blockLength 低于 schema 最小值、消息/组/symbol 截断、负 ID/time、`U>u` 都返回 false；
- 大于最小值的 root/group blockLength 按 SBE 扩展规则跳过未知尾部字段，因此 blockLength 校验严格但不是“必须等于最小值”；
- BestBidAsk 解出 `u`、BBO、固定 32 字节 symbol、exponent 和 event time；DepthDiff 解出 `U/u`、bids/asks、固定 symbol、exponent，Spot `pu=0`；两者均可安全进行多标的路由；
- schema 中 template 10002 的 `DepthSnapshotStreamEvent` ID 常量已声明，但当前没有 decoder API。

`SpotSbeDecoder::availability=Available`，`capability(Profile::Spot).sbe=Available`，Spot `WireProtocol::Sbe` 可通过 service 配置校验。单元测试构造官方布局 fixture，覆盖 template 10001/10003 解码与错误 schema 拒绝。

### 10.2 尚未闭环

- USD-M `Capability.sbe=Unavailable`，USD-M SBE 配置仍返回 `UnsupportedVenueProduct`；
- `allow_json_fallback` 仍不执行自动 fallback；
- 未实现 `stream-sbe.binance.com` 的 API Key upgrade、binary market event/text control routing、serverShutdown 切换、REST snapshot/订单簿/wire 发布；
- 未做真实公网 SBE capture/replay、REST 逐档比对或 24h 验收。

因此可声明“Spot SBE 1.0 decoder 与离线测试可用”，不能声明“Spot SBE 公网行情链路已上线”。

## 11. 故障处理

| 故障 | 必须动作 |
|---|---|
| JSON/decimal/symbol/type 非法 | 丢弃整条、计数；连续出现时重连 |
| `U>u`、Spot gap、USD-M `pu` mismatch | book Invalid，generation+1，清缓存，重快照 |
| snapshot 太旧/缓存溢出 | 重新快照并退避 |
| REST 429/5xx | 遵循 header/backoff+jitter，保持非 Live |
| WS ping 超时/close/TLS 错误 | 关闭并重连；重新完整桥接 |
| scale/tick/lot 变化 | NeedsRestart，重新 metadata+snapshot |
| SHM 背压 | 不覆盖；告警并执行已配置消费者策略 |

## 12. 官方参考

- Spot WebSocket Streams（频道、连接、心跳、限频、本地订单簿）：https://developers.binance.com/docs/binance-spot-api-docs/web-socket-streams
- Spot SBE Market Data Streams：https://github.com/binance/binance-spot-api-docs/blob/master/sbe-market-data-streams.md
- Spot SBE `stream_1_0.xml`：https://github.com/binance/binance-spot-api-docs/blob/master/sbe/schemas/stream_1_0.xml
- Spot REST Market Data：https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints
- Spot Order Book：https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints#order-book
- USD-M WebSocket General：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams
- USD-M Book Ticker：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams/Individual-Symbol-Book-Ticker-Streams
- USD-M Diff Depth：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams/Diff-Book-Depth-Streams
- USD-M Local Book：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams/How-to-manage-a-local-order-book-correctly
- USD-M REST Order Book：https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Order-Book
- USD-M Exchange Information：https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Exchange-Information

上线前必须重新核对这些官方页面；交易所规则可能更新，本文不替代运行时限频与错误响应。
