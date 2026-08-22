# Wire 与共享内存协议规范（按当前代码）

## 1. 两层协议与版本

当前共享内存记录由两层组成：

1. `mds::transport` 外层 ring record：定位、并发提交、payload CRC32C、reader cursor，`kRingSchemaMajor=5`；
2. payload 可承载 `utils::md::wire` 记录：行情语义，magic `0x444d5153`、schema `2.0`。

外层 `RecordHeader::type` 与内层 `utils::md::wire::RecordHeader::message_type` 当前没有代码级一致性校验，生产者必须写成相同语义值，消费者必须同时验证两层长度和类型。

wire major 必须精确相等。消费者接受不低于自身支持值的 minor；
遇到未知 `message_type` 时，在 magic、major、对齐和长度均合法的前提下
提交并跳过该记录，只推进 ring/bus 序列，不推进 instrument source 序列。
损坏记录仍触发 gap/resync。v2 进程必须拒绝旧 ring schema segment。

### 1.1 `instrument_id` 确定性契约

`instrument_id` 是 canonical key 的 FNV-1a 64 位结果，禁止使用
`std::hash`。序列化固定为 UTF-8 原始字节：

```
<canonical venue>|<canonical product>|<MDS canonical symbol>
```

venue/product 使用本规范的小写全名，分隔符固定为 `|`，symbol 不允许
展示别名、locale 大小写转换或临时缩写。Polymarket 固定拼写为
`polymarket|binary_option|btc5mup`，禁止 `poly`。ID 0 保留；若首轮 hash
为 0，依次对 `<key>|1`、`<key>|2` 进行域分离重哈希，直到非零。MDS
InstrumentManager 建 catalog 时维护 `id -> key` 并检测碰撞；同一 ID
对应不同 key 必须打印双方 key 后硬失败，不允许重映射或线性探测。

## 2. ABI、字节序和基本约束

- `utils::md::wire` 明确 `static_assert(std::endian::native == little)`；多字节字段以原生 little-endian 存储，不执行字节序转换。
- `mds::transport` 共享结构直接映射 C++ 标准布局及 `std::atomic`。它没有独立 big-endian `static_assert`，也不是跨编译器/跨架构序列化格式；实际只能在相同 ABI、相同原子表示且原子 lock-free 的进程间共享。承载 `utils::md::wire` 时整体部署仍被 little-endian 限制。
- offset 均从所属结构起始计，十进制字节；所有显式/隐式 padding 必须写零，除非字段是原子状态。
- ring record 总长 8 字节对齐；记录不会跨物理 ring 末尾。
- 固定点行情值为 `int64_t mantissa`，scale 来自 instrument 的 `price_scale`/`quantity_scale`；实际值为 `mantissa × 10^-scale`。wire 行情记录本身不携带 scale。
- 字符数组不保证在满长时 NUL 结尾；按首个 NUL 或固定容量解析，禁止越界使用 C 字符串函数。

## 3. 枚举值

### 3.1 行情枚举（`utils/md/types.h`）

- `Venue:uint16_t`：Unknown=0, Binance=1, Okx=2, Bybit=3, Gate=4, Bitget=5, Polymarket=6。
- `ProductType:uint8_t`：Unknown=0, Spot=1, Perpetual=2, Future=3, BinaryOption=4, Equity=5。
- `Side:uint8_t`：Bid=1, Ask=2；0 未定义，必须拒绝。
- `BookState:uint8_t`：Empty=0, Building=1, Live=2, Invalid=3, NeedsRestart=4。
- `MessageType:uint16_t`：Bbo=1, Ticker=2, BookDelta=3, SnapshotBegin=4, SnapshotChunk=5, SnapshotEnd=6, InstrumentUpdate=7, AggBbo=8, AggOrderBook=9, InstrumentCatalog=10。

### 3.2 reader 枚举

`ReaderState:uint32_t`：Free=0, Initializing=1, Active=2, Suspect=3。`kPaddingType=0` 是 ring 内部 padding，不是行情消息。

## 4. `utils::md` 内存类型

这些类型是 wire payload 依赖的固定布局；`EventHeader` 系列是进程内事件，不直接等于 wire 记录。

### 4.1 `Fixed`（16 字节，对齐 8）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 8 | `int64_t` | `mantissa` | 尾数 |
| 8 | 1 | `int8_t` | `scale` | 十进制 scale |
| 9 | 7 | `uint8_t[7]` | `reserved` | 保留，写零 |

### 4.2 `Level`（16 字节）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 8 | `int64_t` | `price` | 按 instrument price scale |
| 8 | 8 | `int64_t` | `quantity` | 按 instrument quantity scale；delta 中 0 表示删除 |

### 4.3 `EventHeader`（64 字节，仅进程内）

| offset | size | 类型 | 字段 |
|---:|---:|---|---|
| 0 | 8 | `uint64_t` | `instrument_id` |
| 8 | 4 | `uint32_t` | `book_generation` |
| 12 | 4 | padding | 写零 |
| 16 | 8 | `uint64_t` | `source_seq` |
| 24 | 8 | `uint64_t` | `bus_seq` |
| 32 | 8 | `uint64_t` | `exchange_ts_ns` |
| 40 | 8 | `uint64_t` | `receive_tsc` |
| 48 | 8 | `uint64_t` | `publish_tsc` |
| 56 | 1 | `BookState:uint8_t` | `state` |
| 57 | 1 | `uint8_t` | `source_id` |
| 58 | 2 | `uint16_t` | `validity` |
| 60 | 4 | `uint8_t[4]` | `reserved`，写零 |

### 4.4 `Instrument`（224 字节，对齐 8）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 8 | `uint64_t` | `instrument_id` | 稳定内部 ID，0 预留 |
| 8 | 2 | `Venue` | `venue` | venue 枚举 |
| 10 | 1 | `ProductType` | `product_type` | 产品枚举 |
| 11 | 1 | `uint8_t` | `price_scale` | 价格 scale |
| 12 | 1 | `uint8_t` | `quantity_scale` | 数量 scale |
| 13 | 3 | scale/flags/reserved | 见 `types.h`，未使用位写零 |
| 16 | 8 | `int64_t` | `tick_size` | price mantissa 单位 |
| 24 | 8 | `int64_t` | `lot_size` | quantity mantissa 单位 |
| 32 | 8 | `int64_t` | `contract_multiplier` | 合约乘数；Spot 的约定需由上层冻结 |
| 40 | 4 | `uint32_t` | `expiry_yyyymmdd` | 到期日；永续/Spot 通常 0 |
| 44 | 16 | `char[16]` | `base_asset` | 基础资产 |
| 60 | 16 | `char[16]` | `quote_asset` | 计价资产 |
| 76 | 16 | `char[16]` | `settle_asset` | 结算资产 |
| 92 | 32 | `char[32]` | `canonical_symbol` | 规范符号 |
| 124 | 32 | `char[32]` | `venue_symbol` | 交易所符号 |
| 156 | 64 | `char[64]` | `instrument_key` | 唯一 key |
| 220 | 4 | 尾部 padding | — | 写零 |

### 4.5 Binance adapter symbol/exponent（仅进程内）

更新后的 `mds::exchange::binance::DepthUpdate` 在 `transaction_time_ms` 后、`bids/asks` 前携带：

- `int8_t price_exponent`；
- `int8_t quantity_exponent`；
- `std::array<char,32> symbol`。

`BookTicker` 也携带相同两个 exponent 和固定 32 字节 symbol（symbol 位于时间字段之后）。JSON bookTicker/depth 都写入 `-price_scale/-quantity_scale` 并从 `s` 填 symbol；Spot SBE `DepthDiffStreamEvent`/`BestBidAskStreamEvent` 从消息直接解码 exponent 与 symbol。symbol 必须非空且长度 `<32`，目标数组先清零再复制，因此成功时可安全作为 NUL 结尾字符串用于多标的路由。

`DepthUpdate` 含 `std::vector`，且这些 adapter 类型没有固定 `sizeof/offsetof` 契约，只用于进程内数据流；symbol/exponent **没有加入** `utils::md::wire::RecordHeader`、`DeltaRecord`、`BboRecord` 或其他共享内存 wire struct。因此 market-data wire schema 仍为 1.0，消费者继续通过 `instrument_id` 查找 `Instrument` 的 symbol 与 scale。若未来要逐记录携带这些字段，必须按第 9 节升级 schema，不能直接复制进程内结构。

## 5. `utils::md::wire` 记录

### 5.1 公共 `RecordHeader`（72 字节，对齐 8）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 4 | `uint32_t` | `magic` | 固定 `0x444d5153`；little-endian 内存字节为 `53 51 4d 44`，ASCII `"SQMD"` |
| 4 | 2 | `uint16_t` | `schema_major` | 当前 2 |
| 6 | 2 | `uint16_t` | `schema_minor` | 当前 0 |
| 8 | 2 | `uint16_t` | `message_type` | `MessageType` |
| 10 | 2 | `uint16_t` | `record_length` | 完整内层 wire struct 长度 |
| 12 | 4 | `uint32_t` | `reserved` | 写方写零，读方忽略 |
| 16 | 8 | `uint64_t` | `instrument_id` | instrument ID |
| 24 | 8 | `uint64_t` | `bus_seq` | 总线序列 |
| 32 | 8 | `uint64_t` | `source_seq` | 交易所序列 |
| 40 | 8 | `uint64_t` | `exchange_ts_ns` | Unix epoch ns；源仅有 ms 时乘 1,000,000 |
| 48 | 8 | `uint64_t` | `receive_tsc` | 接收 TSC；不可用为 0 |
| 56 | 8 | `uint64_t` | `publish_tsc` | 发布 TSC；不可用为 0 |
| 64 | 4 | `uint32_t` | `book_generation` | 每次重建递增 |
| 68 | 1 | `uint8_t` | `state` | `BookState` 数值 |
| 69 | 1 | `uint8_t` | `source_id` | A/B 或连接来源，由部署约定 |
| 70 | 2 | `uint16_t` | `flags` | 当前未定义；写零 |

`MakeHeader(type,length)` 只填 magic/schema/type/length，其余字段均为 0。

### 5.2 `BboRecord`（104 字节）

公共 header 位于 0..71。

| offset | size | 类型 | 字段 |
|---:|---:|---|---|
| 72 | 8 | `int64_t` | `bid_price` |
| 80 | 8 | `int64_t` | `bid_quantity` |
| 88 | 8 | `int64_t` | `ask_price` |
| 96 | 8 | `int64_t` | `ask_quantity` |

### 5.3 `TickerRecord`（176 字节）

| offset | size | 类型 | 字段 |
|---:|---:|---|---|
| 0 | 72 | `RecordHeader` | `header` |
| 72 | 8 | `int64_t` | `bid_price` |
| 80 | 8 | `int64_t` | `bid_quantity` |
| 88 | 8 | `int64_t` | `ask_price` |
| 96 | 8 | `int64_t` | `ask_quantity` |
| 104 | 8 | `int64_t` | `last_price` |
| 112 | 8 | `int64_t` | `last_quantity` |
| 120 | 8 | `int64_t` | `mark_price` |
| 128 | 8 | `int64_t` | `index_price` |
| 136 | 8 | `int64_t` | `funding_rate` |
| 144 | 8 | `int64_t` | `open_price` |
| 152 | 8 | `int64_t` | `high_price` |
| 160 | 8 | `int64_t` | `low_price` |
| 168 | 8 | `int64_t` | `close_price` |

当前 adapter 只解析 book ticker/depth，没有填充完整 `TickerRecord` 所需的 last/mark/index/funding 数据流。

### 5.4 `DeltaRecord`（96 字节）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 72 | `RecordHeader` | `header` | type=BookDelta |
| 72 | 1 | `uint8_t` | `side` | Bid=1/Ask=2 |
| 73 | 7 | `uint8_t[7]` | `reserved` | 写零 |
| 80 | 8 | `int64_t` | `price` | price mantissa |
| 88 | 8 | `int64_t` | `quantity` | 0 删除 |

### 5.5 `SnapshotControlRecord`（80 字节）

`SnapshotBeginRecord` 与 `SnapshotEndRecord` 都是该结构的 alias，字段名不随 alias 改变。

| offset | size | 类型 | Begin 语义 | End 语义 |
|---:|---:|---|---|---|
| 0 | 72 | `RecordHeader` | type=SnapshotBegin | type=SnapshotEnd |
| 72 | 4 | `uint32_t item_count` | 总 level 数 | 已接收/发布 level 数 |
| 76 | 4 | `uint32_t chunk_count_or_checksum` | chunk 总数 | checksum |

当前代码没有构建或验证快照 control 记录，也没有定义 checksum 算法；End 的 32 位值不能被称为 CRC。

### 5.6 `SnapshotChunkRecord`（464 字节）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 72 | `RecordHeader` | `header` | type=SnapshotChunk |
| 72 | 4 | `uint32_t` | `chunk_index` | 每个 side 独立从 0 开始的 chunk 序号 |
| 76 | 2 | `uint16_t` | `level_count` | 本 chunk 有效档数，必须 ≤24 |
| 78 | 1 | `uint8_t` | `side` | `Bid=1`、`Ask=2` |
| 79 | 1 | `uint8_t` | `reserved` | 写零 |
| 80 | 384 | `Level[24]` | `levels` | 每项 16 字节；未用项写零 |

### 5.7 `InstrumentUpdateRecord`（296 字节）

| offset | size | 类型 | 字段 |
|---:|---:|---|---|
| 0 | 72 | `RecordHeader` | `header` |
| 72 | 224 | `Instrument` | `instrument`；其子字段 offset 为本规范 4.4 中 offset +72 |

### 5.8 `InstrumentCatalogRecord`（472 字节）

header 后携带 400 字节定长 catalog：canonical identity、tick/lot/scale、
64 字节 market slug、80 字节 venue symbol/token ID、72 字节 condition ID、
outcome 与到期时间。Polymarket rollover 必须先发布同 generation 的 catalog，
再发布 `InstrumentUpdate`，最后才允许 BBO/OrderBook。

## 6. 共享段布局

### 6.1 `SegmentHeader`（4160 字节，对齐 64）

| offset | size | 类型 | 字段 | 语义/并发 |
|---:|---:|---|---|---|
| 0 | 8 | `atomic<uint64_t>` | `magic` | `0x5344514d44535247`；创建者最后 release-store，attach 者 acquire-load |
| 8 | 4 | `uint32_t` | `schema_major` | 当前 5 |
| 12 | 4 | `uint32_t` | `header_bytes` | 当前 `align8(sizeof)=4160` |
| 16 | 8 | `uint64_t` | `ring_bytes` | 2 的幂 |
| 24 | 8 | `uint64_t` | `max_record_bytes` | ≥40 且 ≤ ring/8 |
| 32 | 8 | `uint64_t` | `epoch` | 创建时非零 token，区分重建 |
| 40 | 8 | `atomic<uint64_t>` | `writer_cursor` | 单调逻辑字节 cursor |
| 48 | 8 | `atomic<uint64_t>` | `next_sequence` | 初值 1 |
| 56 | 4 | `uint32_t` | `max_readers` | 1..64 |
| 60 | 4 | `atomic<uint32_t>` | `registry_generation` | reader 注册、注销或回收成功后递增，供 producer 检测 late attach |
| 64 | 4096 | `ReaderSlot[64]` | `readers` | slot i 起点=`64+64i`；仅前 max_readers 可用 |

ring 数据起点为 mapping + `header_bytes`，mapping 总长为 `header_bytes + ring_bytes`。

### 6.2 `ReaderSlot`（64 字节，对齐 64）

| slot 内 offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 4 | `atomic<uint32_t>` | `state` | ReaderState；Free→Initializing 用 CAS，Active 以 release 发布 |
| 4 | 4 | `atomic<uint32_t>` | `pid` | 注册进程 PID |
| 8 | 8 | `atomic<uint64_t>` | `process_start_marker` | Linux `/proc/<pid>/stat` field 22，防 PID 重用 |
| 16 | 8 | `atomic<uint64_t>` | `lease_token` | 非零 handle token |
| 24 | 8 | `atomic<uint64_t>` | `cursor` | 下一待读取的逻辑字节位置 |
| 32 | 8 | `atomic<uint64_t>` | `heartbeat_ns` | 调用方提供的单调 ns；各方必须使用同一时钟域 |
| 40 | 24 | `byte[24]` | `reserved` | 写零 |

注册 reader 从当前 `writer_cursor` 开始，因此不会读注册前历史记录。`ReaderHandle` 不是共享布局，其值为 slot/token/epoch；三者共同验证身份。

### 6.3 ring `RecordHeader`（40 字节，对齐 8）

| offset | size | 类型 | 字段 | 语义 |
|---:|---:|---|---|---|
| 0 | 4 | `uint32_t` | `length` | header+payload+零 padding，8 字节对齐 |
| 4 | 4 | `uint32_t` | `type` | 0=padding；其他由上层定义 |
| 8 | 4 | `uint32_t` | `payload_bytes` | 不含 padding |
| 12 | 4 | `uint32_t` | `payload_crc32c` | payload 有效字节（不含 header/对齐 padding）的 Castagnoli CRC32C |
| 16 | 8 | `uint64_t` | `epoch` | 必须等于 segment epoch |
| 24 | 8 | `uint64_t` | `sequence` | 正常记录从 1 单调；padding 为 0 |
| 32 | 8 | `atomic<uint64_t>` | `commit_sequence` | 0=未提交；正常记录=sequence；padding 当前写 1 |

## 7. 原子与 commit 协议

### 7.1 段创建

创建者清零 mapping、placement-new header、填写不可变布局字段和 epoch，最后对 `magic` 做 release-store。attacher 首先 acquire-load magic，再读取并严格校验 schema/layout/epoch/ring 几何和所有共享原子是否 lock-free。半初始化段必须拒绝。

### 7.2 写记录

1. 单生产者读取 `writer_cursor`。
2. acquire 读取 reader 状态/cursor，任何 Initializing/Suspect 或空间不足返回 `QuotaExceeded`。
3. 若尾部不足且至少可放 40 字节，写 type=0 padding 并 release 提交；不足 40 字节则隐式跳过尾部。
4. `next_sequence.fetch_add(1, relaxed)` 分配序列。
5. 对调用方 payload 计算 CRC32C，写入 header 的 `payload_crc32c`；padding record 的该字段保持 0。
6. 写 `commit_sequence=0`，复制 payload，清零对齐 padding。
7. `commit_sequence.store(sequence, release)`。
8. `writer_cursor.store(next_cursor, release)`。

### 7.3 读与消费

reader acquire-load writer cursor；按 cursor 定位并跳过尾部/padding；先校验长度，再 acquire-load `commit_sequence` 并要求等于 `sequence`，校验 epoch/payload 长度，最后重新计算 payload CRC32C 并与 header 比较。CRC 不匹配返回 `InternalError` 和 `"record payload CRC32C mismatch"`，不创建 lease、不推进 reader cursor。`ReadLease` 的 payload view 在 `commit()`、`release()` 或析构前有效：

- `commit()`：release-store reader cursor，消费记录；
- `release()`/析构：不推进 cursor，下次再次返回同一记录；
- outstanding lease 会阻止生产者覆盖对应字节。

当前 API 没有阻止同一 `ReaderHandle` 同时发起多个 `read()`；调用方必须串行持有/提交 lease，否则可能得到同一记录并出现 stale cursor。共享环 `publish()` 只允许一个生产线程。

## 8. CRC、完整性和损坏检测现状

- ring schema 3 的每个正常 record 都有 payload CRC32C：Castagnoli reflected polynomial `0x82f63b78`，初值 `0xffffffff`，逐字节/逐 bit 处理，最终按位取反。覆盖范围严格为 `payload_bytes`，不含 ring header 和 8 字节对齐 padding。
- producer 在 publish 时计算并保存 CRC；reader 在 commit-sequence acquire 后、暴露 payload 前重新计算并验证。当前是可移植软件实现；硬件加速替换必须保持 golden vector 一致。
- padding record 没有 payload，`payload_crc32c=0`，reader 按 type=0 跳过，不执行 payload CRC 路径。
- 内层 `utils::md::wire` header 本身没有独立 CRC，但当它作为 ring payload 发布时，完整内层记录由外层 CRC32C 覆盖。
- `SnapshotEnd` 的 `chunk_count_or_checksum` 仍没有定义独立 snapshot checksum 算法；该字段不能与 ring 的 payload CRC32C 混为一谈。
- `SequenceArbiter` 的 `payload_hash` 由调用方提供，当前没有 hash 算法，也没有接入 ring。
- CRC32C 用于偶发损坏检测，不提供防篡改认证；跨机器传输、磁盘持久化或恶意共享内存写入不在完整性保证内。

## 9. schema 兼容与 N/N-1

### 9.1 当前事实

- ring 只有 major，没有 minor；attacher 只接受当前 `schema_major == 4`，即只支持精确 N，不支持 N-1。旧段必须拒绝，不能按当前布局解释。
- wire 当前为 1.1，并提供 `ValidateHeader` / `Decode`。消费者至少校验 magic、major=1、最低 minor 与 `record_length`；完整消费规则以 `segment_and_schema_contract.md` 为准。

### 9.2 发布规则

- major：任何 offset、size、字段类型、枚举语义、原子协议或删除/重排字段的变化必须升 major，并使用新共享段名。
- minor：只允许既有记录尾部追加可忽略字段，且 decoder 以 `record_length` 做边界检查；在这种 decoder 落地前，即使只升 minor也仍按“不兼容”处理。
- 目标 N/N-1：生产部署若要求滚动升级，生产者应双写 N 和 N-1 两个不同段，消费者只 attach 自己精确支持的 major；不得让不同 major 进程解释同一段。当前代码未实现双写。
- 生产发布段统一由 `make_publisher_segment_name()` 生成，格式为
  `<prefix>.<profile>.<symbol>.<ticker|orderbook>.<wire_major>`。规范化和
  版本策略的唯一消费方契约见
  [`segment_and_schema_contract.md`](segment_and_schema_contract.md)。

## 10. 实现来源

- `utils/include/utils/md/types.h`
- `utils/include/utils/md/wire.h`
- `mds/include/mds/transport/shared_ring.h`
- `mds/src/transport/shared_ring.cpp`

本规范记录当前 ABI；任何编译器、标准库或字段改动后都必须重新运行 `sizeof/offsetof` 静态校验。

## 11. Polymarket rolling MDS 映射

Polymarket 行情使用 `Venue::Polymarket` 与 `ProductType::BinaryOption`。MDS
为 BTC 五分钟方向市场暴露稳定 alias `BTC5MUP`、`BTC5MDOWN`；alias 是
MDS-only 标识，不是 Polymarket token，也不能直接作为 OMS 下单标的。
Gamma REST 仅异步发现当前精确市场及 token；token ID 保留在 adapter
内部，不加入共享 wire。

滚动到下一市场时，`instrument_id`、`canonical_symbol`、`instrument_key`
及共享段保持 alias 身份稳定；`venue_symbol` 可携带当前 Gamma slug。slug
只能伴随 `book_generation` 递增及 `InstrumentUpdate`（`state=Building`）
一起变化，消费者必须清空旧 book，等待新一代快照恢复 Live。

Polymarket 的 BBO、BookDelta、Snapshot 和 InstrumentUpdate 继续使用本规范
既有记录；不增加 token、slug 或 lifecycle 专用 wire 字段，BBO/Book wire
schema 与大小不变。WSS 的 `book` snapshot、`price_change`、BBO、tick-size
及 lifecycle 事件映射到既有状态机和记录。
