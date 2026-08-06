# self-quant

self-quant 是一个单仓库量化系统，包含：

- [`core/`](core/README.md)：C++20 低延迟行情接入、共享内存行情总线和可安装 SDK。
- [`backend/`](backend/README.md)：Go 资金费同步服务与 HTTP/gRPC Gateway。
- [`web/`](web/README.md)：Next.js 资金费与量化管理界面。

## 支持环境

一键初始化脚本面向 Ubuntu 24.04 LTS，固定使用：

- Go 1.25.1
- Node.js 24.19.0
- CMake 3.20 或更高版本
- C++20、Ninja、OpenSSL、simdjson
- Docker Engine、Docker Compose、PostgreSQL 17 容器

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
- Backend：两个可执行文件、`go test ./...`、`go vet ./...`。
- Web：ESLint、TypeScript typecheck 和 Next.js production build。

生成物位于 `core/build/`、`backend/build/` 和 `web/.next/`，均不会进入
Git。

## 本地运行

启动 PostgreSQL 17：

```bash
docker compose -f backend/deploy/docker-compose.yml up -d
```

启动资金费服务：

```bash
cd backend
set -a
source .env
set +a
go run ./cmd/funding-service
```

另开终端启动 API Gateway：

```bash
cd backend
set -a
source .env
set +a
go run ./cmd/api-gateway
```

再开终端启动 Web：

```bash
cd web
npm run dev
```

默认地址：

- Web：<http://localhost:3000/funding>
- API Gateway：<http://localhost:8080>
- Funding gRPC：`localhost:9090`

停止 PostgreSQL：

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

- 源码、CMake/config、文档、测试 fixture 和 Docker Compose。
- `backend/go.mod`、`backend/go.sum`。
- `web/package.json`、`web/package-lock.json`。
- `backend/gen/` 下生成的 protobuf Go 源码。
- 所有 `.env.example`，但不能提交真实 `.env`。

禁止提交：

- `core/.deps/`、任何 build 目录。
- `web/node_modules/`、`.next/`。
- 本地 `.env`、密钥、日志、coverage、IDE 和临时文件。

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
