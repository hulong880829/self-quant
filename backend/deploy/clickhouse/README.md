# ClickHouse（MDS BBO 录制）

本目录为 [`../docker-compose.yml`](../docker-compose.yml) 中的可选 ClickHouse 服务提供
HTTPS 证书与配置。ClickHouse 供宿主机上的 `mds_shm_consumer --clickhouse-bbo` 写入
crypto BBO 快照，**不参与** backend Go 服务的数据库依赖。

## 首次准备

```bash
cd backend/deploy/clickhouse
./generate-certs.sh

# 让 MDS 客户端信任自签证书（Ubuntu；仅需一次）
sudo cp certs/server.crt /usr/local/share/ca-certificates/clickhouse-localhost.crt
sudo update-ca-certificates
```

## 启动 ClickHouse

在仓库根目录：

```bash
export CLICKHOUSE_BBO_PASSWORD='your-password'

docker compose -f backend/deploy/docker-compose.yml up -d clickhouse
```

检查：

```bash
curl -s http://127.0.0.1:8123/ping
curl -sk https://127.0.0.1:8443/ping
```

两者均应返回 `Ok.`。MDS 录制器连接 **HTTPS 8443**（见 [`core/README.md`](../../../core/README.md)）。

## 默认账号

| 项 | 值 |
|----|-----|
| Host | `127.0.0.1` |
| HTTPS 端口 | `8443` |
| 数据库 | `market_data` |
| 用户 | `bbo_writer` |
| 密码 | 环境变量 `CLICKHOUSE_BBO_PASSWORD`（未设置时 compose 默认 `changeme`） |

`mds_shm_consumer` 会在首次写入时自动 `CREATE TABLE`（需 INSERT 权限）。

## 与 MDS 联调

1. 启动 ClickHouse（上文）
2. 启动 producer，例如 `mds_producer --config core/mds/config/binance_spot.yaml`
3. 配置 [`core/mds/config/mds_clickhouse_bbo.example.yaml`](../../../core/mds/config/mds_clickhouse_bbo.example.yaml)：
   - `host: 127.0.0.1`
   - `service: "8443"`
   - `shm_prefix` 与 producer 的 `shared_memory.prefix` 一致
   - `selectors` 列出全部 multiplex shard
4. 启动录制：

```bash
export CLICKHOUSE_BBO_PASSWORD='your-password'
./core/build/release-bootstrap/mds/mds_shm_consumer \
  --config core/mds/config/mds_clickhouse_bbo.example.yaml \
  --clickhouse-bbo
```

`sample_interval_ms` 最低 **200**；1 秒采样设为 `1000`。
