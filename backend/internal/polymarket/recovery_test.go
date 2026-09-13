package polymarket

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"selfquant/backend/internal/polymarketauth"
)

func TestIsTransientCredentialError(t *testing.T) {
	if !isTransientCredentialError(&polymarketauth.AuthError{StatusCode: 503}) ||
		!isTransientCredentialError(&CLOBError{StatusCode: 429}) ||
		!isTransientCredentialError(&TransportError{Method: "GET", Err: context.DeadlineExceeded}) ||
		!isTransientCredentialError(status.Error(codes.Unavailable, "down")) {
		t.Fatal("expected transient classification")
	}
	if isTransientCredentialError(&CLOBError{StatusCode: 401}) ||
		isTransientCredentialError(status.Error(codes.Unauthenticated, "rebind")) {
		t.Fatal("401 must not be treated as transient")
	}
}

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
	mu           sync.Mutex
	current      Credentials
	refreshed    Credentials
	invalid      bool
	refreshErr   error
	refreshDelay time.Duration
	refreshes    atomic.Int32
	invalidates  atomic.Int32
	activates    atomic.Int32
}

func testAPICredentials() Credentials {
	return Credentials{
		SignerAddress: "0xabc", FunderAddress: "0xwallet", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	}
}

func (f *recoveryCredentials) Get(context.Context, string, int64) (Credentials, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invalid {
		return Credentials{}, "", status.Error(
			codes.Unauthenticated, "polymarket credentials invalid; please rebind",
		)
	}
	return f.current, "account", nil
}

func (f *recoveryCredentials) Refresh(context.Context, string, int64) (Credentials, string, error) {
	if f.refreshDelay > 0 {
		time.Sleep(f.refreshDelay)
	}
	f.refreshes.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshErr != nil {
		return Credentials{}, "", f.refreshErr
	}
	if f.refreshed.APIKey != "" {
		f.current = f.refreshed
		return f.refreshed, "account", nil
	}
	return f.current, "account", nil
}

func (f *recoveryCredentials) Invalidate(context.Context, string, int64) error {
	f.invalidates.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = true
	return nil
}

func (f *recoveryCredentials) Activate(context.Context, string, int64) error {
	f.activates.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = false
	return nil
}

func (f *recoveryCredentials) Owner(context.Context, string) (string, error) {
	return "owner", nil
}

type countingCLOB struct {
	balanceCalls atomic.Int32
	balanceCode  int
}

func newCountingCLOB(t *testing.T, balanceCode int) (*httptest.Server, *countingCLOB) {
	t.Helper()
	counter := &countingCLOB{balanceCode: balanceCode}
	if counter.balanceCode == 0 {
		counter.balanceCode = http.StatusOK
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "balance-allowance") {
			counter.balanceCalls.Add(1)
			if counter.balanceCode != http.StatusOK {
				writer.WriteHeader(counter.balanceCode)
				_, _ = io.WriteString(writer, `{"error":"unauthorized"}`)
				return
			}
			_, _ = io.WriteString(writer, `{"balance":"44374326"}`)
			return
		}
		if strings.Contains(request.URL.Path, "data/orders") ||
			strings.Contains(request.URL.Path, "/orders") {
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{}`)
	}))
	t.Cleanup(server.Close)
	return server, counter
}

func newDataServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "positions") {
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		_, _ = io.WriteString(writer, `{"value":0}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func newRecoveryService(
	t *testing.T,
	provider *recoveryCredentials,
	clobURL, dataURL string,
) *Service {
	t.Helper()
	service := NewService(
		nil, nil,
		NewCLOBClient(clobURL, time.Second),
		NewDataClient(dataURL, time.Second),
		nil, provider, NewSnapshotStore(), time.Millisecond, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return service
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

func TestPrivateCLOBRefreshUnavailableDoesNotInvalidate(t *testing.T) {
	provider := &recoveryCredentials{
		current:    Credentials{APIKey: "old"},
		refreshErr: &polymarketauth.AuthError{StatusCode: 503, Message: "maintenance"},
	}
	service := &Service{credentials: provider}
	_, _, err := callPrivateCLOB(
		context.Background(), service, "token", 7, provider.current,
		func(Credentials) (string, error) {
			return "", &CLOBError{StatusCode: http.StatusUnauthorized}
		},
	)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if provider.refreshes.Load() != 1 || provider.invalidates.Load() != 0 {
		t.Fatalf(
			"refreshes=%d invalidates=%d",
			provider.refreshes.Load(), provider.invalidates.Load(),
		)
	}
}

func TestInvalidAccountRecoversWhenPrivateReadSucceeds(t *testing.T) {
	clob, counter := newCountingCLOB(t, http.StatusOK)
	data := newDataServer(t)
	provider := &recoveryCredentials{
		invalid:   true,
		current:   testAPICredentials(),
		refreshed: testAPICredentials(),
	}
	provider.refreshed.APIKey = "new"
	service := newRecoveryService(t, provider, clob.URL, data.URL)
	summary, err := service.GetAccountSummary(context.Background(), "token", 7)
	if err != nil {
		t.Fatal(err)
	}
	if summary.BindingStatus != "active" || summary.Stale {
		t.Fatalf("summary=%+v", summary)
	}
	if provider.refreshes.Load() != 1 || provider.activates.Load() != 1 ||
		provider.invalidates.Load() != 0 || counter.balanceCalls.Load() < 1 {
		t.Fatalf(
			"refreshes=%d activates=%d invalidates=%d balance=%d",
			provider.refreshes.Load(), provider.activates.Load(),
			provider.invalidates.Load(), counter.balanceCalls.Load(),
		)
	}
	if provider.invalid {
		t.Fatal("account remained invalid after successful verify")
	}
}

func TestInvalidAccountStaysInvalidWhenPrivateReadStill401(t *testing.T) {
	clob, counter := newCountingCLOB(t, http.StatusUnauthorized)
	data := newDataServer(t)
	provider := &recoveryCredentials{
		invalid:   true,
		current:   testAPICredentials(),
		refreshed: testAPICredentials(),
	}
	updated := time.Date(2026, 8, 26, 6, 48, 0, 0, time.UTC)
	service := newRecoveryService(t, provider, clob.URL, data.URL)
	service.summaries[7] = cachedSummary{
		value: AccountSummary{
			TradingAccountID: 7, TotalAssets: "44.374326",
			SourceUpdatedAt: updated,
		},
		expiresAt: time.Now().Add(-time.Second),
	}
	summary, err := service.GetAccountSummary(context.Background(), "token", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Stale || summary.BindingStatus != "invalid" ||
		summary.TotalAssets != "44.374326" {
		t.Fatalf("summary=%+v", summary)
	}
	if provider.refreshes.Load() != 1 || provider.activates.Load() != 0 ||
		counter.balanceCalls.Load() != 1 {
		t.Fatalf(
			"refreshes=%d activates=%d balance=%d invalidates=%d",
			provider.refreshes.Load(), provider.activates.Load(),
			counter.balanceCalls.Load(), provider.invalidates.Load(),
		)
	}
	if !provider.invalid {
		t.Fatal("account was activated after verified 401")
	}
}

func TestInvalidAccountConcurrentReadsRefreshAndVerifyOnce(t *testing.T) {
	clob, counter := newCountingCLOB(t, http.StatusOK)
	data := newDataServer(t)
	provider := &recoveryCredentials{
		invalid:      true,
		current:      testAPICredentials(),
		refreshed:    testAPICredentials(),
		refreshDelay: 50 * time.Millisecond,
	}
	provider.refreshed.APIKey = "new"
	service := newRecoveryService(t, provider, clob.URL, data.URL)
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		if _, err := service.GetAccountSummary(context.Background(), "token", 7); err != nil {
			t.Errorf("summary: %v", err)
		}
	}()
	go func() {
		defer group.Done()
		if _, _, err := service.ListPositions(context.Background(), "token", 7, true); err != nil {
			t.Errorf("positions: %v", err)
		}
	}()
	go func() {
		defer group.Done()
		if _, _, err := service.ListOpenOrders(context.Background(), "token", 7, true); err != nil {
			t.Errorf("orders: %v", err)
		}
	}()
	group.Wait()
	if provider.refreshes.Load() != 1 || provider.activates.Load() != 1 {
		t.Fatalf(
			"refreshes=%d activates=%d balance=%d",
			provider.refreshes.Load(), provider.activates.Load(),
			counter.balanceCalls.Load(),
		)
	}
	var verifyGroup sync.WaitGroup
	provider2 := &recoveryCredentials{
		invalid:      true,
		current:      testAPICredentials(),
		refreshed:    testAPICredentials(),
		refreshDelay: 50 * time.Millisecond,
	}
	clob2, counter2 := newCountingCLOB(t, http.StatusOK)
	service2 := newRecoveryService(t, provider2, clob2.URL, data.URL)
	for range 3 {
		verifyGroup.Add(1)
		go func() {
			defer verifyGroup.Done()
			_, _, _ = service2.refreshAndVerifyCredentials(
				context.Background(), "token", 7, true,
			)
		}()
	}
	verifyGroup.Wait()
	if provider2.refreshes.Load() != 1 || counter2.balanceCalls.Load() != 1 {
		t.Fatalf(
			"direct recover refreshes=%d balance=%d",
			provider2.refreshes.Load(), counter2.balanceCalls.Load(),
		)
	}
}

func TestInvalidAccountBackoffSkipsPolymarket(t *testing.T) {
	clob, counter := newCountingCLOB(t, http.StatusUnauthorized)
	data := newDataServer(t)
	provider := &recoveryCredentials{
		invalid:   true,
		current:   testAPICredentials(),
		refreshed: testAPICredentials(),
	}
	clock := time.Date(2026, 8, 26, 6, 48, 0, 0, time.UTC)
	service := newRecoveryService(t, provider, clob.URL, data.URL)
	service.clock = func() time.Time { return clock }
	if _, err := service.GetAccountSummary(context.Background(), "token", 7); err != nil {
		t.Fatal(err)
	}
	if provider.refreshes.Load() != 1 || counter.balanceCalls.Load() != 1 {
		t.Fatalf(
			"first refreshes=%d balance=%d",
			provider.refreshes.Load(), counter.balanceCalls.Load(),
		)
	}
	clock = clock.Add(31 * time.Second)
	service.cacheMu.Lock()
	cached := service.summaries[7]
	cached.expiresAt = clock.Add(-time.Second)
	service.summaries[7] = cached
	service.cacheMu.Unlock()
	if _, err := service.GetAccountSummary(context.Background(), "token", 7); err != nil {
		t.Fatal(err)
	}
	if provider.refreshes.Load() != 1 || counter.balanceCalls.Load() != 1 {
		t.Fatalf(
			"backoff refreshes=%d balance=%d",
			provider.refreshes.Load(), counter.balanceCalls.Load(),
		)
	}
}

func TestInvalidAccountReturnsStaleSnapshotAndRejectsCancel(t *testing.T) {
	clob, _ := newCountingCLOB(t, http.StatusServiceUnavailable)
	data := newDataServer(t)
	provider := &recoveryCredentials{
		invalid:    true,
		current:    testAPICredentials(),
		refreshErr: status.Error(codes.Unavailable, "polymarket auth unavailable"),
	}
	updated := time.Date(2026, 8, 26, 6, 48, 0, 0, time.UTC)
	service := newRecoveryService(t, provider, clob.URL, data.URL)
	service.summaries[7] = cachedSummary{
		value: AccountSummary{
			TradingAccountID: 7, TotalAssets: "44.374326",
			SourceUpdatedAt: updated, BindingStatus: "invalid",
		},
		expiresAt: time.Now().Add(-time.Second),
	}
	summary, err := service.GetAccountSummary(context.Background(), "token", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Stale || summary.BindingStatus != "invalid" ||
		summary.TotalAssets != "44.374326" {
		t.Fatalf("summary=%+v", summary)
	}
	_, cancelErr := service.CancelOrder(context.Background(), "token", 7, "order-1")
	if cancelErr == nil || !isBindingInvalid(cancelErr) {
		t.Fatalf("cancel err=%v", cancelErr)
	}
}

func TestRestartedServiceRetriesInvalidRecoveryImmediately(t *testing.T) {
	clob, counter := newCountingCLOB(t, http.StatusOK)
	data := newDataServer(t)
	provider := &recoveryCredentials{
		invalid:   true,
		current:   testAPICredentials(),
		refreshed: testAPICredentials(),
	}
	provider.refreshed.APIKey = "new"
	first := newRecoveryService(t, provider, clob.URL, data.URL)
	first.noteRecoveryFailure(7)
	if !first.recoveryBackoffActive(7) {
		t.Fatal("expected backoff on first process")
	}
	restarted := newRecoveryService(t, provider, clob.URL, data.URL)
	if restarted.recoveryBackoffActive(7) {
		t.Fatal("restarted service should not inherit in-memory backoff")
	}
	summary, err := restarted.GetAccountSummary(context.Background(), "token", 7)
	if err != nil {
		t.Fatal(err)
	}
	if summary.BindingStatus != "active" || provider.refreshes.Load() != 1 ||
		counter.balanceCalls.Load() < 1 {
		t.Fatalf(
			"summary=%+v refreshes=%d balance=%d",
			summary, provider.refreshes.Load(), counter.balanceCalls.Load(),
		)
	}
}
