package trader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsPostgresDeadlock(t *testing.T) {
	if isPostgresDeadlock(nil) || isPostgresDeadlock(errors.New("boom")) {
		t.Fatal("non-deadlock errors must not match")
	}
	if isPostgresDeadlock(&pgconn.PgError{Code: "40001"}) {
		t.Fatal("serialization failures must not match")
	}
	if !isPostgresDeadlock(&pgconn.PgError{Code: postgresDeadlockSQLState}) {
		t.Fatal("expected 40P01 to match")
	}
	if !isPostgresDeadlock(errors.Join(errors.New("wrap"), &pgconn.PgError{Code: "40P01"})) {
		t.Fatal("wrapped 40P01 must match")
	}
}

func TestRetryOnDeadlockRetriesTwiceThenSucceeds(t *testing.T) {
	orig := deadlockRetryBackoff
	deadlockRetryBackoff = func() time.Duration { return 0 }
	t.Cleanup(func() { deadlockRetryBackoff = orig })

	calls := 0
	got, err := retryOnDeadlock(context.Background(), func() (int, error) {
		calls++
		if calls <= 2 {
			return 0, &pgconn.PgError{Code: postgresDeadlockSQLState}
		}
		return 7, nil
	})
	if err != nil || got != 7 || calls != 3 {
		t.Fatalf("got=%d calls=%d err=%v", got, calls, err)
	}
}

func TestRetryOnDeadlockStopsAfterTwoExtraAttempts(t *testing.T) {
	orig := deadlockRetryBackoff
	deadlockRetryBackoff = func() time.Duration { return 0 }
	t.Cleanup(func() { deadlockRetryBackoff = orig })

	calls := 0
	_, err := retryOnDeadlock(context.Background(), func() (int, error) {
		calls++
		return 0, &pgconn.PgError{Code: postgresDeadlockSQLState}
	})
	if !isPostgresDeadlock(err) || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRetryOnDeadlockDoesNotRetrySerializationFailure(t *testing.T) {
	calls := 0
	_, err := retryOnDeadlock(context.Background(), func() (int, error) {
		calls++
		return 0, &pgconn.PgError{Code: "40001"}
	})
	var pgErr *pgconn.PgError
	if calls != 1 || !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRetryOnDeadlockHonorsContext(t *testing.T) {
	orig := deadlockRetryBackoff
	deadlockRetryBackoff = func() time.Duration { return time.Hour }
	t.Cleanup(func() { deadlockRetryBackoff = orig })

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := retryOnDeadlock(ctx, func() (int, error) {
		calls++
		cancel()
		return 0, &pgconn.PgError{Code: postgresDeadlockSQLState}
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
