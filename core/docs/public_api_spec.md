# `utils` / `mds` 公共 API 规范（当前实现）

## 1. 适用范围

本文以 `utils/include/**` 与 `mds/include/**` 当前头文件及实现为准。公开头文件能编译不等于功能已端到端接通：当前 `mds::api` 完成配置校验、TLS context/epoll 初始化和订阅句柄状态管理，但订阅不会建连，状态只会保持 `Pending`，直到 `unsubscribe()`/`shutdown()`。

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
| 4 | `UnsupportedVenueProduct` | 非 Binance Spot/Perpetual、未配置 profile、请求不可用的 USD-M SBE |
| 5 | `InstrumentNotFound` | 当前 API 实现未返回 |
| 6 | `ShmCreateFailed` | shm/open/ftruncate/mmap/stat 失败 |
| 7 | `HugepageUnavailable` | hugetlbfs 打开失败（即使根因可能是其他 `errno`） |
| 8 | `TscUnstable` | 要求 stable TSC 但 CPUID 不支持 invariant TSC |
| 9 | `CpuBindFailed` | 当前 `mds::api` 未实际绑核，未返回 |
| 10 | `QuotaExceeded` | 订阅/reader 满、无记录、记录未提交、慢 reader 背压 |
| 11 | `SubscriptionRejected` | ring reader overrun；当前订阅控制面未返回 |
| 12 | `InvalidHandle` | 未知订阅/reader/lease、stale cursor |
| 13 | `InternalError` | epoll/TLS 初始化失败或 ring 元数据损坏 |

枚举是源码 API，目前没有稳定 C ABI；新增中间值会改变后续数值，冻结 wire 前应显式赋值。

## 3. `mds::api`

### 3.1 配置枚举

- `ProductType` 是 `using ProductType = utils::md::ProductType`，不再是独立枚举；完整值为 Unknown=0、Spot=1、Perpetual=2、Future=3、BinaryOption=4、Equity=5。当前 service 配置仍只接受 Binance Spot/Perpetual。
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

当前 service 不创建 `SharedRing`，所以这些参数仅被部分校验和保存。

`VenueProfile`

- `venue="binance"`
- `product=Spot`
- `websocket_endpoint=""`, `rest_endpoint=""`：当前不消费；空值不会自动写回 capability host；
- `protocol=Json`
- `max_streams_per_connection=200`
- `messages_per_second=5`
- `redundant_ab=false`
- `allow_json_fallback=false`

只允许每个 product 一个 Binance profile；venue 必须精确小写 `"binance"`。limits 必须非零。Spot SBE capability 为 Available，USD-M SBE 会被拒绝；`allow_json_fallback` 当前仍没有自动 fallback 行为。

`MdsConfig` 包含 `runtime`、`shm`、`venues` 和 `max_subscriptions=4096`。

`TickerSubscription`：venue=`"binance"`、product=`Spot`、`symbol` 必填。

`OrderBookSubscription : TickerSubscription`：

- `depth=1000`
- `ladder_levels_per_side=4096`（`utils::md::kMaxLadderLevels`）
- `slow_consumer=Latest`
- `update_interval_ms=100`

当前要求 depth 非零，ladder 必须在 `[1,4096]`；超过 `utils::md::kMaxLadderLevels` 直接返回 `InvalidConfig`，不再依赖底层 Ladder 的静默 clamp。尚未验证 Binance depth 档位、更新间隔合法集合，也没有实际应用 slow-consumer policy。

### 3.3 服务函数

```cpp
Result<void> init(const MdsConfig&);
Result<SubscriptionHandle> subticker(const TickerSubscription&);
Result<SubscriptionHandle> suborderbook(const OrderBookSubscription&);
Result<void> unsubscribe(SubscriptionHandle);
SubscriptionState query_state(SubscriptionHandle) noexcept;
void shutdown() noexcept;
```

- 全局单例进程生命周期；不可创建多个 service instance。
- 所有函数通过同一 mutex 串行，因此这些入口可由多线程调用。
- `init` 成功时创建 epoll loop 和 OpenSSL client context，但不启动线程。
- handle 从 1 递增，`shutdown()` 后重置为 1；旧 handle 不能跨 init 周期使用，但因为没有 generation，数值可能复用。
- `unsubscribe` 把状态设为 Stopped，不删除 map 项，也不释放网络资源（当前没有网络资源）。
- `query_state` 在未初始化或未知 handle 时都返回 Unknown，无法区分原因。
- `shutdown` 可重复调用，清空订阅并销毁 TLS/epoll；持有的配置字符串由 service 自己复制。

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

## 10. 生命周期与所有权总则

- `string_view`/`span` 参数默认仅在调用期间借用，除非上文明确复制。
- 返回的 `string_view name()` 借用 `SharedRing` 内部字符串。
- 所有 parser、book、synchronizer、arbiter、registry 对象默认单线程拥有。
- 全局 `mds::api` 入口 mutex-safe，但当前没有运行中的 worker；未来增加 worker 后必须重新审查 shutdown 与 query 的同步契约。
- 公共 API 没有 ABI visibility/versioning 或安装导出规则；当前适用于同一源码树静态链接，不应宣称稳定二进制 SDK。
