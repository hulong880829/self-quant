package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("account not found")

const (
	AccountStatusActive   = "active"
	AccountStatusDisabled = "disabled"
	AccountStatusArchived = "archived"
	CreatedSourceSelfQuant = "selfquant"
	CreatedSourceBroker    = "broker"
)

type Account struct {
	Username      string
	PasswordHash  string
	Permission    string
	BrokerID      string
	Status        string
	CreatedSource string
	CreatedBy     string
	AuthVersion   int64
	DeletedAt     *time.Time
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const accountColumns = `
	username, password_hash, permission,
	COALESCE(broker_id, ''), status, created_source, created_by,
	auth_version, deleted_at`

func scanAccount(row pgx.Row) (Account, error) {
	var account Account
	err := row.Scan(
		&account.Username, &account.PasswordHash, &account.Permission,
		&account.BrokerID, &account.Status, &account.CreatedSource, &account.CreatedBy,
		&account.AuthVersion, &account.DeletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("scan account: %w", err)
	}
	account.AuthVersion = NormalizedAuthVersion(account.AuthVersion)
	if account.Status == "" {
		account.Status = AccountStatusActive
	}
	return account, nil
}

func (r *Repository) GetByUsername(ctx context.Context, username string) (Account, error) {
	return scanAccount(r.pool.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM accounts
		WHERE username=$1`, username))
}

func (r *Repository) Pool() *pgxpool.Pool {
	return r.pool
}
