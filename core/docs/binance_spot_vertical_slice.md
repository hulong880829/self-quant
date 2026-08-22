# Binance Spot `BTCUSDT` 纵向切片

## 1. 目标与当前可执行性

纵向切片的完成定义是：从 Binance 官方 Spot WebSocket 接收 `btcusdt@depth@100ms` 与 `btcusdt@bookTicker`，同时获取 REST 快照，按 `U/u` 桥接并构建本地订单簿，发布 wire 记录到共享内存，消费者读取，录制原始输入并确定性回放，最后与 REST 逐档比对并完成 24h 验收。

当前仓库只提供以下可执行基础：

- `self_quant_utils_tests`：wire layout、symbol、ladder、SPSC/MPSC、timestamp/hardware；
- `mds_tests`：共享环、reader lease/回收、序列仲裁、Spot/USD-M 同步器、WebSocket 帧 parser、Spot SBE `stream_1_0` template 10001/10003 解码与错误 schema 拒绝，以及 SBE service Pending 状态；
- `mds_shm_consumer <segment-name>`：可 attach 已存在的 schema-2 共享环并打印外层记录元数据；
- TLS session、WebSocket 帧 parser、Binance JSON parser、DepthSynchronizer、OrderBook、SharedRing 等库组件。

当前没有 MDS daemon/CLI、TCP connect/HTTP/WebSocket upgrade、订阅发送、REST client、book-to-wire publisher、capture/replay 或 compare executable。故本文件的公网闭环命令是验收接口约定；在这些程序落地前，不能声称纵向切片已经完成。

## 2. 环境与官方入口

- WS raw stream：`wss://stream.binance.com:9443/ws/btcusdt@depth@100ms`
- BBO：`wss://stream.binance.com:9443/ws/btcusdt@bookTicker`
- Combined streams：`wss://stream.binance.com:9443/stream?streams=btcusdt@depth@100ms/btcusdt@bookTicker`
- REST snapshot：`https://api.binance.com/api/v3/depth?symbol=BTCUSDT&limit=5000`
- Exchange info：`https://api.binance.com/api/v3/exchangeInfo?symbol=BTCUSDT`
- SBE WS：`wss://stream-sbe.binance.com:9443/ws/btcusdt@depth` 与 `.../btcusdt@bestBidAsk`
- SBE schema：https://github.com/binance/binance-spot-api-docs/blob/master/sbe/schemas/stream_1_0.xml

官方文档：

- https://developers.binance.com/docs/binance-spot-api-docs/web-socket-streams
- https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints#order-book

需要公网 DNS/TCP 443/9443、可信 CA、时间同步，以及 `curl`；人工探测 JSON 可选 `websocat` 或 `wscat`。SBE endpoint 还要求 Ed25519 API Key 通过 `X-MBX-APIKEY` upgrade header 提交，且 binary market frame 与 JSON text control frame 必须分流。Binance 可能按地区限制访问，遇到 HTTP 451/403 不得改用非官方镜像作为验收证据。

## 3. 第 0 阶段：本地构建与单元测试

在 `/home/hulong/self-quant/core`：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
ctest --test-dir build --output-on-failure
```

预期目标：

```text
build/utils/self_quant_utils_tests
build/mds/mds_tests
build/mds/mds_shm_consumer
```

这是当前可执行阶段。必须保存 CMake 配置（是否找到 simdjson）、编译器版本和 ctest 输出。若未找到 simdjson，Binance JSON parser 会明确返回失败；Spot SBE decoder 不依赖 simdjson，但两种协议的公网切片都仍需要尚不存在的 daemon/REST orchestrator。

建议另建 sanitizer 配置，不覆盖 Release 结果：

```bash
cmake -S . -B build-asan -DCMAKE_BUILD_TYPE=Debug \
  -DCMAKE_CXX_FLAGS="-fsanitize=address,undefined -fno-omit-frame-pointer"
cmake --build build-asan -j
ctest --test-dir build-asan --output-on-failure
```

## 4. 第 1 阶段：联网前元数据冻结

以下需要公网：

```bash
mkdir -p artifacts/btcusdt
curl --fail-with-body --silent --show-error \
  'https://api.binance.com/api/v3/exchangeInfo?symbol=BTCUSDT' \
  -o artifacts/btcusdt/exchangeInfo.json
curl --fail-with-body --silent --show-error \
  'https://api.binance.com/api/v3/depth?symbol=BTCUSDT&limit=5000' \
  -o artifacts/btcusdt/initial-depth.json
```

验收程序应从 exchange info 的 `PRICE_FILTER.tickSize` 与 `LOT_SIZE.stepSize` 计算 `price_scale/quantity_scale` 和整数 mantissa，生成 `InstrumentUpdateRecord`。不得硬编码 BTCUSDT 的精度，也不得用当前盘口字符串的小数位推断。

当前仓库没有 exchange-info parser 或 REST client；上述 `curl` 只采集官方响应，不能直接驱动库。

## 5. 第 2 阶段：建立 WS、缓存增量、桥接快照

正确顺序：

1. 连接 WS depth stream；记录 TLS 完成、upgrade 完成和首条消息时间。
2. 开始把每条原始 JSON 连同本地 `receive_mono_ns/receive_tsc` 追加到 capture；同时送入有界缓存。
3. 连接建立后发起 REST depth snapshot，保存响应字节、HTTP headers、请求开始/结束单调时间。
4. 令快照 ID 为 `L=lastUpdateId`；丢弃缓存中 `u <= L` 的事件。
5. 找到首个满足 `U <= L+1 <= u` 的事件。不存在则继续等待；发现无法覆盖、缓存溢出或非法 `U>u` 时重新取快照。
6. 调用 `inject_snapshot(L)` 后 synchronizer 保持 Bridging；先把快照 bids/asks 装入 book，再调用 `drain_buffered(context, callback)`，由 callback 按接收顺序真正应用每条有效缓存事件。只有 drain 返回 BecameLive 才可发布 Live。每个 `[price,quantity]`：quantity=0 删除，否则替换该价位数量。
7. Live 后，下一事件必须覆盖 `previous_u+1`，即 `U <= previous_u+1 <= u`；否则立即停止发布 Live、generation+1 并重快照。
8. `bookTicker` 作为独立 BBO 源记录并用于交叉检查，不能替代 diff-depth 序列连续性。

人工观察官方流（外部工具，不经过仓库代码）：

```bash
websocat 'wss://stream.binance.com:9443/ws/btcusdt@depth@100ms'
websocat 'wss://stream.binance.com:9443/ws/btcusdt@bookTicker'
```

当前 `DepthSynchronizer` 已提供缓存 drain callback：snapshot 注入不再消费缓存或直接进入 Live；callback 失败、序列 gap 或非法区间会返回 Resnapshot 并置 Invalid。公网 orchestrator 尚未接线，因此端到端切片仍未完成，但此前“缓存无法实际应用”的库接口缺口已经消除。

可选 Spot SBE 路径使用同一 REST snapshot/`U/u` 桥接规则，但 stream 名为 `btcusdt@depth`（20ms）和 `btcusdt@bestBidAsk`。当前 decoder 已支持官方 schema id=1/version=0 的 `DepthDiffStreamEvent` template 10003 与 `BestBidAskStreamEvent` template 10001；两种输出都携带固定 `symbol[32]` 和 `price_exponent/quantity_exponent`，可安全进行多标的路由。schema/version/template、最小 blockLength 和消息边界不合法时拒绝。联网建连、API Key header、frame routing 和 capture 尚未实现，因此仍不能执行真实公网 SBE 纵向切片。

## 6. 第 3 阶段：发布与消费

目标 segment 名：

```text
/selfquant.mds.spot.btcusdt.ticker.2
/selfquant.mds.spot.btcusdt.orderbook.2
```

其中末尾 `1` 是 market-data wire major；外层 ring 自身严格校验 major 4，并对有效 payload 计算/验证 Castagnoli CRC32C。现行命名与版本规则以 [`segment_and_schema_contract.md`](segment_and_schema_contract.md) 为准。目标 producer 必须：

1. 创建 ring；
2. 发布 `InstrumentUpdate`；
3. 重建时发布 SnapshotBegin、连续 SnapshotChunk、SnapshotEnd；
4. Live 后按源事件发布 Delta/Bbo；
5. 给所有记录设置 instrument/source/bus/generation/state/timestamps；
6. 对 `QuotaExceeded` 显式告警并按策略降级，绝不覆盖 reader。

producer 存在后，当前消费者可执行：

```bash
./build/mds/mds_shm_consumer \
  /selfquant.mds.spot.btcusdt.orderbook.2
```

但该消费者只打印外层 epoch/sequence/type/bytes，不解码内层 wire，也不校验订单簿。当前仓库没有 producer，所以现在单独运行会 attach 失败，这是预期事实。

## 7. Capture 与确定性回放

需要新增并冻结 capture 格式，至少逐条保存：

- 文件 magic/schema、profile、symbol、instrument metadata；
- record kind（WS text、WS ping/pong、REST request/response、disconnect）；
- 原始字节长度与原始字节；
- receive monotonic ns、receive TSC、连接 ID；
- REST 快照对应的请求区间；
- 文件级内容 hash。

目标命令接口（当前不存在，不能执行）：

```bash
./build/mds/mds_capture \
  --profile spot --symbol BTCUSDT \
  --streams depth@100ms,bookTicker \
  --output artifacts/btcusdt/run.capture \
  --duration 30m

./build/mds/mds_replay \
  --input artifacts/btcusdt/run.capture \
  --output artifacts/btcusdt/replay-1.wire \
  --clock recorded --speed max

./build/mds/mds_replay \
  --input artifacts/btcusdt/run.capture \
  --output artifacts/btcusdt/replay-2.wire \
  --clock recorded --speed max

sha256sum artifacts/btcusdt/replay-1.wire \
          artifacts/btcusdt/replay-2.wire
cmp artifacts/btcusdt/replay-1.wire artifacts/btcusdt/replay-2.wire
```

若 publish TSC/bus epoch 等运行时字段不固定，回放工具必须提供 canonical 输出（将非确定字段归零或用 capture 值），并分别比较语义输出与完整字节输出。两次 canonical hash 必须相同。

回放必测样本：

- 正常桥接；
- snapshot 前大量缓存；
- 重复/陈旧事件；
- `U/u` gap；
- `U>u`；
- 数量 0 删除；
- 非法小数、overflow；
- WS fragmentation、interleaved ping、close；
- REST 失败/429、断线重连、generation 切换。

## 8. REST 逐档比对

REST 快照不是与不断变化的 WS book 原子同刻，因此不能把“请求返回后直接比较”当严格证明。验收工具采用带序列锚点的比对：

1. 在本地 Live 时记录开始时间 `t0` 并发起 REST depth。
2. 保存 REST 响应 `lastUpdateId=L2` 和完成时间 `t1`。
3. 从 capture 中重放到本地 book 的 source sequence 首次达到/覆盖 `L2` 的状态。
4. 将 REST 返回的最多 5000 档与该锚点状态逐价比较；REST 未覆盖窗口外的本地档不计为差异。
5. 分别报告 missing-local、missing-rest、quantity-mismatch、best-bid/ask mismatch；禁止只比较 BBO 后称“逐档一致”。
6. 如果 capture 无法把事件区间与 `L2` 对齐，结果是 inconclusive，重新采样，不记通过。

目标命令接口（当前不存在）：

```bash
./build/mds/mds_compare_rest \
  --profile spot --symbol BTCUSDT \
  --capture artifacts/btcusdt/run.capture \
  --rest-out artifacts/btcusdt/compare-rest.json \
  --depth 5000 \
  --report artifacts/btcusdt/compare-report.json
```

通过条件：比较窗口内 bids/asks 价格集合与数量全部相同、BBO 相同、source sequence 锚点有效，且报告保留原始 REST 响应 hash。

## 9. 阶段延迟

每条消息至少记录：

- `exchange_ts_ns`：Binance `E`（ms）×1,000,000；
- `receive_tsc` 和对应 monotonic calibration；
- parse start/end；
- sequence decision；
- book apply end；
- wire encode end；
- `publish_tsc`；
- consumer acquire time。

直方图：

1. exchange→receive：受主机 wall-clock 同步影响，单独标记时钟误差；
2. receive→parse；
3. parse→sequence；
4. sequence→book；
5. book→publish；
6. publish→consumer；
7. receive→consumer 总链路。

每阶段输出 count/min/max/p50/p95/p99/p99.9，按连接和 generation 分组；重连/重快照窗口单独统计。TSC 未校准、CPU migration 或非 invariant TSC 时必须使用 monotonic clock或将样本标记 invalid，不能输出伪 ns。

## 10. 24h 验收

运行至少 24h30m，以覆盖 Binance 24h 强制断线及当前 23h50m 预轮换目标。目标命令（daemon 尚不存在）：

```bash
timeout --signal=TERM 25h ./build/mds/mdsd \
  --config configs/binance-spot-btcusdt.json \
  --capture artifacts/btcusdt/24h.capture \
  --metrics-address 127.0.0.1:9108 \
  >artifacts/btcusdt/24h.log 2>&1
```

验收清单：

- 至少一次受控连接轮换，旧/新连接序列交接无未解释 gap；
- 任一 gap 都进入重快照，generation 递增，期间不发布 Live；
- 每小时执行一次带序列锚点的 REST 逐档比对，全部为 pass 或有明确 inconclusive 重试，零未解释 mismatch；
- raw capture 可完整回放，两次 canonical 输出一致；
- parser error、buffer overflow、SHM corruption、reader overrun、crossed book 均为 0；若发生则本次不通过；
- 无持续 reader 背压、FD/RSS/线程数单调泄漏；
- 延迟和 freshness 达到 `requirements.md` Gate，附原始直方图；
- 保存构建 commit、dirty 状态、配置、机器/内核/CPU、CMake 输出、测试结果、指标快照、日志、REST/capture hash。

没有这些 artifact 或运行不足 24h30m，状态只能是“未执行/不完整”，不能写“通过”。
