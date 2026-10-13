# 第三方 Broker 接入

第三方只通过自己的后端调用 API Gateway。Web 与 App 不得持有 `client_secret` 或长期 Broker JWT。

公开文档：`GET /swagger/` 与 `GET /swagger/openapi.json`。Funding / Spread 无需登录；OAuth、Broker、交易账户与交易接口需要鉴权。Admin 仅供本站 Cookie 管理面，不是第三方主路径。

金额、数量、价格、费率一律使用定点十进制字符串。运行时状态（`tradingStatus`、`runtimeState`、订单/TWAP/套利 `status`、`oneShotPhase`）后续可能扩展，客户端必须容忍未知值。

## 接入顺序

1. `POST /api/v1/oauth/token` 用 Client Credentials 换 Token。
2. `POST /api/v1/broker/platform-accounts` 创建本租户平台账户。
3. `POST /api/v1/trading-accounts` 绑定交易账户（`Authorization: Bearer` + `X-SelfQuant-Account`）。
4. `POST /api/v1/trading-accounts/{id}/trading-readiness` 确认 `tradingReady`。
5. `GET /api/v1/trader/accounts/{accountId}/instruments?type=perpetual` 取得 `instrumentId`。
6. 可选 `POST /api/v1/trader/accounts/{accountId}/account-profile/apply`。
7. `POST /api/v1/trader/arbitrage-combinations`（`Idempotency-Key` 必填）。不要发送 `productName`。
8. 轮询 `GET /api/v1/trader/arbitrage-combinations/{id}`。
9. 需要时 `PATCH` 可更新 `askThresholdBps` / `bidThresholdBps` / `targetNotional`；one_shot 还可改已启用的 `exitAnnualizedRate`、`earlyExitFunding8hAnnualizedFloor` 和 `exitAfterSeconds`。省略字段表示不改，空字符串非法。新 one_shot 创建必须 `exitPolicy=time` 并提供合法周期。
10. `DELETE` 关闭，再轮询直到 `status` 为 `closed` 或 `failed`，并忽略未知 `runtimeState`。

可用 openapi-generator、orval 或 openapi-typescript 根据 `/swagger/openapi.json` 生成客户端。生成客户端遇到未知状态时不得反序列化失败。

## 架构

```text
Third-party backend
  -> POST /api/v1/oauth/token   (HTTP Basic client_id:client_secret)
  -> Broker Bearer + X-SelfQuant-Account
  -> API Gateway userSession() 签发委托会话（不会出现在 HTTP 响应或 Cookie）
  -> account-service / trader-service
```

Cookie 与 Bearer 不能同时出现，否则 401。Broker 管理面（`/api/v1/broker/*`）不要带 `X-SelfQuant-Account`。跨租户账户返回 404，不泄露租户。

## 错误处理

- `429`：降低频率并退避。
- `503`：admin 或依赖不可用，稍后重试。
- `504` 且 `error` 为 `order result is uncertain`：先查单，禁止盲下第二笔。
- 业务判断优先看可选字段 `code`，不要解析英文 `error` 文案。

当前已知 `code`：`invalid_leverage`、`insufficient_margin`、`insufficient_spot_balance`、`position_capacity_exceeded`、`leverage_apply_failed`、`venue_unavailable`、`active_arbitrage_instrument_conflict`。

## curl

下列值全部虚构。

```bash
TOKEN=$(curl -sS -u 'br_example_client:example-secret' \
  -d 'grant_type=client_credentials' \
  https://api.example.test/api/v1/oauth/token | jq -r .access_token)

curl -sS -X POST https://api.example.test/api/v1/broker/platform-accounts \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"username":"desk-alpha","password":"example-pass"}'

curl -sS -X POST https://api.example.test/api/v1/trading-accounts \
  -H "Authorization: Bearer $TOKEN" \
  -H 'X-SelfQuant-Account: desk-alpha' \
  -H 'Content-Type: application/json' \
  -d '{
    "productName":"Demo Book",
    "exchange":"binance",
    "accountName":"main",
    "apiKey":"example-key",
    "apiSecret":"example-secret"
  }'

curl -sS -X POST https://api.example.test/api/v1/trader/arbitrage-combinations \
  -H "Authorization: Bearer $TOKEN" \
  -H 'X-SelfQuant-Account: desk-alpha' \
  -H 'Idempotency-Key: 9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d' \
  -H 'Content-Type: application/json' \
  -d '{
    "legAAccountId": 101,
    "legAInstrumentId": 1001,
    "legBAccountId": 102,
    "legBInstrumentId": 1002,
    "askThresholdBps": "12",
    "bidThresholdBps": "-8",
    "targetNotional": "10000",
    "executionMode": "maker_then_hedge",
    "preferredLeg": "a",
    "runMode": "spread"
  }'
```

## TypeScript（服务端）

```ts
const basic = Buffer.from("br_example_client:example-secret").toString("base64");
const tokenRes = await fetch("https://api.example.test/api/v1/oauth/token", {
  method: "POST",
  headers: {
    Authorization: `Basic ${basic}`,
    "Content-Type": "application/x-www-form-urlencoded",
  },
  body: "grant_type=client_credentials",
});
const { access_token } = (await tokenRes.json()) as { access_token: string };

await fetch("https://api.example.test/api/v1/trader/orders", {
  method: "POST",
  headers: {
    Authorization: `Bearer ${access_token}`,
    "X-SelfQuant-Account": "desk-alpha",
    "Idempotency-Key": "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
    "Content-Type": "application/json",
  },
  body: JSON.stringify({
    tradingAccountId: 101,
    instrumentId: 1001,
    side: "buy",
    orderType: "limit",
    quantity: "0.001",
    price: "65000.1",
  }),
});
```

浏览器与 App 不要直接调用上述接口。

## Go

```go
req, _ := http.NewRequest(http.MethodPost, "https://api.example.test/api/v1/oauth/token", strings.NewReader("grant_type=client_credentials"))
req.SetBasicAuth("br_example_client", "example-secret")
req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
```

换到 Token 后，后续请求设置：

```text
Authorization: Bearer <access_token>
X-SelfQuant-Account: desk-alpha
Idempotency-Key: <uuid>   // 下单、TWAP、创建套利
```
# 第三方 Broker 接入

第三方只通过自己的后端调用 API Gateway。Web 与 App 不得持有 `client_secret` 或长期 Broker JWT。

公开文档：`GET /swagger/` 与 `GET /swagger/openapi.json`。Funding / Spread 无需登录；OAuth、Broker、交易账户与交易接口需要鉴权。Admin 仅供本站 Cookie 管理面，不是第三方主路径。

金额、数量、价格、费率一律使用定点十进制字符串。运行时状态（`tradingStatus`、`runtimeState`、订单/TWAP/套利 `status`、`oneShotPhase`）后续可能扩展，客户端必须容忍未知值。

## 接入顺序

1. `POST /api/v1/oauth/token` 用 Client Credentials 换 Token。
2. `POST /api/v1/broker/platform-accounts` 创建本租户平台账户。
3. `POST /api/v1/trading-accounts` 绑定交易账户（`Authorization: Bearer` + `X-SelfQuant-Account`）。
4. `POST /api/v1/trading-accounts/{id}/trading-readiness` 确认 `tradingReady`。
5. `GET /api/v1/trader/accounts/{accountId}/instruments?type=perpetual` 取得 `instrumentId`。
6. 可选 `POST /api/v1/trader/accounts/{accountId}/account-profile/apply`。
7. `POST /api/v1/trader/arbitrage-combinations`（`Idempotency-Key` 必填）。不要发送 `productName`。
8. 轮询 `GET /api/v1/trader/arbitrage-combinations/{id}`。
9. 需要时 `PATCH` 可更新 `askThresholdBps` / `bidThresholdBps` / `targetNotional`；one_shot 还可改已启用的 `exitAnnualizedRate`、`earlyExitFunding8hAnnualizedFloor` 和 `exitAfterSeconds`。省略字段表示不改，空字符串非法。新 one_shot 创建必须 `exitPolicy=time` 并提供合法周期。
10. `DELETE` 关闭，再轮询直到 `status` 为 `closed` 或 `failed`，并忽略未知 `runtimeState`。

可用 openapi-generator、orval 或 openapi-typescript 根据 `/swagger/openapi.json` 生成客户端。生成客户端遇到未知状态时不得反序列化失败。

## 架构

```text
Third-party backend
  -> POST /api/v1/oauth/token   (HTTP Basic client_id:client_secret)
  -> Broker Bearer + X-SelfQuant-Account
  -> API Gateway userSession() 签发委托会话（不会出现在 HTTP 响应或 Cookie）
  -> account-service / trader-service
```

Cookie 与 Bearer 不能同时出现，否则 401。Broker 管理面（`/api/v1/broker/*`）不要带 `X-SelfQuant-Account`。跨租户账户返回 404，不泄露租户。

## 错误处理

- `429`：降低频率并退避。
- `503`：admin 或依赖不可用，稍后重试。
- `504` 且 `error` 为 `order result is uncertain`：先查单，禁止盲下第二笔。
- 业务判断优先看可选字段 `code`，不要解析英文 `error` 文案。

当前已知 `code`：`invalid_leverage`、`insufficient_margin`、`insufficient_spot_balance`、`position_capacity_exceeded`、`leverage_apply_failed`、`venue_unavailable`、`active_arbitrage_instrument_conflict`。

## curl

下列值全部虚构。

```bash
TOKEN=$(curl -sS -u 'br_example_client:example-secret' \
  -d 'grant_type=client_credentials' \
  https://api.example.test/api/v1/oauth/token | jq -r .access_token)

curl -sS -X POST https://api.example.test/api/v1/broker/platform-accounts \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"username":"desk-alpha","password":"example-pass"}'

curl -sS -X POST https://api.example.test/api/v1/trading-accounts \
  -H "Authorization: Bearer $TOKEN" \
  -H 'X-SelfQuant-Account: desk-alpha' \
  -H 'Content-Type: application/json' \
  -d '{
    "productName":"Demo Book",
    "exchange":"binance",
    "accountName":"main",
    "apiKey":"example-key",
    "apiSecret":"example-secret"
  }'

curl -sS -X POST https://api.example.test/api/v1/trader/arbitrage-combinations \
  -H "Authorization: Bearer $TOKEN" \
  -H 'X-SelfQuant-Account: desk-alpha' \
  -H 'Idempotency-Key: 9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d' \
  -H 'Content-Type: application/json' \
  -d '{
    "legAAccountId": 101,
    "legAInstrumentId": 1001,
    "legBAccountId": 102,
    "legBInstrumentId": 1002,
    "askThresholdBps": "12",
    "bidThresholdBps": "-8",
    "targetNotional": "10000",
    "executionMode": "maker_then_hedge",
    "preferredLeg": "a",
    "runMode": "spread"
  }'
```

## TypeScript（服务端）

```ts
const basic = Buffer.from("br_example_client:example-secret").toString("base64");
const tokenRes = await fetch("https://api.example.test/api/v1/oauth/token", {
  method: "POST",
  headers: {
    Authorization: `Basic ${basic}`,
    "Content-Type": "application/x-www-form-urlencoded",
  },
  body: "grant_type=client_credentials",
});
const { access_token } = (await tokenRes.json()) as { access_token: string };

await fetch("https://api.example.test/api/v1/trader/orders", {
  method: "POST",
  headers: {
    Authorization: `Bearer ${access_token}`,
    "X-SelfQuant-Account": "desk-alpha",
    "Idempotency-Key": "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
    "Content-Type": "application/json",
  },
  body: JSON.stringify({
    tradingAccountId: 101,
    instrumentId: 1001,
    side: "buy",
    orderType: "limit",
    quantity: "0.001",
    price: "65000.1",
  }),
});
```

浏览器与 App 不要直接调用上述接口。

## Go

```go
req, _ := http.NewRequest(http.MethodPost, "https://api.example.test/api/v1/oauth/token", strings.NewReader("grant_type=client_credentials"))
req.SetBasicAuth("br_example_client", "example-secret")
req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
```

换到 Token 后，后续请求设置：

```text
Authorization: Bearer <access_token>
X-SelfQuant-Account: desk-alpha
Idempotency-Key: <uuid>   // 下单、TWAP、创建套利
```
