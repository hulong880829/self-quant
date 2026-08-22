package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTradingAccountNotFound  = errors.New("trading account not found")
	ErrDuplicateTradingAccount = errors.New("trading account already exists")
)

type TradingAccountRecord struct {
	ID            int64
	OwnerUsername string
	ProductName   string
	Exchange      string
	AccountName   string
	APIKeyEnc     []byte
	APISecretEnc  []byte
	PassphraseEnc []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type TradingRepository struct {
	pool *pgxpool.Pool
}

func NewTradingRepository(pool *pgxpool.Pool) *TradingRepository {
	return &TradingRepository{pool: pool}
}

func (r *TradingRepository) ListByOwner(
	ctx context.Context,
	ownerUsername string,
) ([]TradingAccountRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_username, product_name, exchange, account_name,
		       api_key_enc, api_secret_enc, passphrase_enc, created_at, updated_at
		FROM trading_accounts
		WHERE owner_username=$1
		ORDER BY product_name ASC, exchange ASC, account_name ASC, id ASC`, ownerUsername)
	if err != nil {
		return nil, fmt.Errorf("list trading accounts: %w", err)
	}
	defer rows.Close()

	result := make([]TradingAccountRecord, 0)
	for rows.Next() {
		var record TradingAccountRecord
		if err := rows.Scan(
			&record.ID,
			&record.OwnerUsername,
			&record.ProductName,
			&record.Exchange,
			&record.AccountName,
			&record.APIKeyEnc,
			&record.APISecretEnc,
			&record.PassphraseEnc,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan trading account: %w", err)
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trading accounts: %w", err)
	}
	return result, nil
}

func (r *TradingRepository) GetByOwner(
	ctx context.Context,
	ownerUsername string,
	id int64,
) (TradingAccountRecord, error) {
	var record TradingAccountRecord
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_username, product_name, exchange, account_name,
		       api_key_enc, api_secret_enc, passphrase_enc, created_at, updated_at
		FROM trading_accounts WHERE owner_username=$1 AND id=$2`,
		ownerUsername, id,
	).Scan(&record.ID, &record.OwnerUsername, &record.ProductName, &record.Exchange,
		&record.AccountName, &record.APIKeyEnc, &record.APISecretEnc,
		&record.PassphraseEnc, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TradingAccountRecord{}, ErrTradingAccountNotFound
	}
	if err != nil {
		return TradingAccountRecord{}, fmt.Errorf("get trading account: %w", err)
	}
	return record, nil
}

func (r *TradingRepository) ListByOwnerProduct(
	ctx context.Context,
	ownerUsername string,
	productName string,
) ([]TradingAccountRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_username, product_name, exchange, account_name,
		       api_key_enc, api_secret_enc, passphrase_enc, created_at, updated_at
		FROM trading_accounts
		WHERE owner_username=$1 AND product_name=$2
		ORDER BY exchange ASC, account_name ASC, id ASC`, ownerUsername, productName)
	if err != nil {
		return nil, fmt.Errorf("list trading accounts by product: %w", err)
	}
	defer rows.Close()
	result := make([]TradingAccountRecord, 0)
	for rows.Next() {
		var record TradingAccountRecord
		if err := rows.Scan(&record.ID, &record.OwnerUsername, &record.ProductName,
			&record.Exchange, &record.AccountName, &record.APIKeyEnc, &record.APISecretEnc,
			&record.PassphraseEnc, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan trading account: %w", err)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *TradingRepository) Create(
	ctx context.Context,
	record TradingAccountRecord,
) (TradingAccountRecord, error) {
	var created TradingAccountRecord
	err := r.pool.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name,
			api_key_enc, api_secret_enc, passphrase_enc
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, owner_username, product_name, exchange, account_name,
		          api_key_enc, api_secret_enc, passphrase_enc, created_at, updated_at`,
		record.OwnerUsername,
		record.ProductName,
		record.Exchange,
		record.AccountName,
		record.APIKeyEnc,
		record.APISecretEnc,
		nullableBytes(record.PassphraseEnc),
	).Scan(
		&created.ID,
		&created.OwnerUsername,
		&created.ProductName,
		&created.Exchange,
		&created.AccountName,
		&created.APIKeyEnc,
		&created.APISecretEnc,
		&created.PassphraseEnc,
		&created.CreatedAt,
		&created.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return TradingAccountRecord{}, ErrDuplicateTradingAccount
		}
		return TradingAccountRecord{}, fmt.Errorf("create trading account: %w", err)
	}
	return created, nil
}

func (r *TradingRepository) DeleteByOwner(
	ctx context.Context,
	ownerUsername string,
	id int64,
) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM trading_accounts
		WHERE owner_username=$1 AND id=$2`, ownerUsername, id)
	if err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTradingAccountNotFound
	}
	return nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
