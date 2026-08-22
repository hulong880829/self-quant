package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("account not found")

type Account struct {
	Username     string
	PasswordHash string
	Permission   string
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) GetByUsername(ctx context.Context, username string) (Account, error) {
	var account Account
	err := r.pool.QueryRow(ctx, `
		SELECT username, password_hash, permission
		FROM accounts
		WHERE username=$1`, username).Scan(
		&account.Username, &account.PasswordHash, &account.Permission,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return account, nil
}
