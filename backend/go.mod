module selfquant/backend

go 1.25.1

require (
	github.com/ethereum/go-ethereum v1.17.5
	github.com/go-chi/chi/v5 v5.2.3
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgx/v5 v5.7.6
	github.com/klauspost/compress v1.19.2
	github.com/shopspring/decimal v1.4.0
	golang.org/x/crypto v0.48.0
	golang.org/x/sync v0.22.0
	golang.org/x/time v0.15.0
	google.golang.org/grpc v1.79.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/ClickHouse/ch-go v0.67.0 // indirect
	github.com/ClickHouse/clickhouse-go/v2 v2.40.1 // indirect
	github.com/ProjectZKM/Ziren/crates/go-runtime/zkvm_runtime v0.0.0-20251001021608-1fe7b43fc4d6 // indirect
	github.com/Simon-Busch/hyperliquid-go v0.1.2 // indirect
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/consensys/gnark-crypto v0.18.1 // indirect
	github.com/crate-crypto/go-eth-kzg v1.5.0 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0 // indirect
	github.com/elliottech/lighter-go v1.0.8 // indirect
	github.com/elliottech/poseidon_crypto v0.0.15 // indirect
	github.com/ethereum/c-kzg-4844/v2 v2.1.8 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/holiman/uint256 v1.3.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/paulmach/orb v0.11.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/segmentio/asm v1.2.0 // indirect
	github.com/supranational/blst v0.3.16 // indirect
	github.com/valyala/fastjson v1.6.4 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	go.opentelemetry.io/otel v1.41.0 // indirect
	go.opentelemetry.io/otel/trace v1.41.0 // indirect
	golang.org/x/net v0.50.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260209200024-4cfbd4190f57 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/Simon-Busch/hyperliquid-go => ./third_party/hyperliquid-go
