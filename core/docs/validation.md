# 验证计划与 Gate

## 1. 结果声明规则

- 测试源码存在只表示“有覆盖”，不表示“已运行/通过”。
- 每次结果必须绑定：UTC 时间、git commit 与 dirty 状态、主机/内核/CPU、编译器、CMake options、依赖版本、完整命令、退出码和日志 artifact。
- 未保留证据的条目状态只能是 `NOT RUN`；受环境阻断为 `BLOCKED`；不得写成 PASS。
- 公网测试必须保存官方 endpoint、HTTP/WS 状态、原始输入或 hash；地区封锁、DNS/TLS/429 是环境结果，不可改写为代码通过。
- 性能数字必须来自目标硬件 Release build；debug/sanitizer/虚拟机结果不能冒充生产 SLO。

本文初始清单默认 `NOT RUN`。若在同一变更中实际执行，结果只记录在“执行记录”中，且不推导尚未执行的 Gate。

## 2. 当前已有自动化测试

### 2.1 `self_quant_utils_tests`

源码：`utils/tests/utils_tests.cpp`。单一 executable，以 `require` 判定，当前覆盖：

1. wire：
   - `RecordHeader` size=64；
   - MakeHeader magic/schema/type/length；
   - InstrumentUpdate type/length；
   - schema minor 1 的 168-byte TickerRecord OHLC offset、Encode/Decode 往返与错误长度拒绝。
2. symbol：
   - XBT alias、分隔符和 BTCUSDT 规范化；
   - Spot/Perpetual instrument key 区分；
   - registry 查找、duplicate ID；
   - 注册到 4095 后早期 pointer 稳定。
3. ladder/order book：
   - bid/ask apply、bitmap word boundary、BBO；
   - quantity=0 删除；
   - out-of-window→Invalid；
   - tick size change→NeedsRestart。
4. queue：
   - SPSC lease reserve/cancel/commit/move/生命周期；
   - MPSC 两 producer、单 consumer；
   - non-default-constructible 对象析构；
   - throwing constructor 在编译期被约束。
5. timestamp/hardware：
   - monotonic time、TSC capability；
   - calibration 与 conversion；
   - invalid interval；
   - hardware discovery 与 automatic plan 基础行为。

明显未覆盖：所有 wire 字段 offset/零 padding/枚举值、fixed decimal、registry duplicate key/invalid、ladder 容量边界与随机模型比对、queue wrap-around 长稳/TSan、manual hardware plan/bind 错误。

### 2.2 `mds_tests`

源码：`mds/tests/mds_tests.cpp`。自定义 `require`，当前覆盖：

1. SharedRing：
   - create/register/publish/read/commit/heartbeat/unregister；
   - wrap/padding 路径的多轮读写；
   - outstanding/released lease 背压与重复读取；
   - stale reader 回收；
   - 半初始化段拒绝；
   - Initializing reader 阻塞 publisher 及崩溃回收；
   - schema/ring size/epoch/max_readers 损坏拒绝；
   - ring schema 4 payload CRC32C：发布后直接篡改共享内存 payload，read 返回 InternalError；
   - 默认 OverwriteOldest 不受停滞 reader 背压、Lossless 保持背压；
   - 套圈检测、`resync_to_latest()` 与 registry generation 通知；
   - 零拷贝租约被写者覆盖时 `commit()` 返回 `RecordOverwritten`。
2. SequenceArbiter：
   - `submit()` 控制/测试路径的 gap buffer/fill、duplicate、divergence；
   - `submit_each()` 配合固定 `std::array` context 的 callback 路径，验证单条 Publish 决策且不使用返回 vector。
3. Binance：
   - Spot buffer→snapshot 后保持 Bridging，`drain_buffered` callback 实际应用后才 Live，以及 continuation→gap；
   - USD-M bridge、`pu` continuation/mismatch；
   - buffer 上限；
   - 官方 `stream_1_0.xml` schema id=1/version=0 的 BestBidAsk template 10001 fixture；
   - JSON bookTicker/depth fixture 的 symbol、exponent 和定点数归一化；
   - SBE BestBidAsk/DepthDiff template 10001/10003 的固定 symbol、exponent、两侧 group fixture；
   - Spot SBE capability Available，以及错误 schema 拒绝。
4. WebSocket：
   - extension-free 与拒绝 permessage-deflate；
   - partial frame；
   - fragmented message + interleaved ping；
   - 拒绝 RSV1、reserved opcode、非法 64-bit length。
5. service：
   - Spot SBE profile init 成功；
   - BTCUSDT ticker subscription 保持 Pending，防止未就绪伪 Live。

明显未覆盖：CRC32C golden vectors、header/CRC 字段本身篡改矩阵；SBE 错误 version/template、过小/扩展 blockLength、组/符号截断和 5000 档边界的完整负例矩阵；真实 TLS/upgrade、SBE API Key header、binary/text routing、client masking/write、JSON decimal/非法 symbol 边界、combined stream、REST、真实 book apply、wire encode/decode、并发多进程 reader、hugetlbfs/fallback、24h rotation 实际切换、限频和所有公网行为。

CRC、JSON/SBE symbol/exponent 与 buffer drain 覆盖曾由父代理在 Debug/Werror、ASan+UBSan/Werror 和 Release/Werror 三种构建中分别执行，均为 2/2 tests passed。新增 `SequenceArbiter::submit_each` 固定 callback 覆盖后，最新实际确认的是 Debug/Werror 2/2 PASS；不得把较早的 ASan/Release 记录外推为已覆盖这项新增测试。

### 2.3 `oms_core_tests`

源码：`oms/tests/oms_core_tests.cpp`。使用抛异常的 `REQUIRE`，Release 的
`NDEBUG` 不会禁用检查。当前离线覆盖：

1. 公共 POD：固定容量 client/venue/trade ID、request token、64-bit generation
   order handle、request/event/order update/fill update 的
   trivially-copyable、standard-layout、size 和 enum ABI 契约；
2. immutable registry：底层 `utils::md::InstrumentRegistry`、duplicate/freeze、
   无交易 side metadata 时 untradeable，以及 Polymarket 256-bit
   condition/token、outcome、negative-risk、signature、minimum size 和 taker delay；
3. `OrderTable`：固定 slab/free-list/generation、request/client/venue 三类
   预分配 open-addressed index、容量、冲突、幂等 venue binding、erase 和 ABA；
4. state engine：本地 submit/reject、venue new ack/reject、early cancel、
   cancel ack/reject、expire、reconcile、fill-before-ack、terminal 后 late fill，
   以及 handle/token/client/venue 多标识一致性和 venue-only/client-only 回报；
5. fill：每笔独立 `FillUpdate`（含一次订单超过 4 笔成交）、fixed-capacity
   trade dedup、相同 duplicate 忽略、冲突 duplicate、overfill、scale 和
   精确累计 128-bit notional、checked weighted-average 边界和不同价格多笔成交；
6. 固定 seed 的确定性随机状态序列；
7. `oms/tests/fixtures/manifest/fixture_manifest.json` 的 schema、tier、Binance
   与 Polymarket 条目结构检查，不引入 JSON 依赖；
8. allocator instrumentation 下 submit/ack/fill/cancel 热路径为 0 次 heap
   allocation。

fixture tier 语义：`doc-derived-unverified-live` 只表示按官方文档采集，
`implementation-derived` 表示来自既有实现/测试，
`implementation-derived-deterministic` 表示可重复的签名向量；这些 tier
均不自动等价于真实 venue 联调 PASS。

### 2.4 OMS phase 3 runtime

`oms_fixed_spsc_ring_tests`、`oms_runtime_primitives_tests`、
`oms_runtime_tests` 和 `oms_dual_mode_tests` 覆盖：

1. 固定容量 SPSC ring 的 reserve/cancel/commit、consumer peek cancel、
   wrap、并发传输和 runtime POD payload；
2. eventfd/timerfd flags、drain/arm、deadline 顺序、取消、容量与 ABA handle；
3. Inline 与 Dedicated-I/O 的同 lane place/cancel FIFO、session
   epoch/sequence、lane round-robin，以及某 lane 输出背压不阻塞其他 lane；
4. 状态变更前预留最多两个 venue update，输出满时保留单个 pending venue
   event；command 背压时保留 FIFO 头而不是消费丢弃；
5. Inline `service_io` 的 poll/有限等待/无限等待和 owner-thread guard；
   Dedicated-I/O 的 enqueue-only submit、`InvalidMode`、eventfd 唤醒和 shutdown；
6. normalized replay 的 delay/disconnect/reconnect、decimal scale 解析，以及
   Inline/Dedicated 完整 update byte parity。

Release 测试目标显式取消 `NDEBUG` 对两个仍使用 `assert` 的 primitive test
executables 的影响，避免检查及带副作用表达式被编译掉。Runtime/public API
不暴露 internal headers；真实交易所 transport、鉴权、重连策略仍属于后续
venue adapter 阶段，不由 fake replay PASS 外推。

### 2.5 OMS StrategyFrame example 与 queue benchmark

`oms_strategy_frame_example` 只使用公共 `ExecutionChannel` facade，覆盖
lane 初始化、place、Inline `service_io`、策略线程 `drain_updates` 与显式
shutdown 的最小集成形状。Dedicated-I/O 的等价契约是等待
`notification_fd`，不调用 `service_io`。

`oms_benchmark` 对 Inline 与 DedicatedIo 分别串行量测 place 调用开始到匹配
`Submitted` update 被 drain 的耗时，输出 p50/p95/p99/p99.9/max、queue high
water，以及短时 idle wall/process-CPU 观察。它明确标记
`offline_fake_loopback` 与 `production_slo:false`；该结果包含虚拟机调度、时钟、
poll 和 callback 成本，不接触 venue，也不替代第 7 节生产性能 Gate。

Polymarket 公共构造路径只接受 signature type 0 与 type 3；type 1 被拒绝。
无授权 credentials 时，外部 Binance/Polymarket acceptance 为 **PENDING**。

## 3. 当前本地执行命令

```bash
cd /home/hulong/self-quant/core
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
ctest --test-dir build --output-on-failure
```

父代理已配置并验证的本地工具链启用 `SELF_QUANT_ENABLE_WERROR=ON`，构建与测试命令为：

```bash
cmake --build build/all-zig2
ctest --test-dir build/all-zig2 --output-on-failure

cmake --build build/asan-zig
ctest --test-dir build/asan-zig --output-on-failure

cmake --build build/release-zig
ctest --test-dir build/release-zig --output-on-failure
```

对应 Zig/Clang 21.1.0 wrapper、CMake 4.4.2、源码构建 simdjson（为该工具链关闭 AVX512）和 OpenSSL 3.0.13 headers/libs；真实结果见第 11 节追加记录。

只验证 utils：

```bash
cmake -S . -B build-utils -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=OFF
cmake --build build-utils -j
ctest --test-dir build-utils --output-on-failure
```

直接运行并保存日志：

```bash
mkdir -p artifacts/validation
./build/utils/self_quant_utils_tests \
  >artifacts/validation/utils-tests.log 2>&1
./build/mds/mds_tests \
  >artifacts/validation/mds-tests.log 2>&1
```

OMS Release 双矩阵：

```bash
cmake -S . -B build-oms-off -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=OFF -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON -DSELF_QUANT_INSTALL=OFF
cmake --build build-oms-off -j2
ctest --test-dir build-oms-off --output-on-failure

cmake -S . -B build-oms-on -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=ON -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON -DSELF_QUANT_INSTALL=OFF
cmake --build build-oms-on -j2
ctest --test-dir build-oms-on --output-on-failure
```

注意：`assert` 在定义 `NDEBUG` 时会被编译掉。CMake 的 Release 通常定义 `NDEBUG`，因此 `utils_tests.cpp` 的大部分运行时断言可能失效。上线 Gate 应把测试改为不依赖 `assert` 的框架，或至少在未定义 `NDEBUG` 的配置运行；`mds_tests` 的 `require` 不受此影响。

## 4. 必补离线单元/属性测试 Gate

### 4.1 Wire/ABI

- 对规范中的每个 struct 执行 `sizeof/alignof/offsetof` 静态断言；
- 对每个 enum 执行显式数值断言；
- golden bytes 验证 little-endian、record length、零 padding；
- unknown type/major/minor、短记录、超长记录、非法 state/side 拒绝；
- snapshot 0/1/24/25/最大档分片与重组；
- 验证 ring schema 4 CRC32C golden vectors、空/不同长度 payload、篡改拒绝和 wrap/padding；SnapshotEnd 独立 checksum 在算法冻结前仍不得伪称已验证。

### 4.2 JSON/metadata

- Spot/USD-M 官方 bookTicker/depth fixture；
- 缺字段、错类型、combined envelope、超容量；
- `0`、负数、前导零、小数点、空字符串、仅符号、超 scale 零/非零尾、INT64 边界/overflow；
- event symbol/type routing；
- exchange info filters 到 scale/tick/lot 的转换。

### 4.3 序列与 book 模型

- 随机生成 snapshot + overlap/duplicate/gap/乱序输入，与简单 `std::map` reference book 逐事件比较；
- Spot `U/u` 边界、USD-M `pu` 边界；
- `inject_snapshot` 后必须保持 Bridging；drain callback 按序实际应用缓存，callback 失败/gap 转 Invalid，至少应用一条后才 Live；
- resnapshot generation、状态期间禁止 Live publish；
- tick/lot metadata 变化、crossed book、窗口外档位。

### 4.4 SharedRing/队列并发

- 真实 fork 多 reader、不同速度、reader crash/PID reuse；
- cursor wrap-around、尾部 <40 与 ≥40、sequence/epoch 边界；
- creator/attacher race、unlink 生命周期、权限；
- hugetlbfs 成功/失败/fallback；
- 生产者误用（多 producer）应在文档/调试保护中可诊断；
- SPSC/MPSC wrap 和压力测试；TSan（注意跨进程 atomics不由 TSan 完整覆盖）。

### 4.5 SequenceArbiter 热路径

- 构造后对 `submit_each` 做 allocator instrumentation，证明不同 gap/duplicate/divergence/resync 批次无堆分配；
- 固定 sink 容量不足并返回 false 时，验证停止语义及调用方恢复策略；
- pending/history ring wrap、槽冲突、超 history 窗口 Regression、`max_gap=0` 规范化；
- `submit()` 仅用于控制/测试，性能测试不得把其 vector 分配计入无分配热路径。

## 5. 公网 Gate

以下必须在允许访问 Binance 官方域名的受控环境执行：

### 5.1 Spot

- BTCUSDT `bookTicker`/`depth@100ms` 连续采集；
- `stream-sbe.binance.com` 的 `bestBidAsk`/`depth` binary stream，含 API Key upgrade、schema 1:0 decode 和 text control routing；
- exchange info + `/api/v3/depth?limit=5000`；
- snapshot bridge、每小时带 update-ID 锚点的 REST 逐档比对；
- raw capture 两次确定性回放；
- ping/pong、服务端 close、24h 预轮换；
- combined 与 raw stream routing（若产品支持 combined）。

### 5.2 USD-M

- BTCUSDT `bookTicker`/`depth@100ms`；
- `/fapi/v1/exchangeInfo` + `/fapi/v1/depth?limit=1000`；
- bridge 与每个 Live 事件 `pu==previous u`；
- 3min ping/10min timeout 行为、24h rotation；
- REST 锚点逐档比对和确定性回放。

每项必须保存 capture、REST 原文、日志、metrics、compare report。当前已有 JSON combined-stream daemon、REST snapshot orchestrator 和 wire consumer；capture/replay/REST compare、Spot 公网 SBE 与 24h 轮换证据仍未补齐，因此完整公网 Gate 仍未全部 PASS。

### 5.3 Polymarket rolling MDS live smoke

该 Gate 必须以隔离 producer 部署执行，不能与跨 venue aggregator 共进程，
也不能把 alias 当作 OMS 可交易 symbol。运行期间固定消费 `BTC5MUP` 与
`BTC5MDOWN` 的 ticker/orderbook SHM 段，至少跨越两个完整五分钟 rollover：

- 保存每轮 Gamma exact-market/token 异步发现证据，但确认实时行情仅来自 WSS；
- 覆盖 WSS `book` snapshot、`price_change`、BBO、tick-size 与 lifecycle；
- 每次 slug 变化必须同时观察到 `book_generation` 递增、
  `InstrumentUpdate(Building)`、旧 book 清空及新 snapshot 后恢复 Live；
- 验证固定段名、稳定 alias/`instrument_id`，且 token ID 未进入 wire；
- 注入或观察断连及 stale timeout，确认旧行情立即退出 Live、不会伪刷新，
  重连后以新 generation 重建；
- 对两次 rollover 前后 BBO/book 做有界新鲜度、单调 generation、无跨市场
  档位残留检查，并保存日志、WSS capture/hash 与 SHM consumer 输出。

2026-08-19 UTC 在隔离 prefix `/selfquant.mds.poly` 上完成 producer 侧
650 秒公网验收（dirty worktree，Linux 6.17，GCC 13.3，RelWithDebInfo）：

```bash
./build/mds/mds_producer \
  --config mds/config/mds_polymarket.example.yaml --duration 650
```

退出码为 0；期间 `discovery_requests=2`、`discovery_failures=0`、
`market_rollovers=2`、`reconnects=0`、`parse_errors=0`、
`publish_errors=0`、`heartbeat_timeouts=0`，共发布 497038 次 depth 与
489068 次 ticker 更新。该结果确认异步 Gamma 预发现、WSS 行情、动态订退及
两个连续 rollover 的 producer 路径可运行。

完整 Gate 仍为 **PARTIAL**：本次未同时保存独立 SHM consumer 的逐记录输出、
WSS capture/hash 和故障注入证据，因此不能把 producer 进程 PASS 外推为第
289–297 行全部验收项均已完成。

## 6. 故障注入 Gate

至少覆盖：

- DNS failure、connect timeout/refused、TLS cert/hostname/expiry、HTTP upgrade 非 101；
- WebSocket fragment/truncate/oversize/compressed/reserved opcode、ping 丢失、close；
- 断网、半开连接、网络抖动、重复/乱序/缺口；
- REST timeout/429/418/5xx、快照持续落后、malformed JSON；
- simdjson 未编译、parser capacity、decimal overflow；
- depth buffer 满、ladder out-of-window、metadata 变化；
- SHM 不存在/已存在/半初始化/损坏、满 ring、慢/死 reader、publisher/consumer SIGKILL；
- TSC unsupported/迁移、绑核失败、NUMA 配置非法；
- 磁盘 capture 满、FD exhaustion、内存限制。

通过条件不是“进程未崩溃”而已：状态必须降级、禁止错误 Live 数据、指标/日志准确、按限频恢复且没有重连风暴。

## 7. 性能 Gate

在固定目标硬件、CPU governor、NUMA/IRQ/网卡配置下：

1. 录制生产等价输入，建立 1×峰值基线；
2. 回放 2×预估峰值至少 60 分钟；
3. 分别测 parse、sequence、book、wire、SHM publish、consumer；
4. 输出吞吐和 p50/p95/p99/p99.9/max；记录 histogram 原始文件；
5. 监控 queue/ring 水位、backpressure、CPU、migration、allocation、RSS、page fault；
6. 结果达到 `requirements.md` SLO，零未解释丢失/重快照/错误状态。

同时运行 microbenchmark 与端到端 benchmark；不能用单条消息平均耗时替代 tail latency。虚拟机结果可用于回归，不作为最终生产 Gate。

## 8. 24h 与 7 天长稳

### 8.1 24h 垂直切片

按 `binance_spot_vertical_slice.md` 运行 ≥24h30m。覆盖至少一次 24h connection 生命周期；每小时 REST 锚点比对；结束后完整回放和资源趋势分析。

### 8.2 7 天长稳

Spot + USD-M、多标的、目标订阅规模运行连续 7×24h：

- 每次重连/重快照都有原因和恢复时长；
- 连接轮换无未解释序列缺口；
- RSS、FD、线程、pending map/capture buffer 无单调泄漏；
- reader slot 无永久 Suspect/Initializing；
- REST 比对零未解释 mismatch；
- 延迟/新鲜度按小时/天无趋势退化；
- capture 抽样每日重放一致；
- 无 crash、deadlock、data race 证据。

计划内部署必须单独标记，不得从 uptime 中静默删除。7 天期间任何 P1 数据正确性事件都使 Gate 失败并从修复后重新计时。

## 9. Sanitizer 与静态检查

- ASan+UBSan：当前 `self_quant_utils_tests` 与 `mds_tests` 已在 `build/asan-zig` 通过；后续新增的回放和故障测试仍必须纳入 sanitizer Gate；
- TSan：进程内 queue/service/network orchestration 测试；
- 编译 warnings：`SELF_QUANT_ENABLE_WERROR=ON`；
- clang-tidy/cppcheck（引入配置后）；
- fuzz：WebSocket parser、JSON parser、wire decoder、capture reader。

OpenSSL/simdjson 等依赖版本和 sanitizer suppressions 必须随证据保存；suppression 需评审，不能用于隐藏本项目错误。

## 10. 发布判定矩阵

| Gate | 当前可执行基础 | 发布要求 |
|---|---|---|
| utils/mds 单元测试 | Debug/Werror、ASan+UBSan/Werror、Release/Werror 均 2/2 PASS | PASS + 日志 |
| ABI golden/完整 decoder | 部分 | PASS |
| Binance JSON fixtures | bookTicker/depth symbol/exponent/定点数 fixture 已有 | PASS（最新本地 mds_tests 覆盖） |
| Spot SBE 1:0 decoder 单元测试 | template 10001/10003 symbol/exponent fixture 已有 | PASS（最新本地 mds_tests 覆盖） |
| SharedRing CRC32C 篡改拒绝 | schema 3 payload tamper fixture 已有 | PASS（最新本地 mds_tests 覆盖） |
| SequenceArbiter callback 路径 | `submit_each` + 固定 `std::array` sink fixture 已有 | PASS（最新 Debug/Werror mds_tests 覆盖） |
| Spot BTCUSDT 公网切片 | JSON `bookTicker` + `depth@100ms` 10 分钟 smoke PASS；完整 SBE/capture/compare 未完成 | PASS |
| USD-M 公网切片 | JSON `bookTicker` + `depth@100ms` 10 分钟 smoke PASS；完整 capture/compare 未完成 | PASS |
| Capture/replay/REST compare | `BLOCKED`：executable 不存在 | PASS |
| 故障注入 | 少量离线覆盖 | PASS |
| OMS queue microbenchmark | offline fake loopback，输出 p50/p95/p99/p99.9 与 idle 观察 | 仅回归证据；不作为生产 SLO |
| 2×峰值性能 | OMS benchmark 不覆盖录制生产等价输入或 venue 网络 | PASS |
| 24h | daemon/orchestrator 已存在；`NOT RUN` | PASS |
| 7 天长稳 | `NOT RUN`；capture/compare/监控工具仍不完整 | PASS |
| 监控/告警/runbook | 不存在 | 评审通过 |

任一发布要求未 PASS，版本只能标记开发/实验用途。

## 11. 执行记录

在此追加不可变记录，不覆盖历史：

```text
时间:
commit/dirty:
主机:
配置:
命令:
结果: PASS | FAIL | BLOCKED | NOT RUN
日志:
备注:
```

截至本文创建时，不预填任何未实际执行的 PASS 结果。

### 2026-08-04T16:21:31Z 文档创建验证

```text
时间: 2026-08-04T16:21:31Z
commit/dirty: BLOCKED；/home/hulong/self-quant 及 core 均未检测到 Git repository
主机: Linux 6.17.0-1010-aws x86_64 GNU/Linux
配置: 既有 /home/hulong/self-quant/core/build；另尝试 /tmp 全新 Debug 配置
命令 1: ctest --test-dir build --output-on-failure
结果 1: BLOCKED；退出码 0，但输出 "No tests were found"，执行测试数为 0，不计 PASS
命令 2: ctest --test-dir build/utils --output-on-failure
结果 2: BLOCKED；登记了 self_quant_utils_tests，但 executable 不存在，Test #1 Not Run，退出码 8
命令 3: cmake -S /home/hulong/self-quant/core -B /tmp/self-quant-doc-validation-ninja -G Ninja -DCMAKE_BUILD_TYPE=Debug
结果 3: BLOCKED；环境找不到 C++ compiler，配置失败，后续 build/ctest 未执行
公网/性能/故障/24h/7d: NOT RUN
备注: 本次没有任何测试被记录为 PASS
```

上述 BLOCKED 记录发生在父代理配置 Zig/Clang 本地工具链之前，作为历史保留；它不覆盖下述后续成功结果。

### 2026-08-04 父代理本地工具链验证

```text
时间: 2026-08-04（父代理实际执行）
工具链: Zig/Clang 21.1.0 wrapper；CMake 4.4.2
依赖: simdjson 源码构建，AVX512 针对该工具链关闭；OpenSSL 3.0.13 headers/libs
命令 1: cmake --build build/all-zig2
结果 1: PASS
命令 2: ctest --test-dir build/all-zig2 --output-on-failure
结果 2: PASS；2/2 tests passed
覆盖边界: 本地 self_quant_utils_tests 与 mds_tests；包括 Spot SBE template 10001/10003 离线 fixture
公网 daemon/REST orchestrator: BLOCKED（仍不存在）
真实 Spot/Spot SBE/USD-M 公网: NOT RUN
性能/故障/24h/7d: NOT RUN
备注: 该 PASS 只证明上述构建和两个本地测试；不外推到公网、24h 或生产 SLO
```

### 2026-08-04 父代理最新 Werror 回归

```text
时间: 2026-08-04（父代理在最新代码上实际执行）
配置: build/all-zig2；SELF_QUANT_ENABLE_WERROR=ON
命令 1: cmake --build build/all-zig2
结果 1: PASS（Werror build）
命令 2: ctest --test-dir build/all-zig2 --output-on-failure
结果 2: PASS；2/2 tests passed
新增覆盖: SharedRing schema 3 CRC32C payload 篡改拒绝；JSON bookTicker/depth fixture；
          SBE BBO/depth 固定 symbol 与 exponent；snapshot 后 buffer drain callback 才进入 Live
公网 daemon/REST orchestrator: BLOCKED（仍不存在）
真实 Spot/Spot SBE/USD-M 公网、24h、7d: NOT RUN
备注: 此记录晚于上一条父代理 PASS，命令保持 Werror build + ctest；不外推到公网或生产 SLO
```

### 2026-08-04 父代理 ASan+UBSan/Werror 验证

```text
时间: 2026-08-04（当前会话，父代理实际执行）
配置: build/asan-zig；Clang/Zig 21.1.0；SELF_QUANT_ENABLE_WERROR=ON
编译参数: -fsanitize=address,undefined -fno-omit-frame-pointer
命令 1: cmake --build build/asan-zig
结果 1: PASS（build 成功）
命令 2: ctest --test-dir build/asan-zig --output-on-failure
结果 2: PASS；2/2 tests passed
覆盖: SharedRing CRC32C payload 篡改拒绝；JSON/SBE symbol 与 exponent；
      snapshot 后 drain_buffered callback 实际应用缓存才进入 Live
公网/性能/24h/7d: NOT RUN
备注: 该结果仅覆盖现有本地测试集的 ASan+UBSan/Werror 运行
```

### 2026-08-04 父代理 Release/Werror 验证

```text
时间: 2026-08-04（当前会话，父代理实际执行）
配置: build/release-zig；Release；Clang/Zig 21.1.0；SELF_QUANT_ENABLE_WERROR=ON
命令 1: cmake --build build/release-zig
结果 1: PASS（build 成功）
命令 2: ctest --test-dir build/release-zig --output-on-failure
结果 2: PASS；2/2 tests passed
覆盖: SharedRing CRC32C payload 篡改拒绝；JSON/SBE symbol 与 exponent；
      snapshot 后 drain_buffered callback 实际应用缓存才进入 Live
公网/性能/24h/7d: NOT RUN
备注: 该结果仅覆盖现有本地测试集的 Release/Werror 运行，不构成性能结果
```

### 2026-08-04 父代理最新 Debug/Werror 仲裁器回归

```text
时间: 2026-08-04（当前会话，父代理实际执行）
配置: build/all-zig2；Debug；SELF_QUANT_ENABLE_WERROR=ON
命令 1: cmake --build build/all-zig2
结果 1: PASS（Werror build）
命令 2: ctest --test-dir build/all-zig2 --output-on-failure
结果 2: PASS；2/2 tests passed
新增覆盖: SequenceArbiter::submit_each 使用固定 std::array context 的 callback 路径；
          现有 CRC 篡改、JSON/SBE symbol/exponent 与 buffer drain 覆盖继续通过
边界: 测试验证无 vector 返回的 callback 路径；尚未通过全局 allocator hook 量测所有仲裁分支
公网/性能/24h/7d: NOT RUN
备注: 返回 vector 的 submit() 仅为控制/测试便利接口；本记录不外推到 ASan/Release 新回归
```

### 2026-08-05 Binance 端到端 Demo 验证

```text
时间: 2026-08-05T09:06:57Z
commit/dirty: BLOCKED；工作目录未检测到 Git repository
主机: Linux 6.17.0-1010-aws x86_64
配置: C++20；Zig/Clang wrapper；SELF_QUANT_ENABLE_WERROR=ON；Ladder=16384
命令 1: cmake --build build/all-zig2 &&
        ctest --test-dir build/all-zig2 --output-on-failure
结果 1: PASS；5/5 tests passed
覆盖 1: wire/book/overlay、Spot/USD-M REST/stream fixture、TCP/TLS/HTTP/WS、
        POSIX SHM publisher、side-aware snapshot、真实 loopback TLS+WS+REST、
        disconnect/sequence-gap recovery
命令 2: cmake --build build/asan-zig &&
        ASAN_OPTIONS=detect_leaks=1 UBSAN_OPTIONS=print_stacktrace=1
        ctest --test-dir build/asan-zig --output-on-failure
结果 2: PASS；5/5 tests passed
命令 3: binance_mds_demo --profile both --symbol BTCUSDT --duration 600
        --ladder 16384 --shm-prefix /selfquant.smoke.pass
结果 3: PASS；Spot 与 USD-M 均完成 Metadata→Snapshot→Bridging→Live；
        Spot depth=5985/ticker=33183、1 次窗口重同步并恢复 Live、0 reconnect；
        USD-M depth=5869/ticker=71305、0 resync、0 reconnect
命令 4: mds_shm_consumer --bbo-only <Spot/USD-M ticker/orderbook segments>
结果 4: PASS；解析真实 symbol/profile、定点价格数量、generation、source/bus seq、
        side-aware snapshot 与 Live canonical BBO
附加验证: consumer 退出后的 120 秒公网运行 PASS；stale reader 被注销/回收，
          Spot depth=1190/ticker=5993，USD-M depth=1161/ticker=9622，
          两侧均 0 resync/0 reconnect
未运行: 24h、7 天、2×峰值性能、capture/replay/REST 周期比对、Spot 公网 SBE
判定: Demo smoke PASS；不构成生产发布 Gate 全部通过
```

### 2026-08-05 SharedRing 覆盖模式与股票行情 Demo 验证

```text
时间: 2026-08-05T10:55:24Z 至 2026-08-05T11:06:27Z
commit/dirty: BLOCKED；/home/hulong/self-quant/core 不是 Git repository
主机: Linux 6.17.0-1010-aws x86_64
配置: C++20；Zig/Clang wrapper；SELF_QUANT_ENABLE_WERROR=ON；
      SharedRing schema 4；默认 OVERWRITE_OLDEST
命令 1: cmake --build build/all-zig2 -j2 &&
        ctest --test-dir build/all-zig2 --output-on-failure
结果 1: PASS；6/6 tests passed
命令 2: cmake --build build/asan-zig -j2 &&
        ctest --test-dir build/asan-zig --output-on-failure
结果 2: PASS；6/6 tests passed（ASan+UBSan）
新增覆盖: Ticker OHLC wire 往返；OverwriteOldest/RecordOverwritten/套圈恢复；
          Lossless 背压回归；股票 producer + 慢策略丢失统计 + 重启 LATEST 集成测试
手工强制套圈: ring_bytes=4096、interval_ms=2、process_ms=90；
              策略持续恢复并报告递增 lost，未发生 CRC torn-read 退出
手工重启: 重启前末段 bus_seq 约 133，重启后首条 ticker bus_seq=886；
          证明新 reader 从当前 writer_cursor 开始，不回放历史行情
命令 3: binance_mds_demo --profile both --symbol BTCUSDT --duration 600
        --ladder 16384 --shm-prefix /selfquant.overwrite.20260805
结果 3: PASS；Spot/USDM 均完成 TCP→TLS→Upgrade→Snapshot/Bridge→Live；
        Spot depth=5986/ticker=37080、1 次窗口重同步后恢复 Live、0 reconnect；
        USDM depth=5866/ticker=76943、0 resync、0 reconnect
覆盖模式观察: 全程未出现 "slow reader prevents ring overwrite"
未运行: 24h、7 天、2×峰值性能、capture/replay/REST 周期比对、Spot 公网 SBE
判定: 本阶段功能、Werror、sanitizer、离线集成与 10 分钟公网 smoke PASS；
      不构成生产发布 Gate 全部通过
```

### 2026-08-05 股票 Ticker 独立随机间隔验证

```text
时间: 2026-08-05
commit/dirty: BLOCKED；/home/hulong/self-quant/core 不是 Git repository
配置: 每只股票独立 next_deadline；默认均匀随机区间 1000..3000ms；
      测试 seed=20260805；SharedRing 与策略协议保持不变
生产启动示例:
  equity_ticker_publisher --segment /selfquant.mds.equity.ticker.2
  （默认 --min-interval-ms 1000 --max-interval-ms 3000）
可复现启动示例:
  equity_ticker_publisher --min-interval-ms 1000
  --max-interval-ms 3000 --seed 20260805
命令 1: cmake --build build/all-zig2 -j2 &&
        ctest --test-dir build/all-zig2 --output-on-failure
结果 1: PASS；6/6 tests passed
命令 2: cmake --build build/asan-zig -j2 &&
        ctest --test-dir build/asan-zig --output-on-failure
结果 2: PASS；6/6 tests passed（ASan+UBSan）
新增覆盖: min=0 拒绝；max<min 拒绝；min=max 允许；
          固定 seed + 2..4ms 随机区间下强制套圈、丢失统计和重启 LATEST
手工默认区间 smoke: duration=4、seed=20260805，10 只股票均产生 Ticker；
                    启动日志确认 min_interval_ms=1000、max_interval_ms=3000
速率说明: 10 只股票的单只期望周期为 2 秒，总期望速率约 5 条/秒；
          聚合平均间隔约 0.2 秒，但实际到达保持随机
判定: 随机调度改造及既有 Binance/SharedRing 回归 PASS
```

### 2026-08-05 股票消费者 Busy Loop 验证

```text
时间: 2026-08-05
commit/dirty: BLOCKED；/home/hulong/self-quant/core 不是 Git repository
实现: SharedRing::try_read 返回 ErrorCode 并通过输出参数交付 ReadLease；
      空读不构造 api::Result/std::string；股票策略空闲路径使用 _mm_pause
命令 1: cmake --build build/all-zig2 -j2 &&
        ctest --test-dir build/all-zig2 --output-on-failure
结果 1: PASS；6/6 tests passed
命令 2: cmake --build build/asan-zig -j2 &&
        ctest --test-dir build/asan-zig --output-on-failure
结果 2: PASS；6/6 tests passed（ASan+UBSan）
无分配覆盖: allocator instrumentation 下连续 100000 次空 try_read 为 0 次 heap allocation
功能覆盖: 空读、成功租约/commit、套圈错误码、busy-spin 集成、丢失统计和重启 LATEST
手工 smoke: 默认 1..3 秒随机 Producer，Consumer --process-ms 0；
              常见 lag_ms 约 0.022..0.036ms，观测到 0.223/0.570ms 调度异常值
边界: 当前结果含虚拟机调度、wall-clock 取时和逐条终端日志，不作为生产性能 Gate；
      busy-spin 实例持续占用 CPU，生产部署应绑定独立物理核心并避开 SMT sibling
判定: try_read 无分配热路径、busy-spin 功能和 sanitizer 回归 PASS
```

### 2026-08-06 SDK 安装契约与延迟回归

```text
时间: 2026-08-06
commit/dirty: BLOCKED；/home/hulong/self-quant/core 不是 Git repository
编译器: Zig Clang 21.1.0；Release -O3；benchmark 使用 taskset -c 0
功能命令:
  cmake --build build/sdk-v3-all -j2
  ctest --test-dir build/sdk-v3-all --output-on-failure
结果: PASS；SELF_QUANT_ENABLE_WERROR=ON，6/6 tests passed
Sanitizer:
  ASan+UBSan Debug build，6/6 tests passed
安装冒烟:
  tests/install_package_smoke.sh ... ON
  tests/install_package_smoke.sh ... OFF
结果: PASS；self_quant::mds / self_quant::utils 均可 find_package；
      spsc_ring.h 可用，hardware.h 与 mds/network 不可达

性能基线（改动前 5 次中位数）:
  wire EncodeTicker: throughput=863616.72 ops/s，p99=497ns
  SharedRing publish/read: throughput=223694.42 ops/s，p99=3573ns
性能结果（改动后隔离运行 10 次中位数）:
  wire EncodeTicker: throughput=884192.25 ops/s（+2.38%），p99=493ns（-0.80%）
  SharedRing publish/read: throughput=225340.73 ops/s（+0.74%），p99=3718.5ns（+4.07%）
原始 after 样本: build/sdk-v3-benchmark-results/*.csv
判定: PASS；吞吐与 p99 均未劣化超过 5%。虚拟机结果仅用于代码回归，
      不替代生产裸机 SLO Gate。
```

### 2026-08-15 OMS 第二阶段离线核心

```text
范围: protocol fixtures、POD API、immutable instrument side table、
      fixed-capacity OrderTable、single-writer StateEngine

Release / MDS OFF + OMS ON:
  SELF_QUANT_ENABLE_WERROR=ON
  结果: PASS；2/2 tests passed

Release / MDS ON + OMS ON:
  SELF_QUANT_ENABLE_WERROR=ON
  结果: PASS；25/25 tests passed

ASan+UBSan / MDS ON + OMS ON:
  结果: PASS；25/25 tests passed
  备注: 首次全量执行的既有 mds_inprocess_aggregation_tests 出现一次断言波动；
        targeted rerun 与随后全量 rerun 均 PASS，oms_core_tests 始终 PASS。

TSan / MDS OFF + OMS ON:
  build: PASS
  默认运行: 环境因 ASLR 报 ThreadSanitizer unexpected memory mapping
  setarch x86_64 -R 后直接执行 utils 与 oms_core_tests: PASS

Polymarket Go golden:
  go test ./internal/polymarket
  结果: PASS

Fixture:
  10 个 JSON 文件均可解析；manifest 20 个稳定 ID 的路径全部存在。

判定: 第二阶段离线 Gate PASS。Binance doc-derived fixture 仍未经过真实
      testnet/account stream 验证，不得标记为 live-verified。
```

### 2026-08-15 OMS phase 3 runtime

```text
时间: 2026-08-15T10:59Z
commit/dirty: 2e5f906315ff395feb61b2bec693ae975f1fe7da；dirty（保留既有工作）
主机: Linux 6.17.0-1017-aws x86_64
工具链: GCC 13.3.0；CMake 3.28.3；Ninja；SELF_QUANT_ENABLE_WERROR=ON

Release / MDS OFF + OMS ON:
  cmake --build build/oms-phase3-off -j2
  ctest --test-dir build/oms-phase3-off --output-on-failure --timeout 30
  结果: PASS；6/6 tests passed

Release / MDS ON + OMS ON:
  cmake --build build/oms-phase3-on -j2
  ctest --test-dir build/oms-phase3-on --output-on-failure --timeout 60
  结果: PASS；29/29 tests passed

ASan+UBSan / MDS OFF + OMS ON:
  CXX flags: -fsanitize=address,undefined -fno-omit-frame-pointer
  ASAN_OPTIONS=detect_leaks=1；UBSAN_OPTIONS=print_stacktrace=1
  结果: PASS；6/6 tests passed

TSan / MDS OFF + OMS ON:
  CXX flags: -fsanitize=thread -fno-omit-frame-pointer
  setarch x86_64 -R ctest --test-dir build/oms-phase3-tsan ...
  结果: PASS；6/6 tests passed

diff/lint:
  git diff --check -- core/oms core/docs/validation.md core/CMakeLists.txt: PASS
  IDE diagnostics for core/oms and validation.md: 0

边界: normalized fake transport 只验证 deterministic runtime orchestration；
      未实现真实 venue wire adapter/鉴权，本结果不构成 testnet/live trading PASS。
```
