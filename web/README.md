# Nuts Quant Web

数字资产量化平台的静态前端原型。当前包含完整的资金费机会页面，以及聚合盘口、实盘交易、账户管理和报表分析的页面入口。

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
10 秒获取一份完整快照。搜索、交易所、持仓量、成交额、结算周期和费率方向筛选
均在浏览器本地执行；刷新失败时保留上一份成功快照。浏览器不直接访问交易所或
C++ MDS。
