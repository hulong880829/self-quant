package rpc

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	aiv1 "selfquant/backend/gen/ai/v1"
	"selfquant/backend/internal/ai"
)

type aiService interface {
	GetCredentialStatus(context.Context, string, string) (ai.CredentialStatus, error)
	TestCredential(context.Context, string, string) (ai.CredentialStatus, error)
	ListConversations(context.Context, string, int, string) ([]ai.Conversation, string, error)
	CreateConversation(context.Context, string, string) (ai.Conversation, error)
	RenameConversation(context.Context, string, string, string) (ai.Conversation, error)
	DeleteConversation(context.Context, string, string) error
	ListMessages(context.Context, string, string, int, string) ([]ai.ConversationMessage, string, error)
	StreamChat(context.Context, string, ai.StreamChatInput, func(ai.StreamEvent) error) error
}

type AIServer struct {
	aiv1.UnimplementedAIServiceServer
	service aiService
}

func NewAIServer(service aiService) *AIServer {
	return &AIServer{service: service}
}

func (s *AIServer) GetCredentialStatus(
	ctx context.Context,
	request *aiv1.GetCredentialStatusRequest,
) (*aiv1.CredentialStatusResponse, error) {
	result, err := s.service.GetCredentialStatus(ctx, request.GetToken(), request.GetProvider())
	if err != nil {
		return nil, mapAIError(err)
	}
	return &aiv1.CredentialStatusResponse{Credential: credentialStatusToProto(result)}, nil
}

func (s *AIServer) TestCredential(
	ctx context.Context,
	request *aiv1.TestCredentialRequest,
) (*aiv1.CredentialStatusResponse, error) {
	result, err := s.service.TestCredential(ctx, request.GetToken(), request.GetProvider())
	if err != nil {
		return nil, mapAIError(err)
	}
	return &aiv1.CredentialStatusResponse{Credential: credentialStatusToProto(result)}, nil
}

func (s *AIServer) ListConversations(
	ctx context.Context,
	request *aiv1.ListConversationsRequest,
) (*aiv1.ListConversationsResponse, error) {
	items, nextCursor, err := s.service.ListConversations(
		ctx, request.GetToken(), int(request.GetLimit()), request.GetCursor(),
	)
	if err != nil {
		return nil, mapAIError(err)
	}
	result := make([]*aiv1.Conversation, 0, len(items))
	for _, item := range items {
		result = append(result, conversationToProto(item))
	}
	return &aiv1.ListConversationsResponse{Items: result, NextCursor: nextCursor}, nil
}

func (s *AIServer) CreateConversation(
	ctx context.Context,
	request *aiv1.CreateConversationRequest,
) (*aiv1.ConversationResponse, error) {
	item, err := s.service.CreateConversation(ctx, request.GetToken(), request.GetModelAlias())
	if err != nil {
		return nil, mapAIError(err)
	}
	return &aiv1.ConversationResponse{Conversation: conversationToProto(item)}, nil
}

func (s *AIServer) RenameConversation(
	ctx context.Context,
	request *aiv1.RenameConversationRequest,
) (*aiv1.ConversationResponse, error) {
	item, err := s.service.RenameConversation(
		ctx, request.GetToken(), request.GetConversationId(), request.GetTitle(),
	)
	if err != nil {
		return nil, mapAIError(err)
	}
	return &aiv1.ConversationResponse{Conversation: conversationToProto(item)}, nil
}

func (s *AIServer) DeleteConversation(
	ctx context.Context,
	request *aiv1.DeleteConversationRequest,
) (*aiv1.DeleteConversationResponse, error) {
	if err := s.service.DeleteConversation(
		ctx, request.GetToken(), request.GetConversationId(),
	); err != nil {
		return nil, mapAIError(err)
	}
	return &aiv1.DeleteConversationResponse{}, nil
}

func (s *AIServer) ListMessages(
	ctx context.Context,
	request *aiv1.ListMessagesRequest,
) (*aiv1.ListMessagesResponse, error) {
	items, nextCursor, err := s.service.ListMessages(
		ctx, request.GetToken(), request.GetConversationId(),
		int(request.GetLimit()), request.GetCursor(),
	)
	if err != nil {
		return nil, mapAIError(err)
	}
	result := make([]*aiv1.ConversationMessage, 0, len(items))
	for _, item := range items {
		result = append(result, conversationMessageToProto(item))
	}
	return &aiv1.ListMessagesResponse{Items: result, NextCursor: nextCursor}, nil
}

func (s *AIServer) StreamChat(
	request *aiv1.StreamChatRequest,
	stream aiv1.AIService_StreamChatServer,
) error {
	err := s.service.StreamChat(
		stream.Context(),
		request.GetToken(),
		ai.StreamChatInput{
			ConversationID:  request.GetConversationId(),
			Prompt:          request.GetPrompt(),
			ClientMessageID: request.GetClientMessageId(),
			ModelAlias:      request.GetModelAlias(),
		},
		func(event ai.StreamEvent) error {
			return stream.Send(&aiv1.StreamChatEvent{
				Type: event.Type, Delta: event.Delta, Tool: event.Tool, Error: event.Error,
				InputTokens: int32(event.InputTokens), OutputTokens: int32(event.OutputTokens),
				ConversationId:     event.ConversationID,
				UserMessageId:      event.UserMessageID,
				AssistantMessageId: event.AssistantMessageID,
				Title:              event.Title,
				Status:             event.Status,
			})
		},
	)
	if err != nil {
		return mapAIError(err)
	}
	return nil
}

func conversationToProto(item ai.Conversation) *aiv1.Conversation {
	return &aiv1.Conversation{
		Id: item.ID, Title: item.Title,
		LastMessagePreview: item.LastMessagePreview,
		MessageCount:       int32(item.MessageCount), ModelAlias: item.ModelAlias,
		LastMessageAt: timestamppb.New(item.LastMessageAt),
		ExpiresAt:     timestamppb.New(item.ExpiresAt),
		CreatedAt:     timestamppb.New(item.CreatedAt),
		UpdatedAt:     timestamppb.New(item.UpdatedAt),
	}
}

func conversationMessageToProto(item ai.ConversationMessage) *aiv1.ConversationMessage {
	return &aiv1.ConversationMessage{
		Id: item.ID, ConversationId: item.ConversationID, TurnId: item.TurnID,
		ClientMessageId: item.ClientMessageID, Role: item.Role, Status: item.Status,
		Content: item.Content, Tool: item.Tool,
		InputTokens: int32(item.InputTokens), OutputTokens: int32(item.OutputTokens),
		CreatedAt: timestamppb.New(item.CreatedAt),
		UpdatedAt: timestamppb.New(item.UpdatedAt),
	}
}

func credentialStatusToProto(item ai.CredentialStatus) *aiv1.CredentialStatus {
	result := &aiv1.CredentialStatus{
		Provider: item.Provider, ApiKeyMasked: item.APIKeyMasked,
		Status: item.Status, LastError: item.LastError,
	}
	if !item.LastTestedAt.IsZero() {
		result.LastTestedAt = timestamppb.New(item.LastTestedAt)
	}
	return result
}

func mapAIError(err error) error {
	if status.Code(err) == codes.Unauthenticated {
		return status.Error(codes.Unauthenticated, "invalid session")
	}
	switch {
	case errors.Is(err, ai.ErrInvalidRequest):
		return status.Error(codes.InvalidArgument, "invalid ai request")
	case errors.Is(err, ai.ErrCredentialMissing):
		return status.Error(codes.NotFound, "ai credential not configured")
	case errors.Is(err, ai.ErrCredentialInvalid):
		return status.Error(codes.FailedPrecondition, "ai credential is not valid")
	case errors.Is(err, ai.ErrRateLimited):
		return status.Error(codes.ResourceExhausted, "ai provider rate limited")
	case errors.Is(err, ai.ErrProviderUnavailable):
		return status.Error(codes.Unavailable, "ai provider unavailable")
	case errors.Is(err, ai.ErrConversationNotFound):
		return status.Error(codes.NotFound, "ai conversation not found")
	case errors.Is(err, ai.ErrConversationBusy):
		return status.Error(codes.AlreadyExists, "ai conversation already streaming")
	case errors.Is(err, ai.ErrConversationLimit):
		return status.Error(codes.ResourceExhausted, "ai conversation limit reached")
	case errors.Is(err, ai.ErrRawMessageLimit):
		return status.Error(codes.ResourceExhausted, "ai conversation raw message limit reached")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	default:
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "invalid session") {
			return status.Error(codes.Unauthenticated, "invalid session")
		}
		return status.Error(codes.Internal, "ai request failed")
	}
}
