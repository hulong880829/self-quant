package rpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	accountv1 "selfquant/backend/gen/account/v1"
	"selfquant/backend/internal/account"
)

type accountService interface {
	Login(context.Context, string, string) (account.Session, error)
	ValidateSession(string) (account.Session, error)
	ListTradingAccounts(context.Context, string) ([]account.TradingAccountView, error)
	CreateTradingAccount(context.Context, string, account.CreateTradingAccountInput) (account.TradingAccountView, error)
	CreatePolymarketTradingAccount(context.Context, string, account.CreatePolymarketTradingAccountInput) (account.TradingAccountView, error)
	GetPolymarketCredentials(context.Context, string, int64) (account.PolymarketCredentials, error)
	RefreshPolymarketCredentials(context.Context, string, int64) (account.PolymarketCredentials, error)
	InvalidatePolymarketCredentials(context.Context, string, int64) error
	DeleteTradingAccount(context.Context, string, int64) error
	GetTradingAccountSnapshot(context.Context, string, int64) (account.TradingAccountSnapshot, error)
	GetProductGroupSnapshot(context.Context, string, string) (account.ProductGroupSnapshot, error)
	GetProductAccountSnapshotsInternal(context.Context, string, string, string) (account.ProductAccountSnapshots, error)
	SyncProductTradeFillsInternal(context.Context, string, string, string, time.Time) ([]account.TradeFillSyncResult, error)
	GetTradingCredentials(context.Context, string, int64) (account.TradingCredentials, error)
	GetTradingCredentialsInternal(context.Context, string, string, int64) (account.TradingCredentials, error)
	GetAICredential(context.Context, string, string) (account.AICredentialView, error)
	UpsertAICredential(context.Context, string, string, string) (account.AICredentialView, error)
	DeleteAICredential(context.Context, string, string) error
	GetAICredentialSecret(context.Context, string, string) (account.AICredentialSecret, error)
	UpdateAICredentialStatus(context.Context, string, string, string, string) (account.AICredentialView, error)
}

func (s *AccountServer) GetProductAccountSnapshotsInternal(
	ctx context.Context,
	request *accountv1.GetProductAccountSnapshotsInternalRequest,
) (*accountv1.GetProductAccountSnapshotsInternalResponse, error) {
	result, err := s.service.GetProductAccountSnapshotsInternal(
		ctx, request.GetServiceToken(), request.GetOwnerUsername(), request.GetProductName(),
	)
	if err != nil {
		return nil, mapInternalAccountError(err)
	}
	response := &accountv1.GetProductAccountSnapshotsInternalResponse{
		Errors: result.Errors, ServerTime: timestamppb.Now(),
		Snapshots: make([]*accountv1.TradingAccountSnapshot, 0, len(result.Snapshots)),
	}
	for _, snapshot := range result.Snapshots {
		response.Snapshots = append(response.Snapshots, toProtoAccountSnapshot(snapshot))
	}
	return response, nil
}

func (s *AccountServer) SyncProductTradeFillsInternal(
	ctx context.Context,
	request *accountv1.SyncProductTradeFillsInternalRequest,
) (*accountv1.SyncProductTradeFillsInternalResponse, error) {
	through := time.Time{}
	if request.GetThroughTime() != nil {
		through = request.GetThroughTime().AsTime()
	}
	results, err := s.service.SyncProductTradeFillsInternal(
		ctx, request.GetServiceToken(), request.GetOwnerUsername(), request.GetProductName(), through,
	)
	if err != nil {
		return nil, mapInternalAccountError(err)
	}
	response := &accountv1.SyncProductTradeFillsInternalResponse{
		Results:    make([]*accountv1.TradeFillSyncResult, 0, len(results)),
		ServerTime: timestamppb.Now(),
	}
	for _, result := range results {
		response.Results = append(response.Results, &accountv1.TradeFillSyncResult{
			TradingAccountId: result.TradingAccountID, Exchange: result.Exchange,
			InsertedCount: int32(result.InsertedCount), SyncedThrough: optionalTimestamp(result.SyncedThrough),
			Error: result.Error,
		})
	}
	return response, nil
}

func (s *AccountServer) GetTradingCredentialsInternal(
	ctx context.Context,
	request *accountv1.GetTradingCredentialsInternalRequest,
) (*accountv1.GetTradingCredentialsResponse, error) {
	credentials, err := s.service.GetTradingCredentialsInternal(
		ctx, request.GetServiceToken(), request.GetOwnerUsername(), request.GetTradingAccountId(),
	)
	if err != nil {
		return nil, mapInternalAccountError(err)
	}
	return tradingCredentialsToProto(credentials), nil
}

func mapInternalAccountError(err error) error {
	switch {
	case errors.Is(err, account.ErrInvalidServiceToken):
		return status.Error(codes.Unauthenticated, "invalid service token")
	case errors.Is(err, account.ErrInvalidTradingAccount):
		return status.Error(codes.InvalidArgument, "owner and product are required")
	default:
		return status.Error(codes.Internal, "internal account source failed")
	}
}

func (s *AccountServer) GetTradingAccountSnapshot(
	ctx context.Context,
	request *accountv1.GetTradingAccountSnapshotRequest,
) (*accountv1.GetTradingAccountSnapshotResponse, error) {
	if strings.TrimSpace(request.GetToken()) == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	item, err := s.service.GetTradingAccountSnapshot(ctx, request.GetToken(), request.GetTradingAccountId())
	if err != nil {
		slog.ErrorContext(ctx, "account snapshot failed", "account_id", request.GetTradingAccountId(), "error", err)
		return nil, mapTradingAccountError(err, "account snapshot unavailable")
	}
	positions := make([]*accountv1.PortfolioPosition, 0, len(item.Positions))
	for _, position := range item.Positions {
		positions = append(positions, &accountv1.PortfolioPosition{
			Key: position.Key, Kind: position.Kind, Exchange: position.Exchange,
			Symbol: position.Symbol, Side: position.Side, NotionalUsd: position.NotionalUSD,
			Size: position.Size, EntryPrice: position.EntryPrice, MarkPrice: position.MarkPrice,
			UnrealizedPnl: position.UnrealizedPnL, MarketTitle: position.MarketTitle,
			Outcome: position.Outcome, InitialValue: position.InitialValue,
			CurrentValue: position.CurrentValue, CashPnl: position.CashPnL,
			ConditionId: position.ConditionID, TokenId: position.TokenID,
			EndTime:  optionalTimestamp(position.EndTime),
			SpotSize: position.SpotSize, SignedContractSize: position.SignedContractSize,
		})
	}
	return &accountv1.GetTradingAccountSnapshotResponse{
		Snapshot:   toProtoAccountSnapshotWithPositions(item, positions),
		ServerTime: timestamppb.Now(),
	}, nil
}

func toProtoAccountSnapshot(item account.TradingAccountSnapshot) *accountv1.TradingAccountSnapshot {
	positions := make([]*accountv1.PortfolioPosition, 0, len(item.Positions))
	for _, position := range item.Positions {
		positions = append(positions, &accountv1.PortfolioPosition{
			Key: position.Key, Kind: position.Kind, Exchange: position.Exchange,
			Symbol: position.Symbol, Side: position.Side, NotionalUsd: position.NotionalUSD,
			Size: position.Size, EntryPrice: position.EntryPrice, MarkPrice: position.MarkPrice,
			UnrealizedPnl: position.UnrealizedPnL, MarketTitle: position.MarketTitle,
			Outcome: position.Outcome, InitialValue: position.InitialValue,
			CurrentValue: position.CurrentValue, CashPnl: position.CashPnL,
			ConditionId: position.ConditionID, TokenId: position.TokenID,
			EndTime: optionalTimestamp(position.EndTime), SpotSize: position.SpotSize,
			SignedContractSize: position.SignedContractSize,
		})
	}
	return toProtoAccountSnapshotWithPositions(item, positions)
}

func toProtoAccountSnapshotWithPositions(
	item account.TradingAccountSnapshot,
	positions []*accountv1.PortfolioPosition,
) *accountv1.TradingAccountSnapshot {
	return &accountv1.TradingAccountSnapshot{
		TradingAccountId: item.TradingAccountID, ProductName: item.ProductName,
		Exchange: item.Exchange, AccountName: item.AccountName,
		AccountEquityUsd: item.AccountEquityUSD, AvailableFundsUsd: item.AvailableFundsUSD,
		RiskPercent: item.RiskPercent, Positions: positions,
		SourceUpdatedAt: optionalTimestamp(item.SourceUpdatedAt),
		Stale:           item.Stale, LastError: item.LastError,
	}
}

func (s *AccountServer) GetProductGroupSnapshot(
	ctx context.Context,
	request *accountv1.GetProductGroupSnapshotRequest,
) (*accountv1.GetProductGroupSnapshotResponse, error) {
	if strings.TrimSpace(request.GetToken()) == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	item, err := s.service.GetProductGroupSnapshot(ctx, request.GetToken(), request.GetProductName())
	if err != nil {
		slog.ErrorContext(ctx, "product group snapshot failed", "product", request.GetProductName(), "error", err)
		return nil, mapTradingAccountError(err, "product snapshot unavailable")
	}
	positions := make([]*accountv1.ProductGroupPosition, 0, len(item.Positions))
	for _, position := range item.Positions {
		positions = append(positions, &accountv1.ProductGroupPosition{
			Symbol: position.Symbol, Side: position.Side, TotalNotionalUsd: position.TotalNotionalUSD,
			SpotSize: position.SpotSize, ContractSize: position.ContractSize,
		})
	}
	return &accountv1.GetProductGroupSnapshotResponse{
		Snapshot: &accountv1.ProductGroupSnapshot{
			ProductName: item.ProductName, AccountCount: int32(item.AccountCount),
			Positions: positions, SourceUpdatedAt: timestamppb.New(item.SourceUpdatedAt),
			Stale: item.Stale, Partial: item.Partial, Errors: item.Errors,
			AccountEquityUsd: item.AccountEquityUSD, AvailableFundsUsd: item.AvailableFundsUSD,
		},
		ServerTime: timestamppb.Now(),
	}, nil
}

func optionalTimestamp(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value)
}

func (s *AccountServer) CreatePolymarketTradingAccount(
	ctx context.Context,
	request *accountv1.CreatePolymarketTradingAccountRequest,
) (*accountv1.CreatePolymarketTradingAccountResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	created, err := s.service.CreatePolymarketTradingAccount(
		ctx,
		token,
		account.CreatePolymarketTradingAccountInput{
			ProductName:   request.GetProductName(),
			AccountName:   request.GetAccountName(),
			PrivateKey:    request.GetPrivateKey(),
			WalletType:    request.GetWalletType(),
			FunderAddress: request.GetFunderAddress(),
		},
	)
	if err != nil {
		return nil, mapTradingAccountError(err, "create polymarket account failed")
	}
	return &accountv1.CreatePolymarketTradingAccountResponse{
		Account: toProtoTradingAccount(created),
	}, nil
}

func (s *AccountServer) GetPolymarketCredentials(
	ctx context.Context,
	request *accountv1.GetPolymarketCredentialsRequest,
) (*accountv1.GetPolymarketCredentialsResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	credentials, err := s.service.GetPolymarketCredentials(
		ctx, token, request.GetTradingAccountId(),
	)
	if err != nil {
		return nil, mapTradingAccountError(err, "get polymarket credentials failed")
	}
	return polymarketCredentialsToProto(credentials), nil
}

func polymarketCredentialsToProto(
	credentials account.PolymarketCredentials,
) *accountv1.GetPolymarketCredentialsResponse {
	return &accountv1.GetPolymarketCredentialsResponse{
		TradingAccountId: credentials.TradingAccountID,
		AccountName:      credentials.AccountName,
		SignerAddress:    credentials.SignerAddress,
		FunderAddress:    credentials.FunderAddress,
		WalletType:       credentials.WalletType,
		SignatureType:    credentials.SignatureType,
		PrivateKey:       credentials.PrivateKey,
		ApiKey:           credentials.APIKey,
		ApiSecret:        credentials.APISecret,
		Passphrase:       credentials.Passphrase,
	}
}

func (s *AccountServer) RefreshPolymarketCredentials(
	ctx context.Context,
	request *accountv1.GetPolymarketCredentialsRequest,
) (*accountv1.GetPolymarketCredentialsResponse, error) {
	if strings.TrimSpace(request.GetToken()) == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	credentials, err := s.service.RefreshPolymarketCredentials(
		ctx, request.GetToken(), request.GetTradingAccountId(),
	)
	if err != nil {
		return nil, mapTradingAccountError(err, "refresh polymarket credentials failed")
	}
	return polymarketCredentialsToProto(credentials), nil
}

func (s *AccountServer) InvalidatePolymarketCredentials(
	ctx context.Context,
	request *accountv1.GetPolymarketCredentialsRequest,
) (*accountv1.DeleteTradingAccountResponse, error) {
	if strings.TrimSpace(request.GetToken()) == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	if err := s.service.InvalidatePolymarketCredentials(
		ctx, request.GetToken(), request.GetTradingAccountId(),
	); err != nil {
		return nil, mapTradingAccountError(err, "invalidate polymarket credentials failed")
	}
	return &accountv1.DeleteTradingAccountResponse{}, nil
}

type AccountServer struct {
	accountv1.UnimplementedAccountServiceServer
	service accountService
}

func NewAccountServer(service accountService) *AccountServer {
	return &AccountServer{service: service}
}

func (s *AccountServer) Login(
	ctx context.Context,
	request *accountv1.LoginRequest,
) (*accountv1.LoginResponse, error) {
	username := strings.TrimSpace(request.GetUsername())
	password := request.GetPassword()
	if username == "" || password == "" {
		return nil, status.Error(codes.InvalidArgument, "username and password are required")
	}
	session, err := s.service.Login(ctx, username, password)
	if errors.Is(err, account.ErrInvalidCredentials) {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "login failed")
	}
	return &accountv1.LoginResponse{
		Token: session.Token, Username: session.Username, Permission: session.Permission,
	}, nil
}

func (s *AccountServer) ValidateSession(
	_ context.Context,
	request *accountv1.ValidateSessionRequest,
) (*accountv1.ValidateSessionResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	session, err := s.service.ValidateSession(token)
	if errors.Is(err, account.ErrInvalidToken) || errors.Is(err, account.ErrExpiredToken) {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "validate session failed")
	}
	return &accountv1.ValidateSessionResponse{
		Username: session.Username, Permission: session.Permission,
	}, nil
}

func (s *AccountServer) ListTradingAccounts(
	ctx context.Context,
	request *accountv1.ListTradingAccountsRequest,
) (*accountv1.ListTradingAccountsResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	items, err := s.service.ListTradingAccounts(ctx, token)
	if err != nil {
		return nil, mapTradingAccountError(err, "list trading accounts failed")
	}
	response := &accountv1.ListTradingAccountsResponse{
		Items: make([]*accountv1.TradingAccount, 0, len(items)),
	}
	for _, item := range items {
		response.Items = append(response.Items, toProtoTradingAccount(item))
	}
	return response, nil
}

func (s *AccountServer) CreateTradingAccount(
	ctx context.Context,
	request *accountv1.CreateTradingAccountRequest,
) (*accountv1.CreateTradingAccountResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	created, err := s.service.CreateTradingAccount(ctx, token, account.CreateTradingAccountInput{
		ProductName: request.GetProductName(),
		Exchange:    request.GetExchange(),
		AccountName: request.GetAccountName(),
		APIKey:      request.GetApiKey(),
		APISecret:   request.GetApiSecret(),
		Passphrase:  request.GetPassphrase(),
	})
	if err != nil {
		return nil, mapTradingAccountError(err, "create trading account failed")
	}
	return &accountv1.CreateTradingAccountResponse{
		Account: toProtoTradingAccount(created),
	}, nil
}

func (s *AccountServer) GetTradingCredentials(
	ctx context.Context,
	request *accountv1.GetTradingCredentialsRequest,
) (*accountv1.GetTradingCredentialsResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	if request.GetTradingAccountId() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "trading account id is required")
	}
	credentials, err := s.service.GetTradingCredentials(ctx, token, request.GetTradingAccountId())
	if err != nil {
		return nil, mapTradingAccountError(err, "get trading credentials failed")
	}
	return tradingCredentialsToProto(credentials), nil
}

func tradingCredentialsToProto(credentials account.TradingCredentials) *accountv1.GetTradingCredentialsResponse {
	return &accountv1.GetTradingCredentialsResponse{
		TradingAccountId: credentials.TradingAccountID,
		ProductName:      credentials.ProductName,
		Exchange:         credentials.Exchange,
		AccountName:      credentials.AccountName,
		ApiKey:           credentials.APIKey,
		ApiSecret:        credentials.APISecret,
		Passphrase:       credentials.Passphrase,
	}
}

func (s *AccountServer) GetAICredential(
	ctx context.Context,
	request *accountv1.GetAICredentialRequest,
) (*accountv1.AICredentialResponse, error) {
	credential, err := s.service.GetAICredential(ctx, request.GetToken(), request.GetProvider())
	if err != nil {
		return nil, mapAICredentialError(err)
	}
	return &accountv1.AICredentialResponse{Credential: aiCredentialToProto(credential)}, nil
}

func (s *AccountServer) UpsertAICredential(
	ctx context.Context,
	request *accountv1.UpsertAICredentialRequest,
) (*accountv1.AICredentialResponse, error) {
	credential, err := s.service.UpsertAICredential(
		ctx, request.GetToken(), request.GetProvider(), request.GetApiKey(),
	)
	if err != nil {
		return nil, mapAICredentialError(err)
	}
	return &accountv1.AICredentialResponse{Credential: aiCredentialToProto(credential)}, nil
}

func (s *AccountServer) DeleteAICredential(
	ctx context.Context,
	request *accountv1.DeleteAICredentialRequest,
) (*accountv1.DeleteAICredentialResponse, error) {
	if err := s.service.DeleteAICredential(ctx, request.GetToken(), request.GetProvider()); err != nil {
		return nil, mapAICredentialError(err)
	}
	return &accountv1.DeleteAICredentialResponse{}, nil
}

func (s *AccountServer) GetAICredentialSecret(
	ctx context.Context,
	request *accountv1.GetAICredentialRequest,
) (*accountv1.AICredentialSecretResponse, error) {
	secret, err := s.service.GetAICredentialSecret(ctx, request.GetToken(), request.GetProvider())
	if err != nil {
		return nil, mapAICredentialError(err)
	}
	return &accountv1.AICredentialSecretResponse{
		Provider: secret.Provider,
		ApiKey:   secret.APIKey,
	}, nil
}

func (s *AccountServer) UpdateAICredentialStatus(
	ctx context.Context,
	request *accountv1.UpdateAICredentialStatusRequest,
) (*accountv1.AICredentialResponse, error) {
	credential, err := s.service.UpdateAICredentialStatus(
		ctx,
		request.GetToken(),
		request.GetProvider(),
		request.GetStatus(),
		request.GetErrorMessage(),
	)
	if err != nil {
		return nil, mapAICredentialError(err)
	}
	return &accountv1.AICredentialResponse{Credential: aiCredentialToProto(credential)}, nil
}

func aiCredentialToProto(item account.AICredentialView) *accountv1.AICredential {
	return &accountv1.AICredential{
		Provider:     item.Provider,
		ApiKeyMasked: item.APIKeyMasked,
		Status:       item.Status,
		LastError:    item.LastError,
		LastTestedAt: optionalTimestamp(item.LastTestedAt),
		CreatedAt:    optionalTimestamp(item.CreatedAt),
		UpdatedAt:    optionalTimestamp(item.UpdatedAt),
	}
}

func mapAICredentialError(err error) error {
	switch {
	case errors.Is(err, account.ErrInvalidToken), errors.Is(err, account.ErrExpiredToken):
		return status.Error(codes.Unauthenticated, "invalid session")
	case errors.Is(err, account.ErrInvalidAICredential):
		return status.Error(codes.InvalidArgument, "invalid ai credential")
	case errors.Is(err, account.ErrAICredentialNotFound):
		return status.Error(codes.NotFound, "ai credential not found")
	default:
		return status.Error(codes.Internal, "ai credential operation failed")
	}
}

func (s *AccountServer) DeleteTradingAccount(
	ctx context.Context,
	request *accountv1.DeleteTradingAccountRequest,
) (*accountv1.DeleteTradingAccountResponse, error) {
	token := strings.TrimSpace(request.GetToken())
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "session required")
	}
	if request.GetId() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "trading account id is required")
	}
	if err := s.service.DeleteTradingAccount(ctx, token, request.GetId()); err != nil {
		return nil, mapTradingAccountError(err, "delete trading account failed")
	}
	return &accountv1.DeleteTradingAccountResponse{}, nil
}

func toProtoTradingAccount(item account.TradingAccountView) *accountv1.TradingAccount {
	return &accountv1.TradingAccount{
		Id:            item.ID,
		ProductName:   item.ProductName,
		Exchange:      item.Exchange,
		AccountName:   item.AccountName,
		ApiKeyMasked:  item.APIKeyMasked,
		HasPassphrase: item.HasPassphrase,
		CreatedAt:     timestamppb.New(item.CreatedAt),
		UpdatedAt:     timestamppb.New(item.UpdatedAt),
		WalletAddress: item.WalletAddress,
		WalletType:    item.WalletType,
		BindingStatus: item.BindingStatus,
	}
}

func mapTradingAccountError(err error, internalMessage string) error {
	switch {
	case errors.Is(err, account.ErrInvalidToken), errors.Is(err, account.ErrExpiredToken):
		return status.Error(codes.Unauthenticated, "invalid session")
	case errors.Is(err, account.ErrInvalidTradingAccount),
		errors.Is(err, account.ErrUnsupportedExchange),
		errors.Is(err, account.ErrPassphraseRequired):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, account.ErrInvalidPolymarketAccount):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, account.ErrPolymarketCredentialsInvalid):
		return status.Error(codes.Unauthenticated, err.Error())
	case errors.Is(err, account.ErrDuplicateTradingAccount):
		return status.Error(codes.AlreadyExists, "trading account already exists")
	case errors.Is(err, account.ErrTradingAccountNotFound):
		return status.Error(codes.NotFound, "trading account not found")
	case errors.Is(err, account.ErrSnapshotUnavailable):
		return status.Error(codes.Unavailable, "account snapshot unavailable")
	default:
		return status.Error(codes.Internal, internalMessage)
	}
}
