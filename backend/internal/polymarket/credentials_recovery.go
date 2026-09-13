package polymarket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"selfquant/backend/internal/polymarketauth"
)

var errCredentialRecoveryBackoff = errors.New("polymarket credential recovery backoff")
var errCredentialRecoveryFailed = errors.New("polymarket credential recovery failed")

type credentialRecoveryState struct {
	failures int
	nextAt   time.Time
}

type verifiedCredentials struct {
	credentials Credentials
	accountName string
}

func isTransientHTTPStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func isTransientCredentialError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		return true
	}
	var clobErr *CLOBError
	if errors.As(err, &clobErr) {
		return isTransientHTTPStatus(clobErr.StatusCode)
	}
	var authErr *polymarketauth.AuthError
	if errors.As(err, &authErr) {
		return isTransientHTTPStatus(authErr.StatusCode)
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled, codes.Unavailable, codes.ResourceExhausted:
		return true
	}
	return false
}

func isBindingInvalid(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	if st, ok := status.FromError(err); ok {
		if st.Code() != codes.Unauthenticated {
			return false
		}
		message = st.Message()
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "rebind") || strings.Contains(lower, "credentials invalid")
}

func nextRecoveryBackoff(failures int) time.Duration {
	switch {
	case failures <= 1:
		return time.Minute
	case failures == 2:
		return 5 * time.Minute
	case failures == 3:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

func (s *Service) nowTime() time.Time {
	if s != nil && s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func (s *Service) forgetAccountCaches(accountID int64) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.summaries != nil {
		delete(s.summaries, accountID)
	}
	if s.positions != nil {
		delete(s.positions, accountID)
	}
	if s.openOrders != nil {
		delete(s.openOrders, accountID)
	}
}

func (s *Service) markCredentialsInvalid(ctx context.Context, token string, accountID int64) {
	_ = s.credentials.Invalidate(context.WithoutCancel(ctx), token, accountID)
	s.forgetAccountCaches(accountID)
}

func (s *Service) recoveryBackoffActive(accountID int64) bool {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if s.recovery == nil {
		return false
	}
	state, ok := s.recovery[accountID]
	return ok && s.nowTime().Before(state.nextAt)
}

func (s *Service) noteRecoveryFailure(accountID int64) {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if s.recovery == nil {
		s.recovery = make(map[int64]credentialRecoveryState)
	}
	state := s.recovery[accountID]
	state.failures++
	state.nextAt = s.nowTime().Add(nextRecoveryBackoff(state.failures))
	s.recovery[accountID] = state
}

func (s *Service) clearRecoveryState(accountID int64) {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if s.recovery != nil {
		delete(s.recovery, accountID)
	}
}

func (s *Service) refreshAfterUnauthorized(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, error) {
	value, err, _ := s.group.Do(fmt.Sprintf("credentials:%d", accountID), func() (any, error) {
		refreshed, _, refreshErr := s.credentials.Refresh(ctx, token, accountID)
		return refreshed, refreshErr
	})
	if err != nil {
		return Credentials{}, err
	}
	return value.(Credentials), nil
}

func (s *Service) recoverInvalidCredentials(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, string, error) {
	return s.refreshAndVerifyCredentials(ctx, token, accountID, true)
}

func (s *Service) refreshAndVerifyCredentials(
	ctx context.Context,
	token string,
	accountID int64,
	useBackoff bool,
) (Credentials, string, error) {
	value, err, _ := s.group.Do(fmt.Sprintf("recover:%d", accountID), func() (any, error) {
		if useBackoff && s.recoveryBackoffActive(accountID) {
			return verifiedCredentials{}, errCredentialRecoveryBackoff
		}
		refreshed, accountName, refreshErr := s.credentials.Refresh(ctx, token, accountID)
		if refreshErr != nil {
			s.noteRecoveryFailure(accountID)
			return verifiedCredentials{}, refreshErr
		}
		if s.clob == nil {
			s.noteRecoveryFailure(accountID)
			return verifiedCredentials{}, errors.New("polymarket clob client is not configured")
		}
		if _, verifyErr := s.clob.CollateralBalance(ctx, refreshed); verifyErr != nil {
			if isCLOBUnauthorized(verifyErr) {
				s.markCredentialsInvalid(ctx, token, accountID)
			}
			s.noteRecoveryFailure(accountID)
			return verifiedCredentials{}, verifyErr
		}
		if activateErr := s.credentials.Activate(ctx, token, accountID); activateErr != nil {
			s.noteRecoveryFailure(accountID)
			return verifiedCredentials{}, activateErr
		}
		s.clearRecoveryState(accountID)
		return verifiedCredentials{credentials: refreshed, accountName: accountName}, nil
	})
	if err != nil {
		return Credentials{}, "", err
	}
	result := value.(verifiedCredentials)
	return result.credentials, result.accountName, nil
}

func callPrivateCLOB[T any](
	ctx context.Context,
	service *Service,
	token string,
	accountID int64,
	credentials Credentials,
	call func(Credentials) (T, error),
) (T, Credentials, error) {
	result, err := call(credentials)
	if !isCLOBUnauthorized(err) {
		return result, credentials, err
	}
	refreshed, refreshErr := service.refreshAfterUnauthorized(ctx, token, accountID)
	if refreshErr != nil {
		var zero T
		return zero, credentials, refreshErr
	}
	result, err = call(refreshed)
	if isCLOBUnauthorized(err) {
		service.markCredentialsInvalid(ctx, token, accountID)
	}
	return result, refreshed, err
}

func isRecoverableCredentialError(err error) bool {
	return isBindingInvalid(err) ||
		errors.Is(err, errCredentialRecoveryBackoff) ||
		errors.Is(err, errCredentialRecoveryFailed)
}

func (s *Service) resolvePrivateCredentials(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, string, error) {
	credentials, accountName, err := s.credentials.Get(ctx, token, accountID)
	if err == nil {
		return credentials, accountName, nil
	}
	if !isBindingInvalid(err) {
		return Credentials{}, "", err
	}
	recovered, recoveredName, recoverErr := s.recoverInvalidCredentials(ctx, token, accountID)
	if recoverErr != nil {
		if recoveredName == "" {
			recoveredName = accountName
		}
		return Credentials{}, recoveredName, fmt.Errorf("%w: %w", errCredentialRecoveryFailed, recoverErr)
	}
	return recovered, recoveredName, nil
}
