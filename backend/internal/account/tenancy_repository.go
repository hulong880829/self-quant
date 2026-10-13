package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Broker struct {
	BrokerID           string
	Name               string
	ClientSecretHash   string
	ClientSecretMasked string
	Status             string
	TokenVersion       int64
}

func (r *Repository) RequireActiveBroker(ctx context.Context, brokerID string) error {
	var status string
	err := r.pool.QueryRow(ctx, `
		SELECT status FROM brokers WHERE broker_id=$1`, brokerID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBrokerMissing
	}
	if err != nil {
		return fmt.Errorf("lookup broker: %w", err)
	}
	if status != "active" {
		return ErrBrokerInactive
	}
	return nil
}

func (r *Repository) GetBroker(ctx context.Context, brokerID string) (Broker, error) {
	var broker Broker
	err := r.pool.QueryRow(ctx, `
		SELECT broker_id, name, client_secret_hash, client_secret_masked, status, token_version
		FROM brokers WHERE broker_id=$1`, brokerID).Scan(
		&broker.BrokerID, &broker.Name, &broker.ClientSecretHash,
		&broker.ClientSecretMasked, &broker.Status, &broker.TokenVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Broker{}, ErrBrokerMissing
	}
	if err != nil {
		return Broker{}, fmt.Errorf("get broker: %w", err)
	}
	return broker, nil
}

func (r *Repository) ListPlatformAccounts(ctx context.Context, brokerID string) ([]Account, error) {
	query := `
		SELECT ` + accountColumns + `
		FROM accounts
		WHERE permission <> 'admin'`
	args := []any{}
	if brokerID != "" {
		query += ` AND broker_id=$1`
		args = append(args, brokerID)
	}
	query += ` ORDER BY username`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list platform accounts: %w", err)
	}
	defer rows.Close()
	accounts := make([]Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (r *Repository) ListOwnerAccountRows(ctx context.Context) ([]OwnerAccountRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT username, COALESCE(broker_id, ''), status, permission
		FROM accounts
		WHERE permission <> 'admin' AND status <> $1
		ORDER BY username`, AccountStatusArchived)
	if err != nil {
		return nil, fmt.Errorf("list owner rows: %w", err)
	}
	defer rows.Close()
	items := make([]OwnerAccountRow, 0)
	for rows.Next() {
		var row OwnerAccountRow
		if err := rows.Scan(&row.Username, &row.BrokerID, &row.Status, &row.Permission); err != nil {
			return nil, err
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

func (r *Repository) CreatePlatformAccount(ctx context.Context, account Account) (Account, error) {
	var broker any
	if strings.TrimSpace(account.BrokerID) == "" {
		broker = nil
	} else {
		broker = account.BrokerID
	}
	created, err := scanAccount(r.pool.QueryRow(ctx, `
		INSERT INTO accounts (
			username, password_hash, permission, broker_id, status,
			created_source, created_by, auth_version
		) VALUES ($1, $2, 'user', $3, $4, $5, $6, $7)
		RETURNING `+accountColumns, account.Username, account.PasswordHash, broker,
		account.Status, account.CreatedSource, account.CreatedBy, account.AuthVersion))
	if isUniqueViolation(err) {
		return Account{}, ErrAccountExists
	}
	if err != nil {
		return Account{}, fmt.Errorf("create platform account: %w", err)
	}
	return created, nil
}

func (r *Repository) SetPlatformAccountStatus(
	ctx context.Context, username, status string, bumpAuth bool,
) (Account, error) {
	query := `
		UPDATE accounts
		SET status=$2, updated_at=now()`
	if bumpAuth {
		query += `, auth_version=auth_version+1`
	}
	if status == AccountStatusActive {
		query += `, deleted_at=NULL`
	}
	query += `
		WHERE username=$1
		RETURNING ` + accountColumns
	account, err := scanAccount(r.pool.QueryRow(ctx, query, username, status))
	if err != nil {
		return Account{}, err
	}
	return account, nil
}

func (r *Repository) ResetPlatformAccountPassword(
	ctx context.Context, username, passwordHash string,
) (Account, error) {
	return scanAccount(r.pool.QueryRow(ctx, `
		UPDATE accounts
		SET password_hash=$2, auth_version=auth_version+1, updated_at=now()
		WHERE username=$1
		RETURNING `+accountColumns, username, passwordHash))
}

func (r *Repository) UpdatePlatformAccountBroker(
	ctx context.Context, username, brokerID string,
) (Account, error) {
	var broker any
	if strings.TrimSpace(brokerID) == "" {
		broker = nil
	} else {
		broker = brokerID
	}
	return scanAccount(r.pool.QueryRow(ctx, `
		UPDATE accounts
		SET broker_id=$2, auth_version=auth_version+1, updated_at=now()
		WHERE username=$1
		RETURNING `+accountColumns, username, broker))
}

func (r *Repository) DeleteOrArchivePlatformAccount(ctx context.Context, username string) (Account, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	account, err := scanAccount(tx.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM accounts
		WHERE username=$1
		FOR UPDATE`, username))
	if err != nil {
		return Account{}, err
	}

	var active int
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM trader_arbitrage_combinations
			 WHERE owner_username=$1 AND status IN ('running', 'closing')) +
			(SELECT COUNT(*) FROM trader_twap_jobs
			 WHERE owner_username=$1 AND status IN ('pending', 'running')) +
			(SELECT COUNT(*) FROM trader_orders
			 WHERE owner_username=$1 AND status IN ('pending', 'open', 'partially_filled', 'unknown'))
	`, username).Scan(&active); err != nil {
		return Account{}, fmt.Errorf("check active work: %w", err)
	}
	if active > 0 {
		return Account{}, ErrAccountConflict
	}

	var history int
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM trading_accounts WHERE owner_username=$1) +
			(SELECT COUNT(*) FROM trader_orders WHERE owner_username=$1) +
			(SELECT COUNT(*) FROM trader_arbitrage_combinations WHERE owner_username=$1) +
			(SELECT COUNT(*) FROM trader_twap_jobs WHERE owner_username=$1) +
			(SELECT COUNT(*) FROM products WHERE owner_username=$1)
	`, username).Scan(&history); err != nil {
		return Account{}, fmt.Errorf("check account history: %w", err)
	}

	if history == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM accounts WHERE username=$1`, username); err != nil {
			return Account{}, fmt.Errorf("delete platform account: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Account{}, err
		}
		account.Status = "deleted"
		return account, nil
	}

	archived, err := scanAccount(tx.QueryRow(ctx, `
		UPDATE accounts
		SET status=$2, deleted_at=now(), auth_version=auth_version+1, updated_at=now()
		WHERE username=$1
		RETURNING `+accountColumns, username, AccountStatusArchived))
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	return archived, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
