package polymarket

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnknownSubmissionRequiresWrittenPOST(t *testing.T) {
	timeoutErr := context.DeadlineExceeded
	if !isUnknownOrderSubmission(&TransportError{
		Method: "POST", RequestWritten: true, Err: timeoutErr,
	}) {
		t.Fatal("written POST timeout must be treated as submission_unknown")
	}
	if isUnknownOrderSubmission(&TransportError{
		Method: "POST", RequestWritten: false, Err: timeoutErr,
	}) || isUnknownOrderSubmission(errors.New("plain timeout")) {
		t.Fatal("unwritten or untyped errors must not be treated as unknown submissions")
	}
}

func TestSubmissionCandidateMatchingIsAccountOrderSpecific(t *testing.T) {
	created := time.Now().UTC()
	order := Order{TokenID: "token", Side: "buy", CreatedAt: created}
	if !submissionCandidateMatches(order, "token", "BUY", created.Add(time.Second)) {
		t.Fatal("expected matching private event")
	}
	if submissionCandidateMatches(order, "other", "BUY", created.Add(time.Second)) ||
		submissionCandidateMatches(order, "token", "SELL", created.Add(time.Second)) ||
		submissionCandidateMatches(order, "token", "BUY", created.Add(3*time.Minute)) {
		t.Fatal("mismatched token, side, or time must not resolve an unknown submission")
	}
}

type recoveryCredentials struct {
	current     Credentials
	refreshed   Credentials
	refreshes   atomic.Int32
	invalidates atomic.Int32
}

func (f *recoveryCredentials) Get(context.Context, string, int64) (Credentials, string, error) {
	return f.current, "account", nil
}

func (f *recoveryCredentials) Refresh(context.Context, string, int64) (Credentials, string, error) {
	f.refreshes.Add(1)
	return f.refreshed, "account", nil
}

func (f *recoveryCredentials) Invalidate(context.Context, string, int64) error {
	f.invalidates.Add(1)
	return nil
}

func (f *recoveryCredentials) Owner(context.Context, string) (string, error) {
	return "owner", nil
}

func TestPrivateCLOBRefreshesOnlyOnceAfter401(t *testing.T) {
	provider := &recoveryCredentials{
		current:   Credentials{APIKey: "old"},
		refreshed: Credentials{APIKey: "new"},
	}
	service := &Service{credentials: provider}
	calls := 0
	result, used, err := callPrivateCLOB(
		context.Background(), service, "token", 7, provider.current,
		func(credentials Credentials) (string, error) {
			calls++
			if credentials.APIKey == "old" {
				return "", &CLOBError{StatusCode: http.StatusUnauthorized}
			}
			return "ok", nil
		},
	)
	if err != nil || result != "ok" || used.APIKey != "new" {
		t.Fatalf("result=%q credentials=%+v err=%v", result, used, err)
	}
	if calls != 2 || provider.refreshes.Load() != 1 || provider.invalidates.Load() != 0 {
		t.Fatalf(
			"calls=%d refreshes=%d invalidates=%d",
			calls, provider.refreshes.Load(), provider.invalidates.Load(),
		)
	}
}

func TestPrivateCLOBInvalidatesAfterRefreshedCredentialAlsoGets401(t *testing.T) {
	provider := &recoveryCredentials{
		current:   Credentials{APIKey: "old"},
		refreshed: Credentials{APIKey: "new"},
	}
	service := &Service{credentials: provider}
	_, _, err := callPrivateCLOB(
		context.Background(), service, "token", 7, provider.current,
		func(Credentials) (string, error) {
			return "", &CLOBError{StatusCode: http.StatusUnauthorized}
		},
	)
	if err == nil || provider.refreshes.Load() != 1 || provider.invalidates.Load() != 1 {
		t.Fatalf(
			"err=%v refreshes=%d invalidates=%d",
			err, provider.refreshes.Load(), provider.invalidates.Load(),
		)
	}
}

func TestAccountSummaryReturnsAndCachesStaleValueWhenUpstreamsTimeout(t *testing.T) {
	timeoutServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer timeoutServer.Close()
	provider := &recoveryCredentials{current: Credentials{
		SignerAddress: "0xabc", FunderAddress: "0xwallet", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	}}
	service := NewService(
		nil, nil,
		NewCLOBClient(timeoutServer.URL, 20*time.Millisecond),
		NewDataClient(timeoutServer.URL, 20*time.Millisecond),
		nil, provider, nil, time.Second, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.summaries[7] = cachedSummary{
		value: AccountSummary{
			TradingAccountID: 7, AvailableBalance: "12", PositionValue: "3",
			TotalAssets: "15", SourceUpdatedAt: time.Now().Add(-time.Minute),
		},
		expiresAt: time.Now().Add(-time.Second),
	}
	summary, err := service.GetAccountSummary(context.Background(), "token", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Stale || summary.TotalAssets != "15" {
		t.Fatalf("summary=%+v", summary)
	}
	if !service.summaries[7].expiresAt.After(time.Now()) {
		t.Fatal("stale fallback was not cached with a cooldown")
	}
}
