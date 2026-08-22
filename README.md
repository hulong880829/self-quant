# self-quant

self-quant 是一个单仓库量化系统，包含：

- [`core/`](core/README.md)：C++20 低延迟行情接入、共享内存行情总线和可安装 SDK。
- [`backend/`](backend/README.md)：Go 多服务后端，包含资金费、账户、Polymarket、
  报表、交易、AI、行情聚合与 HTTP/gRPC Gateway。
- [`web/`](web/README.md)：Next.js 资金费与量化管理界面。

## 支持环境

一键初始化脚本面向 Ubuntu 24.04 LTS，固定使用：

- Go 1.25.1
- Node.js 24.19.0
- CMake 3.20 或更高版本
- C++20、Ninja、OpenSSL、simdjson
- Docker Engine、Docker Compose、PostgreSQL 17、Redis 7 容器

## 新机器初始化

```bash
git clone <YOUR_REPOSITORY_URL> self-quant
cd self-quant
./scripts/bootstrap.sh
```

首次安装 Docker 后需要重新登录，或执行：

```bash
newgrp docker
```

Bootstrap 可重复执行，不会覆盖已有的 `backend/.env` 或
`web/.env.local`。可用选项：

```bash
./scripts/bootstrap.sh --skip-docker
./scripts/bootstrap.sh --skip-deps
./scripts/bootstrap.sh --no-env-files
```

## 统一构建与验证

默认构建并验证三个组件：

```bash
./scripts/build.sh
```

常用选项：

```bash
./scripts/build.sh --core-only
./scripts/build.sh --backend-only
./scripts/build.sh --web-only
./scripts/build.sh --clean --jobs 4
./scripts/build.sh --debug --core-only
./scripts/build.sh --skip-tests
```

默认验证包含：

- Core：Werror Release build 和全部 CTest。
- Backend：8 个服务二进制、`go test ./...`、`go vet ./...`。
- Web：ESLint、TypeScript typecheck 和 Next.js production build。

生成物位于 `core/build/`、`backend/build/` 和 `web/.next/`，均不会进入
Git。

## 本地运行

启动 PostgreSQL 与 Redis：

```bash
docker compose -f backend/deploy/docker-compose.yml up -d postgres redis
```

Compose 同时提供 Postgres（`5432`）与 Redis（`6379`）。Redis 为无持久化实例（RDB/AOF
均关闭），供后续行情录制等热数据使用；容器重建后数据清空，可重新录制。

可选 ClickHouse（供 Core MDS BBO 写入，与 backend 业务库无关）：

```bash
backend/deploy/clickhouse/generate-certs.sh
export CLICKHOUSE_BBO_PASSWORD='your-password'
docker compose -f backend/deploy/docker-compose.yml up -d clickhouse
```

详见 [`backend/deploy/clickhouse/README.md`](backend/deploy/clickhouse/README.md) 与
[`core/README.md`](core/README.md) 中的 `--clickhouse-bbo` 说明。

首次运行先准备环境变量：

```bash
cp backend/.env.example backend/.env
```

至少替换以下占位值：

- `ACCOUNT_TOKEN_SECRET`
- `ACCOUNT_CREDENTIALS_KEY`
- `REPORT_INTERNAL_TOKEN`
- `TRADER_INTERNAL_TOKEN`

使用 aggdata/MDS 时还需配置 `MDS_GATEWAY_TOKEN` 与
`AGGDATA_RECORDING_DIR`。真实 `.env` 只保存在本机，不能提交。

先执行 `./scripts/build.sh`。随后在不同终端中进入 `backend/`、加载同一份
`.env`，并按以下顺序启动：

```bash
cd backend
set -a; source .env; set +a

# 1. 先启动会执行数据库 migration 的服务。
./build/bin/funding-service
./build/bin/account-service

# 2. 再分别启动业务服务。
./build/bin/polymarket-service
./build/bin/report-service
./build/bin/trader-service
./build/bin/ai-service

# 3. aggdata-service 为可选；最后启动依赖上述服务的 Gateway。
./build/bin/aggdata-service
./build/bin/api-gateway
```

以上每个长期运行命令应在独立终端执行。`aggdata-service` 仅在需要聚合行情、
录制或订单簿功能时启动。最后启动 Web：

```bash
cd web
npm run dev
```

默认地址：

- Web：<http://localhost:3000/funding>
- API Gateway：<http://localhost:8080>
- Funding gRPC：`localhost:9090`
- Account gRPC：`localhost:9091`
- Polymarket gRPC：`localhost:9092`
- Aggdata HTTP/WebSocket：`127.0.0.1:9093`（可选）
- Report gRPC：`localhost:9094`
- Trader gRPC：`localhost:9095`
- AI gRPC：`localhost:9096`

API Gateway 会连接 Funding、Account、Polymarket、Report、Trader 与 AI；
这些依赖未就绪时 `/health/ready` 会返回未就绪。更完整的服务配置和接口说明见
[`backend/README.md`](backend/README.md)。

停止本地基础设施：

```bash
docker compose -f backend/deploy/docker-compose.yml down
```

## Core SDK 验证

标准 Release 构建：

```bash
./scripts/build.sh --core-only
```

验证安装后的 `find_package(self_quant)`，分别覆盖 MDS 开启和关闭：

```bash
core/tests/install_package_smoke.sh "$PWD/core" \
  "$PWD/core/build/package-smoke-on" ON
core/tests/install_package_smoke.sh "$PWD/core" \
  "$PWD/core/build/package-smoke-off" OFF
```

只构建可复用 utils：

```bash
cmake -S core -B core/build/utils-release -G Ninja \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=OFF
cmake --build core/build/utils-release --parallel
ctest --test-dir core/build/utils-release --output-on-failure
```

## Git 内容政策

必须提交：

- 源码、CMake/config、配置 YAML、文档、测试 fixture 和 Docker Compose。
- `backend/go.mod`、`backend/go.sum`。
- `web/package.json`、`web/package-lock.json`。
- `backend/gen/` 下生成的 protobuf Go 源码。
- 所有 `.env.example`，但不能提交真实 `.env`。

禁止提交：

- `core/.deps/`、任何 build 目录。
- `web/node_modules/`、`.next/`。
- 本地 `.env`、密钥、可执行文件、日志、coverage、IDE 和临时文件。

首次关联远程仓库：

```bash
git remote add origin <YOUR_REPOSITORY_URL>
git add .
git status
git commit -m "Initialize self-quant monorepo"
git push -u origin main
```

执行 commit 前必须检查 `git status`，确认没有依赖缓存、构建产物或本地
配置文件。
