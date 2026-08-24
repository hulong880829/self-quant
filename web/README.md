# 龙歪歪Quant Web

数字资产量化平台前端。当前包含完整的资金费机会页面、实时聚合盘口，以及
登录后可访问的实盘交易与账户管理入口。右上角支持登录/退出；会话通过 Gateway
的 HttpOnly Cookie 保持，刷新后仍保持登录状态。

## 本地运行

要求 Node.js 24 LTS 或更高版本。

```bash
npm install
cp .env.example .env.local
npm run dev
```

打开 [http://localhost:3000/funding](http://localhost:3000/funding)。

## 验证

```bash
npm run lint
npx tsc --noEmit
npm run build
```

## 数据边界

资金费页面通过 `NEXT_PUBLIC_API_BASE_URL` 调用 Go API Gateway，首次加载后每
60 秒校验一次快照 ETag；未变化时复用当前数据，选中合约后按需加载并缓存历史
资金费。搜索、交易所、USDT 持仓市值、成交额、结算周期和费率方向筛选均在浏览
器本地执行；刷新失败时保留上一份成功快照。登录相关请求使用
`credentials: "include"`。浏览器不直接访问交易所或 C++ MDS。

聚合盘口由浏览器直接连接 Go aggdata 服务。设置
`NEXT_PUBLIC_AGGDATA_BASE_URL`（默认 `http://127.0.0.1:9093`）；WebSocket 默认
由该地址推导为 `/v1/stream`，也可用 `NEXT_PUBLIC_AGGDATA_WS_URL` 单独覆盖。若
服务启用了 browser token，可设置公开的 `NEXT_PUBLIC_AGGDATA_TOKEN`。REST
使用 `/v1/markets`、`/v1/markets/{symbol}/snapshot?profile=...` 和
`/v1/markets/{symbol}/spread-history?profile=...&range=24h&type=gated`。
聚合盘口以 `profile + symbol` 区分同名现货和永续市场，WebSocket 订阅也会
同时发送这两个字段。

Polymarket 页面也直接使用 aggdata：通过 `channel:"fairprice"` 的 JSON
WebSocket 帧显示当前 Fair Price，并从
`/v1/markets/{symbol}/fair-price-history` 加载当前合约窗口内的历史折线。
`NEXT_PUBLIC_AGGDATA_FAIRPRICE_PROFILE` 用于在同 symbol 多 profile 时精确选择
聚合源，`NEXT_PUBLIC_AGGDATA_FAIRPRICE_QUOTE` 默认 `USDT`。实时值按序号去重并
最多每 100ms 刷新，图表每秒合并一次；历史查询失败不会影响实时 Fair Price 或
原有 Open/Chainlink 数据。

compact binary v1 与 `backend/internal/aggdata/browser.go` 对齐，细节集中记录在
`src/lib/api/aggdata.ts` 的 decoder 上方：小端 `SQAB` 帧头、帧级
price/quantity scale、逐档 int64 mantissa 和 slot 化 venue contributions。客户端
发送 `{op:"subscribe", symbol, channel:"orderbook", depth:50}`，服务端持续发送
完整 binary snapshot；wire 格式不泄漏到 provider 或页面组件。
