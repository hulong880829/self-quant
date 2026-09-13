package account

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTradingAccountNotFound           = errors.New("trading account not found")
	ErrDuplicateTradingAccount          = errors.New("trading account already exists")
	ErrTradingAccountHasActiveArbitrage = errors.New("无法删除：该账户有运行中的套利组合，请先停止后再删")
	ErrTradingAccountHasActiveTWAP      = errors.New("无法删除：该账户有运行中的 TWAP，请先停止后再删")
)

type TradingAccountRecord struct {
	ID                  int64
	OwnerUsername       string
	ProductName         string
	Exchange            string
	AccountName         string
	APIKeyEnc           []byte
	APISecretEnc        []byte
	PassphraseEnc       []byte
	CredentialKind      string
	TradingAPIKeyEnc    []byte
	TradingAPISecretEnc []byte
	SigningAddress      string
	VaultAddress        string
	AccountIndex        *int64
	APIKeyIndex         *int16
	CreatedAt           time.Time
	UpdatedAt           time.Time
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
		       api_key_enc, api_secret_enc, passphrase_enc,
		       COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		       COALESCE(signing_address,''), COALESCE(vault_address,''),
		       account_index, api_key_index, created_at, updated_at
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
			&record.CredentialKind,
			&record.TradingAPIKeyEnc,
			&record.TradingAPISecretEnc,
			&record.SigningAddress,
			&record.VaultAddress,
			&record.AccountIndex,
			&record.APIKeyIndex,
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
		       api_key_enc, api_secret_enc, passphrase_enc,
		       COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		       COALESCE(signing_address,''), COALESCE(vault_address,''),
		       account_index, api_key_index, created_at, updated_at
		FROM trading_accounts WHERE owner_username=$1 AND id=$2`,
		ownerUsername, id,
	).Scan(&record.ID, &record.OwnerUsername, &record.ProductName, &record.Exchange,
		&record.AccountName, &record.APIKeyEnc, &record.APISecretEnc,
		&record.PassphraseEnc, &record.CredentialKind, &record.TradingAPIKeyEnc,
		&record.TradingAPISecretEnc, &record.SigningAddress, &record.VaultAddress,
		&record.AccountIndex, &record.APIKeyIndex, &record.CreatedAt, &record.UpdatedAt)
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
		       api_key_enc, api_secret_enc, passphrase_enc,
		       COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		       COALESCE(signing_address,''), COALESCE(vault_address,''),
		       account_index, api_key_index, created_at, updated_at
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
			&record.PassphraseEnc, &record.CredentialKind, &record.TradingAPIKeyEnc,
			&record.TradingAPISecretEnc, &record.SigningAddress, &record.VaultAddress,
			&record.AccountIndex, &record.APIKeyIndex, &record.CreatedAt, &record.UpdatedAt); err != nil {
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
			api_key_enc, api_secret_enc, passphrase_enc, credential_kind,
			trading_api_key_enc, trading_api_secret_enc, signing_address,
			vault_address, account_index, api_key_index
		) VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,NULLIF($11,''),NULLIF($12,''),$13,$14)
		RETURNING id, owner_username, product_name, exchange, account_name,
		          api_key_enc, api_secret_enc, passphrase_enc,
		          COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		          COALESCE(signing_address,''), COALESCE(vault_address,''),
		          account_index, api_key_index, created_at, updated_at`,
		record.OwnerUsername,
		record.ProductName,
		record.Exchange,
		record.AccountName,
		record.APIKeyEnc,
		record.APISecretEnc,
		nullableBytes(record.PassphraseEnc),
		record.CredentialKind,
		nullableBytes(record.TradingAPIKeyEnc),
		nullableBytes(record.TradingAPISecretEnc),
		record.SigningAddress,
		record.VaultAddress,
		record.AccountIndex,
		record.APIKeyIndex,
	).Scan(
		&created.ID,
		&created.OwnerUsername,
		&created.ProductName,
		&created.Exchange,
		&created.AccountName,
		&created.APIKeyEnc,
		&created.APISecretEnc,
		&created.PassphraseEnc,
		&created.CredentialKind,
		&created.TradingAPIKeyEnc,
		&created.TradingAPISecretEnc,
		&created.SigningAddress,
		&created.VaultAddress,
		&created.AccountIndex,
		&created.APIKeyIndex,
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

func (r *TradingRepository) UpdateIndexes(
	ctx context.Context,
	ownerUsername string,
	id int64,
	accountIndex *int64,
	apiKeyIndex *int16,
) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE trading_accounts
		SET account_index=$3, api_key_index=$4, updated_at=now()
		WHERE owner_username=$1 AND id=$2`,
		ownerUsername, id, accountIndex, apiKeyIndex)
	if err != nil {
		return fmt.Errorf("update trading account indexes: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTradingAccountNotFound
	}
	return nil
}

func (r *TradingRepository) DeleteByOwner(
	ctx context.Context,
	ownerUsername string,
	id int64,
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM trading_accounts
		WHERE owner_username=$1 AND id=$2
		FOR UPDATE`, ownerUsername, id).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTradingAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}

	var hasTWAP bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trader_twap_jobs
			WHERE trading_account_id=$1 AND status IN ('pending','running')
		)`, id).Scan(&hasTWAP); err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}
	if hasTWAP {
		return ErrTradingAccountHasActiveTWAP
	}

	var hasArb bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trader_arbitrage_combinations
			WHERE status IN ('running','closing')
			  AND (leg_a_trading_account_id=$1 OR leg_b_trading_account_id=$1)
		)`, id).Scan(&hasArb); err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}
	if hasArb {
		return ErrTradingAccountHasActiveArbitrage
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM trading_accounts
		WHERE owner_username=$1 AND id=$2`, ownerUsername, id)
	if err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTradingAccountNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete trading account: %w", err)
	}
	return nil
}

func (r *TradingRepository) GetByID(
	ctx context.Context,
	id int64,
) (TradingAccountRecord, error) {
	var record TradingAccountRecord
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_username, product_name, exchange, account_name,
		       api_key_enc, api_secret_enc, passphrase_enc,
		       COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		       COALESCE(signing_address,''), COALESCE(vault_address,''),
		       account_index, api_key_index, created_at, updated_at
		FROM trading_accounts WHERE id=$1`, id,
	).Scan(&record.ID, &record.OwnerUsername, &record.ProductName, &record.Exchange,
		&record.AccountName, &record.APIKeyEnc, &record.APISecretEnc,
		&record.PassphraseEnc, &record.CredentialKind, &record.TradingAPIKeyEnc,
		&record.TradingAPISecretEnc, &record.SigningAddress, &record.VaultAddress,
		&record.AccountIndex, &record.APIKeyIndex, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TradingAccountRecord{}, ErrTradingAccountNotFound
	}
	if err != nil {
		return TradingAccountRecord{}, fmt.Errorf("get trading account by id: %w", err)
	}
	return record, nil
}

func (r *TradingRepository) GetFeeRates(
	ctx context.Context,
	id int64,
) (FeeRatesRecord, error) {
	var record FeeRatesRecord
	err := r.pool.QueryRow(ctx, feeRatesSelectSQL+` WHERE id=$1`, id).Scan(feeRatesScanArgs(&record)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return FeeRatesRecord{}, ErrTradingAccountNotFound
	}
	if err != nil {
		return FeeRatesRecord{}, fmt.Errorf("get trading account fee rates: %w", err)
	}
	return record, nil
}

func (r *TradingRepository) UpdateFeeRates(
	ctx context.Context,
	record FeeRatesRecord,
) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE trading_accounts SET
			spot_maker_fee_rate=$2::numeric,
			spot_taker_fee_rate=$3::numeric,
			contract_maker_fee_rate=$4::numeric,
			contract_taker_fee_rate=$5::numeric,
			spot_fee_status=NULLIF($6,''),
			contract_fee_status=NULLIF($7,''),
			fee_source=NULLIF($8,''),
			fee_updated_at=$9,
			fee_sync_status=NULLIF($10,''),
			fee_sync_error=NULLIF($11,''),
			fee_markets=$12::jsonb
		WHERE id=$1`,
		record.ID,
		nullableString(record.SpotMaker),
		nullableString(record.SpotTaker),
		nullableString(record.ContractMaker),
		nullableString(record.ContractTaker),
		record.SpotStatus,
		record.ContractStatus,
		record.Source,
		record.UpdatedAt,
		record.SyncStatus,
		record.SyncError,
		nullableBytes(record.Markets),
	)
	if err != nil {
		return fmt.Errorf("update trading account fee rates: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTradingAccountNotFound
	}
	if err := r.NotifyFeeRatesUpdated(ctx, record.ID); err != nil {
		return err
	}
	return nil
}

func (r *TradingRepository) ListFeeCacheRows(ctx context.Context) ([]FeeRatesRecord, error) {
	rows, err := r.pool.Query(ctx, feeRatesSelectSQL+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list trading account fee cache: %w", err)
	}
	defer rows.Close()
	result := make([]FeeRatesRecord, 0)
	for rows.Next() {
		var record FeeRatesRecord
		if err := rows.Scan(feeRatesScanArgs(&record)...); err != nil {
			return nil, fmt.Errorf("scan trading account fee cache: %w", err)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *TradingRepository) ListFeeSyncAccounts(ctx context.Context) ([]TradingAccountRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_username, product_name, exchange, account_name,
		       api_key_enc, api_secret_enc, passphrase_enc,
		       COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		       COALESCE(signing_address,''), COALESCE(vault_address,''),
		       account_index, api_key_index, created_at, updated_at
		FROM trading_accounts
		WHERE exchange <> 'polymarket'
		  AND COALESCE(fee_sync_status, '') <> 'credential_invalid'
		ORDER BY exchange ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list fee sync accounts: %w", err)
	}
	defer rows.Close()
	result := make([]TradingAccountRecord, 0)
	for rows.Next() {
		var record TradingAccountRecord
		if err := rows.Scan(
			&record.ID, &record.OwnerUsername, &record.ProductName, &record.Exchange,
			&record.AccountName, &record.APIKeyEnc, &record.APISecretEnc, &record.PassphraseEnc,
			&record.CredentialKind, &record.TradingAPIKeyEnc, &record.TradingAPISecretEnc,
			&record.SigningAddress, &record.VaultAddress, &record.AccountIndex, &record.APIKeyIndex,
			&record.CreatedAt, &record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan fee sync account: %w", err)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *TradingRepository) ListPendingFeeSyncIDs(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id FROM trading_accounts
		WHERE exchange <> 'polymarket'
		  AND COALESCE(fee_sync_status, '') IN ('pending', 'retrying', 'failed', '')
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list pending fee sync ids: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *TradingRepository) TryDailyFeeSyncLock(ctx context.Context, day string) (func(), bool, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire daily fee sync lock: %w", err)
	}
	var locked bool
	key := dailyFeeLockName + ":" + strings.TrimSpace(day)
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(84001, hashtext($1))`, key).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try daily fee sync lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(84001, hashtext($1))`, key)
		conn.Release()
	}, true, nil
}

func (r *TradingRepository) NotifyFeeRatesUpdated(ctx context.Context, id int64) error {
	return r.notifyFeeRates(ctx, id)
}

func (r *TradingRepository) notifyFeeRates(ctx context.Context, id int64) error {
	if _, err := r.pool.Exec(ctx, `SELECT pg_notify('trading_account_fees', $1)`, strconv.FormatInt(id, 10)); err != nil {
		return fmt.Errorf("notify trading account fees: %w", err)
	}
	return nil
}

func (r *TradingRepository) Pool() *pgxpool.Pool {
	return r.pool
}

const feeRatesSelectSQL = `
		SELECT id, exchange,
		       spot_maker_fee_rate::text, spot_taker_fee_rate::text,
		       contract_maker_fee_rate::text, contract_taker_fee_rate::text,
		       COALESCE(spot_fee_status,''), COALESCE(contract_fee_status,''),
		       COALESCE(fee_source,''), fee_updated_at,
		       COALESCE(fee_sync_status,''), COALESCE(fee_sync_error,''), fee_markets
		FROM trading_accounts`

func feeRatesScanArgs(record *FeeRatesRecord) []any {
	return []any{
		&record.ID,
		&record.Exchange,
		&record.SpotMaker,
		&record.SpotTaker,
		&record.ContractMaker,
		&record.ContractTaker,
		&record.SpotStatus,
		&record.ContractStatus,
		&record.Source,
		&record.UpdatedAt,
		&record.SyncStatus,
		&record.SyncError,
		&record.Markets,
	}
}

func nullableString(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
