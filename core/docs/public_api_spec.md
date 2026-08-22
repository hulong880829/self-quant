# Self Quant C++ public API

The installed consumer surface is the `self_quant` CMake package and the
headers under `mds/api`, `oms/api`, and `strategyframe`. Internal exchange,
transport, state-engine, and manager-access headers are not stable APIs.

## StrategyFrame

Include `strategyframe/strategyframe.h`, load a three-section YAML file with
`load_config()`, construct `StrategyRunner<T>`, and call `run()` once. `T` must
satisfy `Strategy<T>` by implementing the eight callbacks documented in
`strategyframe.md`. Callbacks and manager mutation are serialized on the
strategy owner thread in both threading modes.

`StrategyContext` is the only strategy command/query facade:

- orders: `place_order` and `cancel`; order type, time-in-force, flags,
  price, quantity and expiration are explicit `OrderRequest` fields;
- timers: `schedule_timer` and `cancel_timer`;
- immutable configuration: `params`;
- memory views: `open_orders`, `find_order`, `positions`, `find_position`, and
  `find_instrument`;
- runtime state: `oms_status`, `metrics`, `now_ns`, and `request_stop`.

Order success means local command acceptance. Venue acceptance and terminal
state arrive through `on_order_update`. Returned spans and market-data spans are
borrowed and must not outlive the current callback. Commands called from a
non-owner thread return `InvalidThread`; cross-thread shutdown uses
`StrategyRunner::request_stop()`.

Subscriptions are configured before startup in `mds:`. They are intentionally
not mutable through the context after shared-memory readers and the OMS
instrument registry have been frozen.

## OMS

`oms/api/execution_channel.h` owns an in-process OMS instance. Binance
Spot/USD-M place and cancel use the configured trading WebSocket exclusively;
REST is limited to control, time synchronization, listen-key lifecycle, and
reconciliation. Polymarket order entry uses its official REST CLOB API while
its user WebSocket carries reports.

The channel supports Inline and Dedicated-I/O modes, fixed-capacity command and
update queues, capability preflight, per-lane tokens, status snapshots, and
bounded update draining. A disconnected, authenticating, backpressured, or
reconciling adapter is not order-ready and never falls back to another order
transport.

## MDS

`mds/api/mds_api.h` exposes the process-global self-hosted producer API.
StrategyFrame may instead attach directly to producer-owned shared-memory
rings. Instrument and market records use the versioned wire contract described
in `mds_wire_protocol.md`; consumers must treat schema, sequence, generation,
and overwrite failures as data-integrity events.

## Deliberate exclusions

StrategyFrame v4 does not maintain balances, margin, collateral, fees, or PnL.
It also does not provide strategy-level order-rate, notional, maximum-open-order
or kill-switch risk limits. Deployment or a higher strategy layer must provide
those safeguards before live trading.

Offline tests and loopback benchmarks do not prove live venue acceptance. The
external acceptance status is recorded separately in `strategyframe.md`.
# `utils` / `mds` / `oms` / `strategyframe` 公共 API 规范（当前实现）

## 1. 适用范围

本文以 `utils/include/**`、`mds/include/**`、`oms/include/**` 与
`strategyframe/include/**` 当前头文件及实现为准。`mds::api` 使用 staged
生命周期：`init()` 只初始化上下文，`subticker()`、`suborderbook()` 和
aggregate 注册函数建立 Pending 意图，`start()` 才合并连接、attach
aggregate 输入 Ring 并启动 worker。StrategyFrame v4 的完整运行手册见
[`strategyframe.md`](strategyframe.md)。

## 2. 通用结果与错误

```cpp
template<class T> struct Result {
  T value{};
  ErrorCode error{ErrorCode::Ok};
  std::string message{};
  explicit operator bool() const noexcept;
};
```

`Result<void>` 无 `value`。成功的唯一判断是 `error==Ok`；失败时 `message` 可能为空，不可把空 message 当成功。

`ErrorCode:uint16_t` 当前顺序值：

| 值 | 名称 | 当前可见触发 |
|---:|---|---|
| 0 | `Ok` | 成功 |
| 1 | `AlreadyInitialized` | 重复 `init` |
| 2 | `NotInitialized` | 未 init 的订阅/退订；未打开 ring 注册 reader |
| 3 | `InvalidConfig` | 配置、订阅深度、ring 几何、记录大小或 attach 布局非法 |
| 4 | `UnsupportedVenueProduct` | venue/product 或必需 profile 不受支持 |
| 5 | `InstrumentNotFound` | 当前 API 实现未返回 |
| 6 | `ShmCreateFailed` | shm/open/ftruncate/mmap/stat 失败 |
| 7 | `HugepageUnavailable` | hugetlbfs 打开失败（即使根因可能是其他 `errno`） |
| 8 | `TscUnstable` | 要求 stable TSC 但 CPUID 不支持 invariant TSC |
| 9 | `CpuBindFailed` | 当前 `mds::api` 未实际绑核，未返回 |
| 10 | `QuotaExceeded` | 订阅/reader 满、无记录、记录未提交、慢 reader 背压 |
| 11 | `SubscriptionRejected` | ring reader overrun 或订阅被数据源拒绝 |
| 12 | `RecordOverwritten` | reader 指向的数据已被覆盖 |
| 13 | `InvalidHandle` | 未知订阅/reader/lease、stale cursor |
| 14 | `InternalError` | epoll/TLS、codec 或 ring 元数据错误 |
| 15 | `AlreadyStarted` | `start()` 后再注册或退订 |
| 16 | `AggregateNotReady` | aggregate 尚无完整可读 image |
| 17 | `SubscriptionTypeMismatch` | aggregate handle 与读取类型不匹配 |
| 18 | `InstrumentMismatch` | aggregate 输入 metadata/单位不兼容 |

`ErrorCode` 均显式赋值。已有值不得重排；新值只能追加。

## 3. `mds::api`

### 3.1 配置枚举

- `ProductType` 是 `using ProductType = utils::md::ProductType`，不再是独立枚举；完整值为 Unknown=0、Spot=1、Perpetual=2、Future=3、BinaryOption=4、Equity=5。当前行情 service 接受 capability 表中已实现的 Binance、OKX、Bybit、Bitget、Gate 与 Hyperliquid Spot/Perpetual 组合。
- `WireProtocol:uint8_t`：Json=1, Sbe=2；Spot SBE 当前可通过 `init`，USD-M SBE 被拒绝。
- `RuntimeMode:uint8_t`：Automatic=0, Manual=1。
- `Strictness:uint8_t`：Relaxed=0, Strict=1；当前仅保存，未改变行为。
- `ShmBackend:uint8_t`：PosixShm=0, Hugetlbfs=1。
- `SlowConsumerPolicy:uint8_t`：Latest=0, Disconnect=1, Lossless=2；当前仅保存。
- `SubscriptionState:uint8_t`：Unknown=0, Pending=1, Live=2, Failed=3, Stopped=4。

### 3.2 配置结构

`RuntimeConfig`

- `mode=Automatic`
- `strictness=Relaxed`
- `cpu_ids={}`
- `numa_node=-1`
- `require_stable_tsc=false`

Manual 模式要求非空 `cpu_ids`；所有 CPU 必须小于 `_SC_NPROCESSORS_ONLN` 且不重复。当前不会调用 bind，也未使用 NUMA/strictness。

`ShmConfig`

- `backend=PosixShm`
- `hugetlbfs_mount="/dev/hugepages"`
- `ring_bytes=8 MiB`，必须为 2 的幂；
- `max_record_bytes=64 KiB`，必须非零且 ≤ ring/8；
- `max_readers=32`，`mds::api` 只检查非零，真正 `SharedRing::open` 还要求 ≤64；
- `reader_lease_timeout_ns=5,000,000,000`
- `allow_hugepage_fallback=false`
- `unlink_on_shutdown=false`

原始 producer Ring 在 `start()` 时创建；进程内 aggregate 使用同一组 backend/reader 参数 attach 已存在的输入 Ring。

`VenueProfile`

- `venue="binance"`
- `product=Spot`
- `websocket_endpoint`、`rest_endpoint`：该 `(venue, product)` 的连接端点；
- `api_key_env`、`secret_env`、`passphrase_env`：只保存环境变量名，实际 secret 不写入配置；
- `shm_prefix="/selfquant.mds"`
- `protocol=Json`
- `max_streams_per_connection=200`
- `messages_per_second=5`
- `snapshot_pacing_ms=100`
- `redundant_ab=false`
- `allow_json_fallback=false`

每个 `(venue, product)` 只能配置一个 profile。当前公共行情支持 Binance、OKX、Bybit、Bitget、Gate 和 Hyperliquid 已实现的 Spot/Perpetual 组合。OKX 最快需登录的公共频道从上述环境变量读取凭据，缺失或登录失败不会静默降级。

`MdsConfig` 包含 `runtime`、`shm`、`venues` 和 `max_subscriptions=4096`。

`TickerSubscription`：venue=`"binance"`、product=`Spot`、`symbol` 必填；ticker 语义为低延迟 BBO。

`OrderBookSubscription : TickerSubscription`：

- `depth=1000`
- `ladder_levels_per_side=4096`（兼容字段名，实际单位为 price tick；
  新 YAML 使用 `ladder_ticks_per_side`，上限为
  `utils::md::kMaxLadderLevels`）
- `slow_consumer=Latest`
- `update_interval_ms=0`（0 表示频道默认；仅支持可配置周期的原生频道）
- `orderbook_channel=""`（空表示 `fastest_top10`）
- `ladder_price_band_bps=10`

当前要求 depth 非零，ladder 必须在配置上限内。频道、深度和更新周期由共享 capability 表校验；不支持周期参数的频道收到非零 `update_interval_ms` 时返回 `InvalidConfig`，不会忽略。

### 3.3 服务函数

```cpp
Result<void> init(const MdsConfig&);
Result<SubscriptionHandle> subticker(const TickerSubscription&);
Result<SubscriptionHandle> suborderbook(const OrderBookSubscription&);
Result<SubscriptionHandle> register_agg_bbo(const AggregateSubscription&);
Result<SubscriptionHandle> register_agg_orderbook(const AggregateSubscription&);
Result<void> start();
Result<void> unsubscribe(SubscriptionHandle);
SubscriptionState query_state(SubscriptionHandle) noexcept;
ErrorCode try_read_agg_bbo(SubscriptionHandle, AggBboRecord&) noexcept;
ErrorCode try_read_agg_orderbook(SubscriptionHandle,
                                 AggOrderBookRecord&) noexcept;
void shutdown() noexcept;
```

- 全局单例进程生命周期；不可创建多个 service instance。
- 所有函数通过同一 mutex 串行，因此这些入口可由多线程调用。
- `init` 成功时创建 epoll loop 和 OpenSSL client context，但不启动线程。
- `start` 将相同 `(venue, product)` 的原始行情订阅合并为 multiplexed connection，并启动 producer/aggregate worker。
- handle 从 1 递增，`shutdown()` 后重置为 1；旧 handle 不能跨 init 周期使用，但因为没有 generation，数值可能复用。
- `unsubscribe` 只允许在 `start()` 前取消 Pending handle；运行后返回 `AlreadyStarted`。
- `query_state` 在未初始化或未知 handle 时都返回 Unknown，无法区分原因。
- `shutdown` 可重复调用，清空订阅并销毁 TLS/epoll；持有的配置字符串由 service 自己复制。

### 3.4 进程内聚合

`AggregateSubscription` 接受 2–8 个唯一 venue、一个 product 和一个输出 symbol。`ttl_us=0` 时按各 venue 实际 capability 周期分别派生 TTL；非零值是显式统一覆盖。`fx_ttl_us` 与 `fx_max_depeg_bps` 控制进程内 USDC/USDT 转换有效性。AggBbo 与 AggOrderBook 必须分别注册，拥有独立 handle、freshness 和读取函数。

- 注册只 attach 已存在的单所 Ring，不会替用户自动启动 producer。
- aggregate price/quantity scale 在首次完整 metadata 就绪时冻结；不兼容的成员 metadata 被排除并推进 aggregate generation。
- 每所每侧最多贡献十档；不足十档贡献全部。AggOrderBook 允许跨所合并后出现多档合法交叉。
- `member_mask` 表示已配置且 metadata 有效的 slot；AggBbo 的 `live_mask` 和 AggOrderBook 的 `active_mask` 表示本条记录实际参与的成员。
- `venue_slot_ids[slot]` 把 slot 映射为 `Venue`。`best_venue`、`timestamp_venue` 和 `cross_*_venue` 字段保存的是 slot，不是直接的 Venue 枚举；消费者必须经 `venue_slot_ids` 解引用。
- gated 按 TTL/FX 过滤并用于路由；raw 只保留仍在 `raw_liveness_us` 窗口内的诊断 BBO。`raw_liveness_us=max(ttl,min(saturating_mul(ttl,10),5s))`，因此 gated 成员始终是 raw 成员的子集。
- `raw_bid/ask.venue_mask` 只表示该 raw 顶档的贡献者，不是完整 raw 成员集合；不能用它与 `live_mask` 做集合包含校验。
- Hyperliquid `BTCUSDC` 聚合到 `BTCUSDT` 时自动 attach Binance Spot `USDCUSDT`；FX 无效或过期时，该成员同时从 raw、gated 和 aggregate depth 排除。
- cross-skew 仅作用 AggBbo，默认 observe-only。enforce 模式按 `timestamp_venue` 整家逐记录剔除；`kAggSkewEnforced` 表示本条发生过剔除。同一成员自身 crossed BBO 不做 skew 剔除，并通过 `kAggMemberDataError` 报告。
- 两种 typed read 都复制最新定长 image，不分配内存。两条 aggregate 输出不是原子快照。

## 4. Binance adapter

### 4.1 capability 与数据结构

```cpp
Capability capability(Profile) noexcept;
```

`Profile`：Spot=0, UsdM=1。Spot 返回 `stream.binance.com/api.binance.com`、无需 `pu`、SBE=Available；UsdM 返回 `fstream.binance.com/fapi.binance.com`、要求 `pu`、SBE=Unavailable。两者 bookTicker/diffDepth=true。注意官方 Spot SBE endpoint 实际是 `stream-sbe.binance.com`；当前 capability 仍返回 JSON host，且 service 尚未消费 endpoint 建连，公网 orchestrator 必须按协议选择正确 host。

`SymbolMetadata`、`PriceLevel`、`BookTicker`、`DepthUpdate` 都由调用方拥有。`BookTicker` 和 `DepthUpdate` 都携带进程内 `int8_t price_exponent/quantity_exponent` 与 `std::array<char,32> symbol`；JSON bookTicker/depth 将 exponent 设为 `-price_scale/-quantity_scale` 并从 `s` 填 symbol，SBE 从消息读取。symbol 必须非空且长度 `<32`，成功时 NUL 结尾。`DepthUpdate` 的 bid/ask vector 由 parser/decoder 清空后重新填充。这些字段不属于 `utils::md::wire` ABI。

### 4.2 `JsonParser`

```cpp
JsonParser(int8_t price_scale, int8_t quantity_scale);
~JsonParser();
bool parse_book_ticker(std::string_view, BookTicker&, std::string& error);
bool parse_depth(std::string_view, DepthUpdate&, std::string& error);
```

- 不可复制；也未声明 move，因此不可移动。
- 持有 simdjson parser/buffer，单实例不可并发调用；每线程/连接使用独立实例或外部加锁。
- 输入只在调用期间借用；输出由调用方拥有。
- 未编译 `MDS_HAS_SIMDJSON` 时总是返回 false 并给出明确错误。
- 小数转换要求不超过配置 scale；超出 scale 的尾数只能全为 0。当前空字符串、仅负号、仅小数点等边界可能被转换函数接受为 0，公网校验 Gate 必须覆盖，调用方不能假设 parser 已完成所有词法验证。
- parse 失败可能已部分修改输出；失败后应丢弃整个输出对象。

### 4.3 `SpotSbeDecoder`

基于 Binance 官方 [`stream_1_0.xml`](https://github.com/binance/binance-spot-api-docs/blob/master/sbe/schemas/stream_1_0.xml)，`availability=Available`，常量为：

- `schema_id=1`、`schema_version=0`；
- `best_bid_ask_template_id=10001`；
- `depth_snapshot_template_id=10002`（只有常量，当前没有 snapshot decode API）；
- `depth_diff_template_id=10003`。

```cpp
bool decode_book_ticker(span<const byte>, BookTicker&, string& error)
    const noexcept;
bool decode_depth(span<const byte>, DepthUpdate&, string& error) const;
```

decoder 显式读取 little-endian SBE message header，不用 native struct overlay。schema/version/template 必须精确匹配；BestBidAsk root blockLength 至少 50，DepthDiff root 至少 26，depth group blockLength 至少 16，并严格检查消息/组/symbol 边界。更大的 blockLength 被作为 SBE 扩展字段跳过，并非要求与当前最小值完全相等。负 timestamp/update ID、`U>u`、超过每侧 5000 档或截断数据均拒绝。

BestBidAsk 输出 update ID、BBO、固定 symbol、price/quantity exponent，并把官方微秒 event time 转成毫秒；DepthDiff 输出 `U/u`、两侧 levels、固定 symbol 与 exponent，`pu=0`。两类 SBE BBO/depth 都可在 decode 后直接按 `out.symbol` 安全路由多标的。当前只证明离线 decoder 和 fixture 测试可用；SBE endpoint 建连、API Key header、REST 桥接、daemon 和 24h 公网运行仍未实现。

### 4.4 `DepthSynchronizer`

```cpp
explicit DepthSynchronizer(Profile, size_t max_buffered_updates=4096);
void reset() noexcept;
void inject_snapshot(uint64_t last_update_id) noexcept;
using ApplyBuffered =
    bool (*)(void* context, const DepthUpdate& update) noexcept;
SyncAction drain_buffered(void* context, ApplyBuffered apply) noexcept;
SyncAction on_update(const DepthUpdate&) noexcept;
BookSyncState state() const noexcept;
uint64_t last_update_id() const noexcept;
size_t buffered_updates() const noexcept;
```

- 对象拥有 buffered updates 的副本；`on_update` 在 WaitingSnapshot 会复制 vector，可能分配。
- 非线程安全，限定单 book 线程。
- action：Buffer/Drop/Apply/BecameLive/Resnapshot。
- `inject_snapshot` 只注入 ID 并把状态置为 Bridging，绝不直接置 Live，也不消费缓存。
- snapshot 档位装入外部 OrderBook 后，调用方必须调用 `drain_buffered(context, callback)`；synchronizer 依次丢弃 stale update、验证首条 bridge 与后续 continuity，并只在每条有效缓存 update 的 callback 都返回 true 后推进 `last_update_id`。至少成功应用一条且缓存排空时返回 BecameLive 并置 Live。
- 缓存为空或全部 stale 时返回 Buffer 并保持 Bridging；callback 为空、序列非法/gap 或 callback 返回 false 时清空缓存、置 Invalid、返回 Resnapshot。callback 为裸函数指针且 `noexcept`，上下文/OrderBook 所有权由调用方管理。
- 有 snapshot 前已缓存数据时，禁止绕过 `drain_buffered` 直接把后续 update 送入 `on_update`；正确编排必须先把缓存实际应用，避免快照后伪 Live。

### 4.5 `ConnectionHealth`

记录 ping/pong 时间；`pong_due(now,timeout)` 判断最近 ping 尚无后续 pong且超时；`rotation_due` 固定 23h50m。调用方提供同一单调毫秒时钟。非线程安全。没有网络发送逻辑。

## 5. 共享环 `mds::transport`

### 5.1 `SharedRing`

```cpp
static Result<SharedRing> open(const RingOptions&);
Result<uint64_t> publish(uint32_t type, span<const byte>) noexcept;
Result<ReaderHandle> register_reader(uint64_t start_marker, uint64_t now_ns) noexcept;
Result<void> unregister_reader(ReaderHandle) noexcept;
Result<void> heartbeat(ReaderHandle, uint64_t now_ns) noexcept;
Result<ReadLease> read(ReaderHandle&) noexcept;
size_t reclaim_stale(uint64_t now_ns, uint64_t timeout_ns,
                     bool (*process_alive)(uint32_t,uint64_t)) noexcept;
uint64_t epoch() const noexcept;
string_view name() const noexcept;
```

- move-only RAII；析构 unmap/close，创建者且 `unlink_on_close=true` 时 unlink。
- `open(create=true)` 使用 `O_EXCL`，不会接管已有同名段。
- `RingOptions` 默认值与 `ShmConfig`基本对应，另有 `name`、`create=true`、`unlink_on_close=false`。
- `publish` 为严格单生产者，不等待；payload 在返回前复制，调用方继续拥有。
- ring schema major=3；`publish` 对有效 payload 计算 Castagnoli CRC32C 并存于外层 header offset 12，`read` 在暴露 `RecordView` 前重新计算。CRC 不匹配返回 `InternalError`，reader cursor 不推进。
- reader registry 支持最多 64 个跨进程 reader；每个 handle 应由一个串行消费流使用。
- 同一个 `SharedRing` C++ 对象没有内部 mutex；除了设计中的跨进程原子字段，不应从多个本地线程并发调用非约定操作。
- attach 后的 `RecordView::payload` 是共享映射借用视图，只在 lease 活跃期间有效。

`ReadLease` move-only；`commit()` 推进 reader cursor 并使 lease 失效，`release()`/析构不推进。`view()` 返回内部引用。不要在 owner `SharedRing` 析构后保留 lease；类型系统未共享 owner 生命周期。

辅助函数：

```cpp
string make_segment_name(exchange, product, symbol, stream,
                         uint16_t schema_major, uint32_t depth=0);
uint64_t process_start_marker(uint32_t pid) noexcept;
bool process_identity_alive(uint32_t pid, uint64_t marker) noexcept;
```

segment name 对非 `[A-Za-z0-9_-]` 字符替换为 `_`，长度 >240 返回空。进程 identity 依赖 Linux `/proc` 与 `kill(pid,0)`。

## 6. 网络 API

### 6.1 `EpollLoop`

```cpp
explicit EpollLoop(int max_events=128);
bool add(int fd, uint32_t events, Callback);
bool modify(int fd, uint32_t events);
bool remove(int fd);
int run_once(int timeout_ms);
void run();
void stop() noexcept;
int native_handle() const noexcept;
static bool set_nonblocking(int fd) noexcept;
```

不可复制。loop 拥有 callback 容器但不拥有注册的业务 fd；析构仅负责自身 epoll/wake fd。`stop()` 设计为可跨线程唤醒；add/modify/remove 与 run 的并发修改契约未在头文件保证，使用方应限定到 loop 线程或加外部同步。callback 在 `run_once` 调用栈执行。

### 6.2 TLS/WebSocket

`make_client_ssl_context(error)` 返回共享所有权 `shared_ptr<SSL_CTX>`；`TlsSession` 保存一份共享引用并拥有其 `SSL*`，不拥有传入 socket fd。TlsSession 不可复制、未声明 move，单线程事件循环使用。

```cpp
int handshake() noexcept;
int read_some(span<const byte>& data) noexcept;
int write(span<const byte> data) noexcept;
int wanted_events(int ssl_result) const noexcept;
```

`read_some` 输出 span 借用内部 receive buffer，只到下一次读/析构前有效。返回值是 OpenSSL 风格结果，调用方必须结合 `wanted_events` 处理 WANT_READ/WANT_WRITE。

`WebSocketParser(capacity=1MiB)` 拥有接收与重组 buffer，`feed` 同步调用 callback；`WsFrameView.payload` 仅 callback 期间有效。支持 continuation/text/binary/close/ping/pong，拒绝压缩 RSV、reserved opcode 和非法长度。parser 非线程安全。

## 7. 冗余 API

`SequenceArbiter(max_gap=1024)`：

```cpp
using DecisionSink =
    bool (*)(void* context, const ArbiterDecision& decision) noexcept;
explicit SequenceArbiter(size_t max_gap=1024);
void reset(uint64_t next_sequence) noexcept;
bool submit_each(NativeEvent, void* context, DecisionSink) noexcept;
vector<ArbiterDecision> submit(NativeEvent);
uint64_t next_sequence() const noexcept;
bool divergent() const noexcept;
```

非线程安全。构造函数把 `max_gap=0` 规范为 1，并一次性分配：

- `max_gap+1` 个 `PendingSlot`；
- `max_gap*2+1` 个 `HistorySlot`。

两组 vector 在对象生命周期内不扩容；按 `sequence % slot_count` 作为固定槽 ring。`reset()` 只清零已分配槽，不重新分配。pending 槽发生不同 sequence 冲突时进入 divergent 并发出 Resync；history 槽被较新 sequence 覆盖后，更早事件无法命中历史，会判为 Regression。

`submit_each` 是无分配热路径：用 `DecisionSink(context, decision)` 同步发出一个或多个决策，sink 必须是 `noexcept` 裸函数指针；sink 为空时返回 false，sink 返回 false 时立即停止并返回 false，调用方不能假设状态回滚。调用方应使用固定容量/预分配 context。

`submit()` 是控制面/测试便利 API，每次调用创建结果 vector 并 `reserve(max_gap+2)`，内部转调 `submit_each`；不得用于行情热路径。`payload_hash` 算法仍由调用方负责且没有碰撞处理。action 为 Publish/Duplicate/BufferedGap/Divergence/Regression/Resync。

## 8. `utils::md`

### 8.1 symbol

`SymbolNormalizer` 可增加 alias、规范化 venue symbol、生成 instrument key。修改 alias 与并发 Normalize 不安全；完成配置后，多线程只读的安全性没有文档保证，建议外部冻结并同步。

`InstrumentRegistry::Register` 返回 Ok/Invalid/DuplicateId/DuplicateKey；`Find(id/key)` 返回借用 `const Instrument*`。底层 `deque` 让已有元素地址在后续 push 时保持稳定，测试覆盖了这一点；registry 析构、并发写或对象移动后 pointer 失效。无内部同步。

### 8.2 ladder/order book

`Ladder(side,capacity)` 最大静态容量 4096，并把直接传入的 capacity clamp 到 `[1,4096]`；`OrderBook` 与公共 `OrderBookSubscription` 现在都默认每侧 4096。`suborderbook` 在 service 边界拒绝 0 或 >4096，因此经公共 API 不会发生超上限静默截断。

`Reset/Apply/ChangeTickSize/SetLive` 修改对象；`Best/At/Bbo/state` 读取。对象非线程安全，单 book 线程拥有。`Bbo` 返回值副本。`LadderResult`：Ok/InvalidPrice/OutOfWindow/NeedsRestart。

### 8.3 进程内队列

`SpscRing<T,Capacity>` 是已安装 SDK 的公开 header-only 基础组件，要求 Capacity 为 ≥2 的 2 次幂，严格单 producer/单 consumer。lease move-only；producer lease 析构会取消并销毁未提交对象，consumer lease 析构会消费并销毁对象。任一侧同一时刻只允许一个活跃 lease。它只用于进程内传递，不能替代跨进程 `SharedRing`。

`BoundedMpscQueue<T,Capacity>` 为多 producer、单 consumer；`try_emplace` 要求 nothrow construct，`try_dequeue` 要求 nothrow move-assign。无等待、满/空返回 false。队列析构时不得仍有并发访问。

## 9. `utils::runtime`

### 9.1 Timestamp

`Timestamp::NowTSC/NowMono/Detect/Calibrate/CyclesToNs/TscToMonoNs` 均静态、无全局可变状态，返回值拥有。TSC unsupported 时 sample cycles 可为 0；调用者必须检查 `TscCapability.calibrated/state`，不能仅根据非零值判断可比较。

`TscCalibrationState`：Unsupported, Detected, InvalidInterval, ClockUnavailable, CpuMigration, NonMonotonic, Calibrated（从 0 顺序）。

### 9.2 Hardware

`utils/runtime/hardware.h` 是树内实现接口，不属于已安装 SDK。`DetectHardware(sys_root="/sys")` 返回拓扑副本；`MakeAutomaticPlan` 和 `ValidateManualPlan` 返回 assignment/warnings/valid。`BindCpu`/`BindCurrentThread` 返回 pthread/scheduler 错误码整数，不包装为 `mds::api::ErrorCode`。调用方拥有所有返回容器。

段命名、共享内存布局与 wire 版本的唯一消费方契约见
[`segment_and_schema_contract.md`](segment_and_schema_contract.md)。

## 10. `oms::api::ExecutionChannel`

`ExecutionChannel` 是 StrategyFrame 面向的 owning facade；调用方不接触
adapter 或 transport 实现。当前公共入口为：

```cpp
static Result<std::unique_ptr<ExecutionChannel>> Create(
    const RuntimeConfig&, std::span<const InstrumentInit>,
    std::span<const ReplayStep> replay = {});
Result<void> initialize_lane(uint32_t lane_id, uint32_t session_epoch);
Result<RequestToken> place_order(uint32_t lane_id, NewOrderRequest);
Result<RequestToken> cancel_order(uint32_t lane_id, RequestToken target,
                                  OrderHandle handle = {});
Error service_io(int timeout_ms);
size_t drain_updates(uint32_t lane_id, UpdateCallback, void* context,
                     size_t maximum = SIZE_MAX);
int notification_fd(uint32_t lane_id) const;
RuntimeMetrics metrics() const;
Result<AdapterStatusSnapshot> venue_status(AdapterKind) const;
Error reconcile(AdapterKind);
Error shutdown();
```

- Inline 模式由创建 channel 的线程拥有状态与 I/O；只有该线程调用
  `service_io`。DedicatedIo 模式由内部 worker 拥有状态与 I/O，策略线程不调用
  `service_io`，而是等待每 lane 的 `notification_fd`。
- 两种模式下，update callback 都由 `drain_updates` 同步调用，运行在执行 drain
  的 StrategyFrame 线程，不运行在 DedicatedIo worker。
- 每个 lane 在使用前以非零、进程内唯一的 session epoch 初始化。提交和取消只
  表示命令已接受/排队；最终状态必须从 update 流确认。
- shutdown 顺序是停止新提交、drain 已发布 update、调用 `shutdown`，随后不再
  使用 notification fd 或 callback context。
- Polymarket `InstrumentInit::polymarket_signature_type` 只接受 type 0
  （EOA）与 type 3（browser/proxy wallet）；type 1 在公共构造边界被拒绝。
- 可编译集成示例见
  [`../oms/examples/strategy_frame_execution.cpp`](../oms/examples/strategy_frame_execution.cpp)。
  `oms_benchmark` 的 fake-adapter 结果是 offline loopback 回归数据，不是生产
  SLO 或外部 venue acceptance。
- 当前无授权 credentials 的外部 Binance/Polymarket acceptance 状态为
  **PENDING**；离线 example/benchmark 通过不能改变该状态。

## 11. `strategyframe`

### 11.1 Strategy Concept 与 Runner

`strategyframe::Strategy<T>` 要求策略提供八个返回 `void` 的回调：
`init(StrategyContext&)`、BBO、orderbook、aggregate BBO、aggregate
orderbook、order update、OMS status 和 timer。当前没有 optional callback；
不使用的回调必须提供空实现。

`StrategyRunner<T>` 不可复制且只能 `run()` 一次。所有策略回调都在调用
`run()` 的 owner 线程同步串行执行；即使 OMS 使用 `multi_io_thread`，I/O
worker 也不会执行策略回调。回调抛异常会被边界捕获，计入
`callback_failures`，并使运行返回 `Error::CallbackFailed`。
`request_stop()` 可跨线程调用，也可在 `run()` 前调用；runner 的
`metrics()` 是上一次已结束运行的快照，不是 live metrics view。

### 11.2 `StrategyContext`

当前公开操作：

```cpp
Result<OrderToken> place_order(const OrderRequest&) noexcept;
Result<OrderToken> cancel(OrderToken) noexcept;
Result<TimerHandle> schedule_timer(uint64_t deadline_ns,
                                   uint64_t interval_ns = 0) noexcept;
Error cancel_timer(TimerHandle) noexcept;
std::span<const OrderView> open_orders() const noexcept;
std::span<const PositionView> positions() const noexcept;
Result<OrderView> find_order(OrderToken) const noexcept;
Result<PositionView> find_position(AccountId, InstrumentId,
                                   PositionSide = PositionSide::Net) const noexcept;
const StrategyParams& params() const noexcept;
RuntimeMetrics metrics() const noexcept;
uint64_t now_ns() const noexcept;
void request_stop() noexcept;
```

- place/cancel 成功只表示 bounded OMS command 被接受，不表示 venue 已接受；
  最终状态以 update callback 为准。
- place/cancel/timer mutation 只允许 owner 线程；否则返回 `InvalidThread`。
- 当前固定使用 lane 1；`OrderToken` 为 lane/session epoch/sequence 三元组。
- client order ID 在 context 边界最多 64 字节。
- `now_ns()` 是 steady/monotonic 时间，不是 wall clock 或 venue time。
- span/string_view 均为借用视图；manager 更新或下一次 callback 后不得继续使用。
- context 仅在 runtime 存活期间有效，`run()` 返回后不得使用。

### 11.3 Manager 语义

`OrderManager` 和 `PositionManager` 在构造时按固定容量分配 dense vector 与
open-addressing index，容量不增长。Order manager 在 OMS 接受 place command
后先插入 `PendingSubmit`，保存 Open/PartiallyFilled，terminal 状态会从
open-order dense span 删除。Position manager 当前只由 fill 更新
`PositionSide::Net`。

`AccountManager` 当前只是 Order/Position manager 的非 owning 聚合，不提供
balance、available funds、margin、collateral、fee、funding、realized/unrealized
PnL 或 risk limit，也没有单独暴露在 `StrategyContext`。position table 满或
quantity rescale overflow 目前不会可靠地作为策略错误上报，因此必须保守配置
容量并在策略/外围做校验。

### 11.4 YAML 与 MDS 边界

`load_config(path)` 只接受 root 的 `mds`、`oms`、`strategy`。typed MDS/OMS
map 的未知字段始终拒绝；`oms.strictness` 当前不会改变这一行为。解析异常统一
折叠为 `InvalidConfig`。

- `mds.source=external_shm`：至少一个 segment；attach 时从 writer 当前 cursor
  开始，不回放历史。每个 segment 占一个 reader slot。
  `expected_reader_budget` 是 StrategyFrame attach 前的 active-reader admission
  上限，不是 slot reservation；`ring_bytes/max_record_bytes` 必须与 segment
  header 一致。
- `mds.source=self_hosted`：要求 `producer.config_path`，并直接复用 standalone
  `mds_producer` 的完整 YAML schema 与共享 `ProducerRuntime`。Polymarket rolling
  与 Crypto 因而走同一 discovery/config/connection 路径，不维护第二套能力残缺
  的 venues/subscriptions schema。
- `mds.source=replay`：YAML 可选择，但公开 `StrategyRunner` 尚无 replay record
  注入 API。

若外部 segment 是 `Lossless`，StrategyFrame reader 会参与 producer
backpressure；slow/initializing/suspect reader 会让 producer `publish()` 返回
`QuotaExceeded`，不会在 ring 内等待。overwrite ring 发生 overrun 时，
StrategyFrame 清本地 book、把 reader resync 到 latest 并增加
`out_of_order_updates`；当前不会向策略暴露独立 gap callback。

`oms.instruments` 虽被解析，当前 runtime 不用它 seed OMS；execution registry
只从 MDS instrument metadata 建立。`strategy` map 支持 scalar/nested map，
嵌套 key 以点号 flatten 后由 typed `require_*` 查询，不支持 sequence。

### 11.5 Threading 与 live execution

`single_thread` 映射 OMS Inline：StrategyFrame owner 线程执行 MDS poll、
`service_io(0)`、drain、timer 与 callback。Inline 的 owner constraint 是硬
约束。`multi_io_thread` 映射 DedicatedIo：内部 worker 拥有 OMS state/socket
I/O，但 StrategyFrame owner 仍负责 drain 和所有 callback。当前两种模式都只有
一个策略线程和一个 lane。

Binance Spot/USD-M place/cancel 严格使用 dedicated trading WebSocket
(`use_trading_websocket=true`)；session 未 ready 时不降级 REST order entry。
Polymarket 是明确例外：place/cancel/reconcile 使用 signed HTTPS REST，认证
WebSocket 用于 user/session event 与 liveness。

adapter reconnect/uncertain request 可触发 reconciliation，
`VenueStatus/ReconcileComplete` 通过 `on_oms_status` 发布。StrategyContext
当前没有公开 `ExecutionChannel::reconcile()` 或 `venue_status()`，所以 facade
内不能手工触发 reconcile。

### 11.6 HFT 与未实现控制

manager、timer 和 OMS queues 采用预设 bounded storage，market update 以借用
view 传入，但不能宣称整个 runtime zero-allocation：startup/config/TLS、
instrument discovery、map/vector/string 与用户 callback 都可能分配。

`strategy_cpu/io_cpu` 会转发到 OMS runtime 设置；StrategyFrame 本身不绑定其
MDS/callback owner thread。`numa_node`、`strictness`、memory/socket 设置、
`market_data_queue`、`metric_sample_rate` 当前只解析不执行。HFT profile 必须
在目标环境实测，不能从 busy-spin 或固定容量推导 latency SLO。

balance/margin/PnL/risk controls、durable restart recovery 与 account snapshot
API 均为 deferred。example YAML 的 `max_position` 只是 strategy parameter，
framework 不会自动执行。

构建必须同时启用 MDS/OMS/StrategyFrame，并安装 yaml-cpp：

```bash
cmake -S . -B build-strategyframe -G Ninja \
  -DSELF_QUANT_ENABLE_MDS=ON \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON
cmake --build build-strategyframe -j2
ctest --test-dir build-strategyframe --output-on-failure
```

安装包导出 `self_quant::strategyframe` 与 `self_quant::strategyframe_config`。
当前外部 Binance/Polymarket acceptance 为 **PENDING**；offline
fixture/replay/package/benchmark 不得改写该状态。

## 12. 生命周期与所有权总则

- `string_view`/`span` 参数默认仅在调用期间借用，除非上文明确复制。
- 返回的 `string_view name()` 借用 `SharedRing` 内部字符串。
- 所有 parser、book、synchronizer、arbiter、registry 对象默认单线程拥有。
- 全局 `mds::api` 入口 mutex-safe，但当前没有运行中的 worker；未来增加 worker 后必须重新审查 shutdown 与 query 的同步契约。
- CMake 安装包已导出静态库 targets，但没有跨版本 ABI 稳定承诺或 symbol
  visibility/versioning 契约；版本升级应按源码/头文件兼容性重新验证。
