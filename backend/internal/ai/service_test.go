package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCredentials struct {
	status  CredentialStatus
	secret  string
	err     error
	updates []CredentialStatus
}

func (f *fakeCredentials) Owner(context.Context, string) (string, error) {
	return "alice", f.err
}

func (f *fakeCredentials) Status(
	context.Context, string, string,
) (CredentialStatus, error) {
	return f.status, f.err
}

func (f *fakeCredentials) Secret(context.Context, string, string) (string, error) {
	return f.secret, f.err
}

func (f *fakeCredentials) UpdateStatus(
	_ context.Context,
	_ string,
	provider string,
	credentialStatus string,
	lastError string,
) (CredentialStatus, error) {
	result := CredentialStatus{
		Provider: provider, Status: credentialStatus, LastError: lastError,
	}
	f.updates = append(f.updates, result)
	f.status = result
	return result, nil
}

type fakeModelProvider struct {
	validateErr error
	streamErr   error
	streamCalls int
}

func (f *fakeModelProvider) Name() string {
	return ProviderOpenRouter
}

func (f *fakeModelProvider) ValidateKey(context.Context, string) error {
	return f.validateErr
}

func (f *fakeModelProvider) CompleteChat(
	context.Context, string, ChatRequest,
) (string, error) {
	return "summary", nil
}

func (f *fakeModelProvider) StreamChat(
	_ context.Context,
	_ string,
	_ ChatRequest,
	emit func(StreamEvent) error,
) error {
	f.streamCalls++
	if f.streamErr != nil {
		return f.streamErr
	}
	if err := emit(StreamEvent{Type: "delta", Delta: "answer"}); err != nil {
		return err
	}
	return emit(StreamEvent{Type: "done"})
}

type fakeContextProvider struct {
	value string
	err   error
}

type fakeConversationStore struct {
	finishedStatus string
}

func (f *fakeConversationStore) CreateConversation(
	context.Context, string, string,
) (Conversation, error) {
	return Conversation{}, nil
}
func (f *fakeConversationStore) ListConversations(
	context.Context, string, int, string,
) ([]Conversation, string, error) {
	return nil, "", nil
}
func (f *fakeConversationStore) RenameConversation(
	context.Context, string, string, string,
) (Conversation, error) {
	return Conversation{}, nil
}
func (f *fakeConversationStore) DeleteConversation(context.Context, string, string) error {
	return nil
}
func (f *fakeConversationStore) ListMessages(
	context.Context, string, string, int, string,
) ([]ConversationMessage, string, error) {
	return nil, "", nil
}
func (f *fakeConversationStore) BeginTurn(
	_ context.Context,
	_ string,
	conversationID string,
	clientMessageID string,
	prompt string,
	_ string,
) (BeginTurnResult, error) {
	now := time.Now()
	return BeginTurnResult{
		Conversation: Conversation{ID: conversationID, Title: "question"},
		UserMessage: ConversationMessage{
			ID:              "22222222-2222-2222-2222-222222222222",
			TurnID:          "33333333-3333-3333-3333-333333333333",
			ClientMessageID: clientMessageID, Role: "user", Status: "completed",
			Content: prompt, CreatedAt: now,
		},
		AssistantMessage: ConversationMessage{
			ID:     "44444444-4444-4444-4444-444444444444",
			TurnID: "33333333-3333-3333-3333-333333333333",
			Role:   "assistant", Status: "pending", CreatedAt: now,
		},
	}, nil
}
func (f *fakeConversationStore) FinishTurn(
	_ context.Context,
	_ string,
	conversationID string,
	turnID string,
	status string,
	content string,
	_ string,
	inputTokens int,
	outputTokens int,
) (ConversationMessage, error) {
	f.finishedStatus = status
	return ConversationMessage{
		ID:             "44444444-4444-4444-4444-444444444444",
		ConversationID: conversationID, TurnID: turnID,
		Role: "assistant", Status: status, Content: content,
		InputTokens: inputTokens, OutputTokens: outputTokens,
	}, nil
}
func (f *fakeConversationStore) LoadContext(
	context.Context, string, string, int,
) (Conversation, []ConversationMessage, error) {
	return Conversation{}, nil, nil
}
func (f *fakeConversationStore) LoadSummaryCandidates(
	context.Context, string, string, int, int,
) (Conversation, []ConversationMessage, error) {
	return Conversation{}, nil, nil
}
func (f *fakeConversationStore) UpdateSummary(
	context.Context, string, string, int, string, int64,
) (bool, error) {
	return true, nil
}

func newTestAIService(
	credentials *fakeCredentials,
	model *fakeModelProvider,
	store *fakeConversationStore,
	contextProvider fakeContextProvider,
) *Service {
	return NewService(
		credentials, NewProviderRegistry(model), contextProvider, store,
		ServiceConfig{
			MaxPromptChars: 1000, InputTokenBudget: 2000, RecentMessagePairs: 6,
			SummaryTriggerTokens: 1000, SummaryMaxChars: 500,
			SummaryMaxOutputTokens: 128,
		},
	)
}

var testStreamInput = StreamChatInput{
	ConversationID:  "11111111-1111-1111-1111-111111111111",
	ClientMessageID: "55555555-5555-5555-5555-555555555555",
	Prompt:          "question",
}

func (f fakeContextProvider) Name() string {
	return "funding"
}

func (f fakeContextProvider) Build(context.Context, string) (string, error) {
	return f.value, f.err
}

func TestStreamChatRequiresValidCredential(t *testing.T) {
	credentials := &fakeCredentials{
		status: CredentialStatus{Status: "invalid"}, secret: "key",
	}
	model := &fakeModelProvider{}
	service := newTestAIService(
		credentials, model, &fakeConversationStore{},
		fakeContextProvider{value: "context"},
	)
	err := service.StreamChat(
		context.Background(), "session", testStreamInput,
		func(StreamEvent) error { return nil },
	)
	if !errors.Is(err, ErrCredentialInvalid) || model.streamCalls != 0 {
		t.Fatalf("err=%v calls=%d", err, model.streamCalls)
	}
}

func TestStreamChatEmitsFundingToolAndDeltas(t *testing.T) {
	credentials := &fakeCredentials{
		status: CredentialStatus{Status: "valid"}, secret: "key",
	}
	model := &fakeModelProvider{}
	service := newTestAIService(
		credentials, model, &fakeConversationStore{},
		fakeContextProvider{value: "funding context"},
	)
	var events []StreamEvent
	err := service.StreamChat(
		context.Background(), "session", testStreamInput,
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Type != "turn" || events[1].Tool != "funding" ||
		events[2].Delta != "answer" || events[3].Type != "done" {
		t.Fatalf("events=%+v", events)
	}
}

func TestProviderUnauthorizedInvalidatesCredential(t *testing.T) {
	credentials := &fakeCredentials{
		status: CredentialStatus{Status: "valid"}, secret: "key",
	}
	model := &fakeModelProvider{streamErr: &ProviderError{StatusCode: 401}}
	service := newTestAIService(
		credentials, model, &fakeConversationStore{},
		fakeContextProvider{value: "context"},
	)
	err := service.StreamChat(
		context.Background(), "session", testStreamInput,
		func(StreamEvent) error { return nil },
	)
	if !errors.Is(err, ErrCredentialInvalid) || len(credentials.updates) != 1 ||
		credentials.updates[0].Status != "invalid" {
		t.Fatalf("err=%v updates=%+v", err, credentials.updates)
	}
}
