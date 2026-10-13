package jev

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresSpots struct {
	pool *pgxpool.Pool
}

func NewPostgresSpots(pool *pgxpool.Pool) *PostgresSpots {
	return &PostgresSpots{pool: pool}
}

func (s *PostgresSpots) ListSpots(ctx context.Context) (map[string]SpotInstrument, error) {
	rows, err := s.pool.Query(ctx, `
SELECT lower(exchange), upper(exchange_symbol), upper(base_asset), upper(quote_asset), global_symbol
FROM instruments
WHERE active = TRUE
  AND lower(contract_type) = 'spot'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SpotInstrument{}
	for rows.Next() {
		var item SpotInstrument
		if err := rows.Scan(&item.Exchange, &item.ExchangeSymbol, &item.BaseAsset, &item.QuoteAsset, &item.GlobalSymbol); err != nil {
			return nil, err
		}
		item.Exchange = strings.ToLower(item.Exchange)
		out[spotKey(item.Exchange, item.BaseAsset, item.QuoteAsset)] = item
	}
	return out, rows.Err()
}
