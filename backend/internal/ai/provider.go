package ai

import (
	"context"
	"errors"
	"fmt"
)

const (
	ProviderOpenRouter = "openrouter"
	ModelFreeGeneral   = "free-general"
)

var (
	ErrInvalidRequest      = errors.New("invalid ai request")
	ErrCredentialMissing   = errors.New("ai credential missing")
	ErrCredentialInvalid   = errors.New("ai credential invalid")
	ErrProviderUnavailable = errors.New("ai provider unavailable")
	ErrRateLimited         = errors.New("ai provider rate limited")
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	ModelAlias      string
	Messages        []Message
	MaxOutputTokens int
}

type StreamEvent struct {
	Type               string
	Delta              string
	Tool               string
	Error              string
	InputTokens        int
	OutputTokens       int
	ConversationID     string
	UserMessageID      string
	AssistantMessageID string
	Title              string
	Status             string
}

type ProviderError struct {
	StatusCode int
	Message    string
}

func (e *ProviderError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("provider request failed with status %d", e.StatusCode)
	}
	return e.Message
}

func (e *ProviderError) Unwrap() error {
	switch e.StatusCode {
	case 401, 403:
		return ErrCredentialInvalid
	case 429:
		return ErrRateLimited
	default:
		return ErrProviderUnavailable
	}
}

type ModelProvider interface {
	Name() string
	ValidateKey(context.Context, string) error
	CompleteChat(context.Context, string, ChatRequest) (string, error)
	StreamChat(context.Context, string, ChatRequest, func(StreamEvent) error) error
}
