package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrAccountExists          = errors.New("platform account already exists")
	ErrAccountConflict        = errors.New("platform account has active work")
	ErrBrokerMissing          = errors.New("broker not found")
	ErrBrokerInactive         = errors.New("broker is not active")
	ErrDelegatedForbidden     = errors.New("delegated session denied")
	ErrInvalidPlatformAccount = errors.New("invalid platform account")
)

type PlatformAccountView struct {
	Username      string
	Permission    string
	BrokerID      string
	Status        string
	CreatedSource string
	CreatedBy     string
	AuthVersion   int64
	DeletedAt     *time.Time
}

type OwnerAccountRow struct {
	Username   string
	BrokerID   string
	Status     string
	Permission string
}

type CreatePlatformAccountInput struct {
	ActorUsername string
	ActorID       string
	BrokerID      string
	Username      string
	Password      string
	CreatedSource string
}

type MutatePlatformAccountInput struct {
	ActorUsername string
	ActorID       string
	BrokerID      string
	Username      string
}

func platformView(account Account) PlatformAccountView {
	return PlatformAccountView{
		Username:      account.Username,
		Permission:    account.Permission,
		BrokerID:      account.BrokerID,
		Status:        account.Status,
		CreatedSource: account.CreatedSource,
		CreatedBy:     account.CreatedBy,
		AuthVersion:   account.AuthVersion,
		DeletedAt:     account.DeletedAt,
	}
}

func (s *Service) ListPlatformAccountsInternal(
	ctx context.Context,
	serviceToken string,
	brokerID string,
) ([]PlatformAccountView, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return nil, err
	}
	repo, ok := s.accounts.(*Repository)
	if !ok {
		return nil, fmt.Errorf("platform account repository unavailable")
	}
	accounts, err := repo.ListPlatformAccounts(ctx, strings.TrimSpace(brokerID))
	if err != nil {
		return nil, err
	}
	views := make([]PlatformAccountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, platformView(account))
	}
	return views, nil
}

func (s *Service) CreatePlatformAccountInternal(
	ctx context.Context,
	serviceToken string,
	input CreatePlatformAccountInput,
) (PlatformAccountView, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return PlatformAccountView{}, err
	}
	repo, ok := s.accounts.(*Repository)
	if !ok {
		return PlatformAccountView{}, fmt.Errorf("platform account repository unavailable")
	}
	username := strings.TrimSpace(input.Username)
	password := input.Password
	if username == "" || password == "" || len(password) < 8 {
		return PlatformAccountView{}, ErrInvalidPlatformAccount
	}
	source := strings.TrimSpace(input.CreatedSource)
	if source == "" {
		source = CreatedSourceSelfQuant
	}
	brokerID := strings.TrimSpace(input.BrokerID)
	if source == CreatedSourceBroker {
		if brokerID == "" {
			return PlatformAccountView{}, ErrInvalidPlatformAccount
		}
	}
	if brokerID != "" {
		if err := repo.RequireActiveBroker(ctx, brokerID); err != nil {
			return PlatformAccountView{}, err
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return PlatformAccountView{}, err
	}
	createdBy := strings.TrimSpace(input.ActorID)
	if createdBy == "" {
		createdBy = strings.TrimSpace(input.ActorUsername)
	}
	account, err := repo.CreatePlatformAccount(ctx, Account{
		Username:      username,
		PasswordHash:  hash,
		Permission:    "user",
		BrokerID:      brokerID,
		Status:        AccountStatusActive,
		CreatedSource: source,
		CreatedBy:     createdBy,
		AuthVersion:   1,
	})
	if err != nil {
		return PlatformAccountView{}, err
	}
	_ = repo.WriteAudit(ctx, strings.TrimSpace(input.ActorUsername), strings.TrimSpace(input.ActorID),
		"create_platform_account", "account", username, map[string]any{
			"broker_id": brokerID, "created_source": source,
		})
	return platformView(account), nil
}

func (s *Service) DisablePlatformAccountInternal(
	ctx context.Context, serviceToken string, input MutatePlatformAccountInput,
) (PlatformAccountView, error) {
	return s.mutatePlatformAccount(ctx, serviceToken, input, "disable_platform_account", func(ctx context.Context, repo *Repository, account Account) (Account, error) {
		return repo.SetPlatformAccountStatus(ctx, account.Username, AccountStatusDisabled, true)
	})
}

func (s *Service) RestorePlatformAccountInternal(
	ctx context.Context, serviceToken string, input MutatePlatformAccountInput,
) (PlatformAccountView, error) {
	return s.mutatePlatformAccount(ctx, serviceToken, input, "restore_platform_account", func(ctx context.Context, repo *Repository, account Account) (Account, error) {
		if account.Status == AccountStatusArchived {
			return Account{}, ErrInvalidPlatformAccount
		}
		return repo.SetPlatformAccountStatus(ctx, account.Username, AccountStatusActive, true)
	})
}

func (s *Service) ResetPlatformAccountPasswordInternal(
	ctx context.Context, serviceToken string, input MutatePlatformAccountInput, password string,
) (PlatformAccountView, error) {
	if strings.TrimSpace(password) == "" || len(password) < 8 {
		return PlatformAccountView{}, ErrInvalidPlatformAccount
	}
	hash, err := HashPassword(password)
	if err != nil {
		return PlatformAccountView{}, err
	}
	return s.mutatePlatformAccount(ctx, serviceToken, input, "reset_platform_account_password", func(ctx context.Context, repo *Repository, account Account) (Account, error) {
		return repo.ResetPlatformAccountPassword(ctx, account.Username, hash)
	})
}

func (s *Service) UpdatePlatformAccountBrokerInternal(
	ctx context.Context, serviceToken string, input MutatePlatformAccountInput,
) (PlatformAccountView, error) {
	return s.mutatePlatformAccount(ctx, serviceToken, input, "update_platform_account_broker", func(ctx context.Context, repo *Repository, account Account) (Account, error) {
		brokerID := strings.TrimSpace(input.BrokerID)
		if brokerID != "" {
			if err := repo.RequireActiveBroker(ctx, brokerID); err != nil {
				return Account{}, err
			}
		}
		return repo.UpdatePlatformAccountBroker(ctx, account.Username, brokerID)
	})
}

func (s *Service) DeletePlatformAccountInternal(
	ctx context.Context, serviceToken string, input MutatePlatformAccountInput,
) (PlatformAccountView, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return PlatformAccountView{}, err
	}
	repo, ok := s.accounts.(*Repository)
	if !ok {
		return PlatformAccountView{}, fmt.Errorf("platform account repository unavailable")
	}
	username := strings.TrimSpace(input.Username)
	account, err := s.visiblePlatformAccount(ctx, repo, username, strings.TrimSpace(input.BrokerID))
	if err != nil {
		return PlatformAccountView{}, err
	}
	result, err := repo.DeleteOrArchivePlatformAccount(ctx, account.Username)
	if err != nil {
		return PlatformAccountView{}, err
	}
	s.invalidateAccountStatus(username)
	_ = repo.WriteAudit(ctx, strings.TrimSpace(input.ActorUsername), strings.TrimSpace(input.ActorID),
		"delete_platform_account", "account", username, map[string]any{"result": result.Status})
	return platformView(result), nil
}

func (s *Service) IssueBrokerDelegatedSessionInternal(
	ctx context.Context, serviceToken, brokerID, targetUsername, actorID string,
) (Session, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Session{}, err
	}
	_ = actorID
	targetUsername = strings.TrimSpace(targetUsername)
	brokerID = strings.TrimSpace(brokerID)
	if targetUsername == "" || brokerID == "" {
		return Session{}, ErrDelegatedForbidden
	}
	account, err := s.lookupAccountStatus(ctx, targetUsername)
	if err != nil {
		return Session{}, ErrDelegatedForbidden
	}
	if !accountUsable(account) || account.Permission != "user" || account.BrokerID != brokerID {
		return Session{}, ErrDelegatedForbidden
	}
	token, err := s.tokens.Issue(account.Username, account.Permission, account.AuthVersion)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, Username: account.Username, Permission: account.Permission}, nil
}

func (s *Service) GetAdminAccountSummaryInternal(
	ctx context.Context, serviceToken string,
) ([]OwnerAccountRow, error) {
	if err := s.authorizeAdmin(serviceToken); err != nil {
		return nil, err
	}
	repo, ok := s.accounts.(*Repository)
	if !ok {
		return nil, fmt.Errorf("platform account repository unavailable")
	}
	return repo.ListOwnerAccountRows(ctx)
}

func (s *Service) mutatePlatformAccount(
	ctx context.Context,
	serviceToken string,
	input MutatePlatformAccountInput,
	action string,
	fn func(context.Context, *Repository, Account) (Account, error),
) (PlatformAccountView, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return PlatformAccountView{}, err
	}
	repo, ok := s.accounts.(*Repository)
	if !ok {
		return PlatformAccountView{}, fmt.Errorf("platform account repository unavailable")
	}
	username := strings.TrimSpace(input.Username)
	account, err := s.visiblePlatformAccount(ctx, repo, username, strings.TrimSpace(input.BrokerID))
	if err != nil {
		return PlatformAccountView{}, err
	}
	updated, err := fn(ctx, repo, account)
	if err != nil {
		return PlatformAccountView{}, err
	}
	s.invalidateAccountStatus(username)
	_ = repo.WriteAudit(ctx, strings.TrimSpace(input.ActorUsername), strings.TrimSpace(input.ActorID),
		action, "account", username, map[string]any{"broker_id": updated.BrokerID, "status": updated.Status})
	return platformView(updated), nil
}

func (s *Service) visiblePlatformAccount(
	ctx context.Context, repo *Repository, username, brokerScope string,
) (Account, error) {
	if username == "" {
		return Account{}, ErrNotFound
	}
	account, err := repo.GetByUsername(ctx, username)
	if err != nil {
		if brokerScope != "" {
			return Account{}, ErrNotFound
		}
		return Account{}, err
	}
	if brokerScope != "" && account.BrokerID != brokerScope {
		return Account{}, ErrNotFound
	}
	return account, nil
}

func (r *Repository) WriteAudit(
	ctx context.Context,
	actorUsername, actorID, action, resourceType, resourceID string,
	details map[string]any,
) error {
	if details == nil {
		details = map[string]any{}
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO admin_audit_events (actor_username, actor_id, action, resource_type, resource_id, details)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		actorUsername, actorID, action, resourceType, resourceID, string(payload))
	return err
}
