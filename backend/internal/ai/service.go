package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type credentialProvider interface {
	Owner(context.Context, string) (string, error)
	Status(context.Context, string, string) (CredentialStatus, error)
	Secret(context.Context, string, string) (string, error)
	UpdateStatus(context.Context, string, string, string, string) (CredentialStatus, error)
}

type conversationStore interface {
	CreateConversation(context.Context, string, string) (Conversation, error)
	ListConversations(context.Context, string, int, string) ([]Conversation, string, error)
	RenameConversation(context.Context, string, string, string) (Conversation, error)
	DeleteConversation(context.Context, string, string) error
	ListMessages(context.Context, string, string, int, string) ([]ConversationMessage, string, error)
	BeginTurn(context.Context, string, string, string, string, string) (BeginTurnResult, error)
	FinishTurn(context.Context, string, string, string, string, string, string, int, int) (ConversationMessage, error)
	LoadContext(context.Context, string, string, int) (Conversation, []ConversationMessage, error)
	LoadSummaryCandidates(context.Context, string, string, int, int) (Conversation, []ConversationMessage, error)
	UpdateSummary(context.Context, string, string, int, string, int64) (bool, error)
}

type ServiceConfig struct {
	MaxPromptChars         int
	InputTokenBudget       int
	RecentMessagePairs     int
	SummaryTriggerTokens   int
	SummaryMaxChars        int
	SummaryMaxOutputTokens int
}

type StreamChatInput struct {
	ConversationID  string
	Prompt          string
	ClientMessageID string
	ModelAlias      string
}

type Service struct {
	credentials   credentialProvider
	providers     *ProviderRegistry
	context       ContextProvider
	conversations conversationStore
	config        ServiceConfig
}

func NewService(
	credentials credentialProvider,
	providers *ProviderRegistry,
	contextProvider ContextProvider,
	conversations conversationStore,
	config ServiceConfig,
) *Service {
	return &Service{
		credentials: credentials, providers: providers, context: contextProvider,
		conversations: conversations, config: config,
	}
}

func (s *Service) GetCredentialStatus(
	ctx context.Context,
	token string,
	providerName string,
) (CredentialStatus, error) {
	token, providerName, err := validateCredentialRequest(token, providerName)
	if err != nil {
		return CredentialStatus{}, err
	}
	return s.credentials.Status(ctx, token, providerName)
}

func (s *Service) TestCredential(
	ctx context.Context,
	token string,
	providerName string,
) (CredentialStatus, error) {
	token, providerName, err := validateCredentialRequest(token, providerName)
	if err != nil {
		return CredentialStatus{}, err
	}
	provider, err := s.providers.Get(providerName)
	if err != nil {
		return CredentialStatus{}, err
	}
	apiKey, err := s.credentials.Secret(ctx, token, providerName)
	if err != nil {
		return CredentialStatus{}, err
	}
	if err := provider.ValidateKey(ctx, apiKey); err != nil {
		credentialStatus := "unknown"
		if errors.Is(err, ErrCredentialInvalid) {
			credentialStatus = "invalid"
		}
		updated, updateErr := s.credentials.UpdateStatus(
			ctx, token, providerName, credentialStatus, publicProviderError(err),
		)
		if updateErr != nil {
			return CredentialStatus{}, fmt.Errorf("test provider credential: %w", err)
		}
		return updated, nil
	}
	return s.credentials.UpdateStatus(ctx, token, providerName, "valid", "")
}

func (s *Service) ListConversations(
	ctx context.Context,
	token string,
	limit int,
	cursor string,
) ([]Conversation, string, error) {
	owner, err := s.owner(ctx, token)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		if _, err := uuid.Parse(cursor); err != nil {
			return nil, "", ErrInvalidRequest
		}
	}
	return s.conversations.ListConversations(ctx, owner, limit, cursor)
}

func (s *Service) CreateConversation(
	ctx context.Context,
	token string,
	modelAlias string,
) (Conversation, error) {
	owner, err := s.owner(ctx, token)
	if err != nil {
		return Conversation{}, err
	}
	modelAlias = normalizeModelAlias(modelAlias)
	if modelAlias == "" {
		return Conversation{}, ErrInvalidRequest
	}
	return s.conversations.CreateConversation(ctx, owner, modelAlias)
}

func (s *Service) RenameConversation(
	ctx context.Context,
	token string,
	conversationID string,
	title string,
) (Conversation, error) {
	owner, err := s.owner(ctx, token)
	if err != nil {
		return Conversation{}, err
	}
	if _, err := uuid.Parse(strings.TrimSpace(conversationID)); err != nil {
		return Conversation{}, ErrInvalidRequest
	}
	title = strings.TrimSpace(title)
	if title == "" || len([]rune(title)) > 80 {
		return Conversation{}, ErrInvalidRequest
	}
	return s.conversations.RenameConversation(ctx, owner, conversationID, title)
}

func (s *Service) DeleteConversation(
	ctx context.Context,
	token string,
	conversationID string,
) error {
	owner, err := s.owner(ctx, token)
	if err != nil {
		return err
	}
	if _, err := uuid.Parse(strings.TrimSpace(conversationID)); err != nil {
		return ErrInvalidRequest
	}
	return s.conversations.DeleteConversation(ctx, owner, conversationID)
}

func (s *Service) ListMessages(
	ctx context.Context,
	token string,
	conversationID string,
	limit int,
	cursor string,
) ([]ConversationMessage, string, error) {
	owner, err := s.owner(ctx, token)
	if err != nil {
		return nil, "", err
	}
	if _, err := uuid.Parse(strings.TrimSpace(conversationID)); err != nil {
		return nil, "", ErrInvalidRequest
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 50 {
		limit = 50
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" {
		if _, err := uuid.Parse(cursor); err != nil {
			return nil, "", ErrInvalidRequest
		}
	}
	return s.conversations.ListMessages(ctx, owner, conversationID, limit, cursor)
}

func (s *Service) StreamChat(
	ctx context.Context,
	token string,
	input StreamChatInput,
	emit func(StreamEvent) error,
) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("%w: session required", ErrInvalidRequest)
	}
	input.ConversationID = strings.TrimSpace(input.ConversationID)
	input.ClientMessageID = strings.TrimSpace(input.ClientMessageID)
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.ModelAlias = normalizeModelAlias(input.ModelAlias)
	if _, err := uuid.Parse(input.ConversationID); err != nil {
		return ErrInvalidRequest
	}
	if _, err := uuid.Parse(input.ClientMessageID); err != nil {
		return ErrInvalidRequest
	}
	if input.Prompt == "" || len([]rune(input.Prompt)) > s.config.MaxPromptChars ||
		input.ModelAlias == "" ||
		estimateTokens(input.Prompt)+16 > s.config.InputTokenBudget {
		return ErrInvalidRequest
	}
	owner, err := s.owner(ctx, token)
	if err != nil {
		return err
	}
	credentialStatus, err := s.credentials.Status(ctx, token, ProviderOpenRouter)
	if err != nil {
		return err
	}
	if credentialStatus.Status != "valid" {
		return ErrCredentialInvalid
	}
	apiKey, err := s.credentials.Secret(ctx, token, ProviderOpenRouter)
	if err != nil {
		return err
	}
	provider, err := s.providers.Get(ProviderOpenRouter)
	if err != nil {
		return err
	}

	turn, err := s.conversations.BeginTurn(
		ctx, owner, input.ConversationID, input.ClientMessageID,
		input.Prompt, input.ModelAlias,
	)
	if err != nil {
		return err
	}
	if err := emit(StreamEvent{
		Type: "turn", ConversationID: turn.Conversation.ID,
		UserMessageID:      turn.UserMessage.ID,
		AssistantMessageID: turn.AssistantMessage.ID,
		Title:              turn.Conversation.Title, Status: turn.AssistantMessage.Status,
	}); err != nil {
		if !turn.Duplicate {
			s.finishFailedTurn(owner, turn, "", "stopped", 0, 0)
		}
		return err
	}
	if turn.Duplicate && turn.AssistantMessage.Status == "completed" {
		if turn.AssistantMessage.Tool != "" {
			if err := emit(StreamEvent{Type: "tool", Tool: turn.AssistantMessage.Tool}); err != nil {
				return err
			}
		}
		if turn.AssistantMessage.Content != "" {
			if err := emit(StreamEvent{Type: "delta", Delta: turn.AssistantMessage.Content}); err != nil {
				return err
			}
		}
		return emit(StreamEvent{
			Type: "done", ConversationID: turn.Conversation.ID,
			UserMessageID:      turn.UserMessage.ID,
			AssistantMessageID: turn.AssistantMessage.ID,
			Title:              turn.Conversation.Title, Status: "completed",
			InputTokens:  turn.AssistantMessage.InputTokens,
			OutputTokens: turn.AssistantMessage.OutputTokens,
		})
	}

	conversation, storedMessages, err := s.conversations.LoadContext(
		ctx, owner, input.ConversationID, maxInt(64, s.config.RecentMessagePairs*4+4),
	)
	if err != nil {
		s.finishFailedTurn(owner, turn, "", "failed", 0, 0)
		return err
	}
	contextText, contextErr := s.context.Build(ctx, input.Prompt)
	if contextErr != nil {
		contextText = "资金费只读上下文当前不可用。请明确告知用户无法核验实时资金费数据，不要编造实时数据。"
	}
	providerRequest := ChatRequest{
		ModelAlias: input.ModelAlias,
		Messages: assembleProviderMessages(
			contextText, conversation.Summary, storedMessages,
			turn.UserMessage.TurnID, input.Prompt,
			s.config.InputTokenBudget, s.config.RecentMessagePairs,
		),
	}
	if err := emit(StreamEvent{Type: "tool", Tool: s.context.Name()}); err != nil {
		s.finishFailedTurn(owner, turn, "", "stopped", 0, 0)
		return err
	}

	var content strings.Builder
	inputTokens := 0
	outputTokens := 0
	err = provider.StreamChat(ctx, apiKey, providerRequest, func(event StreamEvent) error {
		switch event.Type {
		case "delta":
			content.WriteString(event.Delta)
		case "usage":
			inputTokens = event.InputTokens
			outputTokens = event.OutputTokens
		case "done":
			return nil
		}
		return emit(event)
	})
	if credentialIsInvalid(err) {
		_, _ = s.credentials.UpdateStatus(
			context.WithoutCancel(ctx),
			token,
			ProviderOpenRouter,
			"invalid",
			"OpenRouter rejected this API key",
		)
	}
	if err != nil {
		statusName := "failed"
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			statusName = "stopped"
		}
		s.finishFailedTurn(owner, turn, content.String(), statusName, inputTokens, outputTokens)
		return err
	}
	if strings.TrimSpace(content.String()) == "" {
		s.finishFailedTurn(owner, turn, "", "failed", inputTokens, outputTokens)
		return fmt.Errorf("%w: provider returned no answer", ErrProviderUnavailable)
	}
	finished, err := s.finishTurn(
		owner, turn, "completed", content.String(), inputTokens, outputTokens,
	)
	if err != nil {
		return err
	}
	if err := emit(StreamEvent{
		Type: "done", ConversationID: turn.Conversation.ID,
		UserMessageID: turn.UserMessage.ID, AssistantMessageID: finished.ID,
		Title: turn.Conversation.Title, Status: "completed",
		InputTokens: inputTokens, OutputTokens: outputTokens,
	}); err != nil {
		return err
	}
	go s.summarizeConversation(
		context.Background(), token, owner, turn.Conversation.ID, apiKey, provider,
	)
	return nil
}

func (s *Service) owner(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrInvalidRequest
	}
	return s.credentials.Owner(ctx, token)
}

func (s *Service) finishTurn(
	owner string,
	turn BeginTurnResult,
	statusName string,
	content string,
	inputTokens int,
	outputTokens int,
) (ConversationMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.conversations.FinishTurn(
		ctx, owner, turn.Conversation.ID, turn.UserMessage.TurnID,
		statusName, content, s.context.Name(), inputTokens, outputTokens,
	)
}

func (s *Service) finishFailedTurn(
	owner string,
	turn BeginTurnResult,
	content string,
	statusName string,
	inputTokens int,
	outputTokens int,
) {
	_, _ = s.finishTurn(owner, turn, statusName, content, inputTokens, outputTokens)
}

func assembleProviderMessages(
	fundingContext string,
	summary string,
	stored []ConversationMessage,
	currentTurnID string,
	prompt string,
	tokenBudget int,
	recentPairs int,
) []Message {
	currentMessage := Message{Role: "user", Content: prompt}
	currentTokens := estimateMessageTokens(currentMessage)
	fundingBudget := maxInt(tokenBudget-currentTokens-6, 1)
	fundingContext = truncateToTokenBudget(fundingContext, fundingBudget)
	result := []Message{{Role: "system", Content: fundingContext}}
	used := estimateMessagesTokens(result)
	if summary = strings.TrimSpace(summary); summary != "" {
		summaryMessage := Message{
			Role: "system", Content: "较早对话摘要：\n" + summary,
		}
		if used+estimateMessageTokens(summaryMessage) < tokenBudget {
			result = append(result, summaryMessage)
			used += estimateMessageTokens(summaryMessage)
		}
	}

	type pair struct{ user, assistant string }
	pairsByTurn := make(map[string]*pair)
	order := make([]string, 0)
	for _, item := range stored {
		if item.TurnID == currentTurnID {
			continue
		}
		current := pairsByTurn[item.TurnID]
		if current == nil {
			current = &pair{}
			pairsByTurn[item.TurnID] = current
			order = append(order, item.TurnID)
		}
		if item.Role == "user" {
			current.user = item.Content
		} else if item.Role == "assistant" {
			current.assistant = item.Content
		}
	}
	selected := make([]pair, 0, recentPairs)
	for index := len(order) - 1; index >= 0 && len(selected) < recentPairs; index-- {
		item := pairsByTurn[order[index]]
		if item == nil || item.user == "" || item.assistant == "" {
			continue
		}
		pairTokens := estimateMessageTokens(Message{Role: "user", Content: item.user}) +
			estimateMessageTokens(Message{Role: "assistant", Content: item.assistant})
		if used+pairTokens+currentTokens > tokenBudget {
			break
		}
		selected = append(selected, *item)
		used += pairTokens
	}
	for index := len(selected) - 1; index >= 0; index-- {
		result = append(result,
			Message{Role: "user", Content: selected[index].user},
			Message{Role: "assistant", Content: selected[index].assistant},
		)
	}
	result = append(result, currentMessage)
	return result
}

func validateCredentialRequest(token, providerName string) (string, string, error) {
	token = strings.TrimSpace(token)
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if token == "" || providerName != ProviderOpenRouter {
		return "", "", ErrInvalidRequest
	}
	return token, providerName, nil
}

func normalizeModelAlias(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ModelFreeGeneral
	}
	if value != ModelFreeGeneral {
		return ""
	}
	return value
}

func publicProviderError(err error) string {
	switch {
	case errors.Is(err, ErrCredentialInvalid):
		return "OpenRouter rejected this API key"
	case errors.Is(err, ErrRateLimited):
		return "OpenRouter rate limit reached"
	default:
		return "OpenRouter is temporarily unavailable"
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
