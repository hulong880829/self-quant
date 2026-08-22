# 行情服务一期需求

## 1. 文档状态与边界

本文定义 `self-quant/core` 行情服务（MDS）一期验收目标。目标与当前实现必须区分：

- **当前可用基础**：C++20 数据类型与 wire ABI、定长订单簿、无锁进程内队列、带 payload CRC32C 的 POSIX SHM/hugetlbfs 共享环、TLS/WebSocket 帧解析、Binance JSON 字段解析、Binance Spot 官方 `stream_1_0.xml` SBE 的 BestBidAsk/DepthDiff 解码、可回调应用缓存的 Spot/USD-M 深度序列同步器、构造期预分配且支持无分配 `submit_each` 热路径的 A/B 序列仲裁器、硬件与 TSC 工具、单元测试。
- **当前尚未闭环**：`mds::api::subticker/suborderbook` 只创建 `Pending` 句柄；没有 DNS/TCP/WebSocket 握手、订阅发送、REST 快照请求、订单簿到 wire 的发布编排、指标导出、回放工具或生产进程入口。因此本文件中的公网、SLO 和 24h/7d 条目均为上线 Gate，不是已达成声明。

## 2. 一期范围

### 2.1 纳入范围

1. 交易所仅 Binance。
2. 产品仅：
   - Spot 现货；
   - USDⓈ-M Futures（公共 API 中映射为 `ProductType::Perpetual`，适用于永续；交割合约虽在底层 `utils::md::ProductType::Future` 有枚举，但不属于当前 `mds::api` 一期）。
3. 公共市场数据：
   - 最优买卖 `bookTicker`；
   - 增量深度 `depth`；
   - REST 深度快照；
   - 规范化 instrument 元数据；
   - 本地订单簿、BBO、增量和快照经共享内存发布。
4. JSON 协议，以及 Binance Spot 官方 `stream_1_0.xml`（schema id=1、version=0）的 SBE BestBidAsk/DepthDiff 解码。USD-M SBE 不在当前实现范围。
5. Linux/x86_64 为主要部署目标；wire ABI 明确只允许 little-endian。

### 2.2 明确不纳入

- 下单、撤单、订单管理、账户、持仓、余额、风控、私有 user-data stream；
- 策略、撮合、回测收益计算；
- 非 Binance 适配器；
- 期权、COIN-M、杠杆账户、交割合约；
- 在没有测量证据时对延迟、吞吐或可用性作生产承诺。

## 3. 产品矩阵

| 产品 | `mds::api::ProductType` | WS 主机 | REST 主机 | BBO | Diff depth | 快照 | JSON | SBE |
|---|---:|---|---|---|---|---|---|---|
| Binance Spot | `Spot=1` | JSON: `stream.binance.com`；SBE: `stream-sbe.binance.com` | `api.binance.com` | 一期 | 一期 | `/api/v3/depth` | 一期 | decoder 当前 `Available`；公网编排未闭环 |
| Binance USD-M 永续 | `Perpetual=2` | `fstream.binance.com` | `fapi.binance.com` | 一期 | 一期 | `/fapi/v1/depth` | 一期 | 当前 `Unavailable` |

默认首个验收标的是 `BTCUSDT`。扩展到其他标的必须先从 exchange info 获取并固定价格/数量 scale、tick size、lot size，禁止根据单条行情猜测精度。

## 4. 功能需求

### 4.1 生命周期与配置

- `init()` 必须验证 venue/product、共享环几何、CPU/NUMA、TSC 要求和协议可用性；重复初始化返回 `AlreadyInitialized`。
- `subticker()`/`suborderbook()` 必须异步建连并驱动状态 `Pending → Live` 或 `Failed`。当前实现只停留在 `Pending`，上线前必须补齐。
- `unsubscribe()` 幂等策略需在实现前冻结；当前行为是未知句柄返回 `InvalidHandle`，有效句柄改为 `Stopped` 但不删除。
- `shutdown()` 必须停止新订阅、退出事件循环、关闭网络与 SHM，并保证不再回调或访问已释放内存。

### 4.2 深度正确性

- 先建立 WS 并缓存增量，再取 REST 快照，再按官方规则桥接。
- 注入 snapshot ID 后状态只能是 Bridging；必须通过 `drain_buffered` callback 将每条有效缓存增量真正应用到外部订单簿，成功后才能进入 Live，禁止仅推进序列号伪 Live。
- Spot：丢弃 `u <= lastUpdateId`；首个有效事件必须覆盖 `lastUpdateId + 1`，在线阶段每个事件必须覆盖下一个期望 ID。
- USD-M：桥接事件覆盖快照 `lastUpdateId`；Live 后强制 `pu == 前一事件 u`。
- 发现倒退、缺口、缓存溢出、非法 `U > u`、scale/tick 变化或 reader overrun 时，不得继续发布 `Live` 订单簿；进入 Invalid/NeedsRestart 并重新快照。
- 数量为零表示删除档位。价格、数量统一转为带外 scale 对应的 `int64_t` mantissa；超精度非零尾数、溢出或非法十进制必须拒绝。

### 4.3 发布

- 共享环为单生产者、多 reader registry；慢 reader 采用背压，不覆盖未消费记录。
- market-data payload 使用 `utils::md::wire` schema 2.0（magic `0x444d5153`，内存字节 `"SQMD"`）；共享环外层使用 ring schema major 5，并对每条正常记录的有效 payload 计算/验证 Castagnoli CRC32C。二者是独立版本域，消费方规则以 `segment_and_schema_contract.md` 为准。
- `bus_seq` 在发布总线上单调；`source_seq` 保存交易所更新号；`book_generation` 每次重建递增。
- 快照超过单记录上限时必须按 Begin/Chunk/End 分片；每个 Chunk 最多 24 档。
- `mds::api::ProductType` 与 `utils::md::ProductType` 是同一类型 alias；订单簿 ladder 默认 4096 档/侧，公共订阅 API 拒绝 0 或超过 4096 的配置。

## 5. SLO（待实测 Gate）

以下均是目标，当前仓库没有生产测量结果：

- 正确性：在无交易所缺失且无本地资源耗尽时，已发布 Live 深度与同一时间点 REST 逐档比对零差异；任何无法证明连续性的状态不得标记 Live。
- 可用性：计划内维护除外，单实例月可用性目标 ≥ 99.9%；A/B 冗余启用后的目标需另行测量和评审。
- 恢复：连接断开后 1 s 内开始重连；网络恢复后 30 s 内完成快照重建并恢复 Live（受 Binance 限频时允许延长并告警）。
- 数据新鲜度：`now - exchange_ts` p99 < 1 s；超过 3 s 告警并禁止静默维持 Live。
- 内部阶段延迟（目标硬件、稳定 TSC）：receive→parse、parse→book、book→publish 分别记录 p50/p95/p99/p99.9；一期发布 Gate 建议端到端 receive→publish p99 < 1 ms，但在基准完成前不视为承诺。
- 丢失：因本地 bug 导致的已接收连续事件丢失为 0；资源耗尽必须显式计数、状态降级并重建。

## 6. 非功能需求

- **确定性**：同一元数据、快照和增量输入必须产生字节一致的规范化输出（时间戳字段可在回放模式固定）。
- **可观测性**：所有连接、解析、序列、重快照、队列、SHM、reader、延迟状态可量化；日志含 venue/product/symbol/connection/generation。
- **无隐藏分配热路径**：连接建立后，网络接收、解析、订单簿更新和发布路径不得发生未界定的堆分配。`SequenceArbiter` 必须在构造期预分配 pending/history 固定槽，并在热路径使用 `submit_each(context, callback)`；返回 `std::vector` 的 `submit()` 只允许控制面/测试使用。当前 `DepthUpdate` 仍使用 `std::vector`，需通过预留容量或替代结构验证。
- **资源有界**：订阅数、连接流数、深度缓存（当前默认 4096）、WebSocket 消息容量（默认 1 MiB）、ring/record/reader 数均有上限。
- **安全**：强制 TLS 证书和主机名验证；拒绝协商 `permessage-deflate`。JSON 公共行情无需密钥；Spot SBE 官方 endpoint 要求 Ed25519 API Key 通过 WebSocket upgrade 的 `X-MBX-APIKEY` header 提交，禁止日志泄露并按 secret 管理。
- **可移植性**：共享内存结构依赖 lock-free 原子和 little-endian；不满足时启动失败，不做未验证降级。
- **兼容性**：消费者启动时校验 magic/schema/layout；不允许按 C++ 头文件猜测不匹配的共享段。

## 7. Binance 限频与连接管理

- Spot WS 基准规则：单连接最多 1024 streams；每秒最多 5 条进入服务端的消息（PING/PONG/JSON control 均计入）；单 IP 每 5 分钟最多 300 次连接尝试；连接最长 24h。
- USD-M WS 基准规则：单连接最多 1024 streams；每秒最多 10 条进入服务端的消息；连接最长 24h。
- 配置默认 `max_streams_per_connection=200`、`messages_per_second=5`，因此 USD-M 默认值比官方上限更保守。
- 控制面必须使用 token bucket，订阅/退订/心跳共享预算；收到 429/断连时指数退避并加 jitter，禁止重连风暴。
- 当前 `ConnectionHealth` 在 23h50m 触发预轮换，但实际双连接平滑切换尚未实现。
- REST 快照请求必须按 endpoint 的 request weight 动态记账；不同 limit 权重以运行时官方文档为准，不得硬编码过期值后宣称合规。

## 8. 监控与告警

至少导出：

- 连接：状态、重连次数、连接年龄、ping/pong RTT、最后消息时间、限频退避；
- 数据：每 stream 消息率、解析失败、字段/精度错误、`U/u/pu` 缺口/倒退/重复、缓存水位、重快照次数和原因；
- 订单簿：generation、state、档位数、crossed book、与 REST 差异档数；
- 发布：bus/source seq、记录类型/字节、publish 失败、`QuotaExceeded`、ring 使用率；
- reader：slot/state/pid/heartbeat age/cursor lag、回收数；
- 延迟：exchange→receive（时钟可比时）、receive→publish、各阶段直方图；
- 系统：线程 CPU、调度迁移、RSS、FD、NUMA、TSC calibration state。

P1：错误发布 Live 订单簿、持续序列缺口、所有连接不可用、ring 完全背压。P2：单连接降级、重快照频繁、延迟/新鲜度超 SLO、reader stale。告警必须含可操作 runbook 链接。

## 9. 上线 Gate

1. wire ABI 与公共 API 评审冻结，生成 offset/size 静态校验。
2. 当前单元测试在目标构建配置通过；不能以“可编译”代替测试。
3. BTCUSDT Spot 端到端公网切片通过，包含录制、确定性回放和 REST 逐档比对。
4. USD-M 同等序列/快照/心跳/24h 轮换测试通过。
5. 断网、乱序、重复、缺口、半初始化 SHM、慢 reader、进程崩溃、REST 429 故障测试通过。
6. 目标硬件 2×预估峰值负载性能测试通过且无未解释丢失。
7. 24h 单标的验收和 7 天多标的长稳通过；ASan/UBSan/TSan 在适用测试集无新增问题。
8. 仪表盘、告警、容量、回滚、版本兼容和 on-call runbook 评审完成。

## 10. 官方参考

- Spot WebSocket Streams：https://developers.binance.com/docs/binance-spot-api-docs/web-socket-streams
- Spot SBE Market Data Streams：https://github.com/binance/binance-spot-api-docs/blob/master/sbe-market-data-streams.md
- Spot SBE `stream_1_0.xml`：https://github.com/binance/binance-spot-api-docs/blob/master/sbe/schemas/stream_1_0.xml
- Spot REST Market Data / Order Book：https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints#order-book
- USD-M WebSocket Market Streams：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams
- USD-M Diff Book Depth：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams/Diff-Book-Depth-Streams
- USD-M Local Order Book：https://developers.binance.com/docs/derivatives/usds-margined-futures/websocket-market-streams/How-to-manage-a-local-order-book-correctly
- USD-M REST Order Book：https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Order-Book
