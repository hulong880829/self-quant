package trader

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	postgresDeadlockSQLState   = "40P01"
	deadlockRetryExtraAttempts = 2
)

var deadlockRetryBackoff = func() time.Duration {
	return 10*time.Millisecond + time.Duration(rand.IntN(41))*time.Millisecond
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == postgresDeadlockSQLState
}

func retryOnDeadlock[T any](ctx context.Context, op func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		result, err := op()
		if err == nil || !isPostgresDeadlock(err) || attempt >= deadlockRetryExtraAttempts {
			return result, err
		}
		timer := time.NewTimer(deadlockRetryBackoff())
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
}
