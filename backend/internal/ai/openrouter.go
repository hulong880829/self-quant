package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type OpenRouterProvider struct {
	client          *http.Client
	baseURL         string
	model           string
	referer         string
	title           string
	maxOutputTokens int
}

func NewOpenRouterProvider(
	client *http.Client,
	baseURL string,
	model string,
	referer string,
	title string,
	maxOutputTokens int,
) *OpenRouterProvider {
	return &OpenRouterProvider{
		client: client, baseURL: strings.TrimRight(baseURL, "/"), model: model,
		referer: referer, title: title, maxOutputTokens: maxOutputTokens,
	}
}

func (p *OpenRouterProvider) Name() string {
	return ProviderOpenRouter
}

func (p *OpenRouterProvider) ValidateKey(ctx context.Context, apiKey string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/key", nil)
	if err != nil {
		return fmt.Errorf("build provider key request: %w", err)
	}
	p.setHeaders(request, apiKey)
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: validate provider key: %v", ErrProviderUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return providerHTTPError(response)
	}
	return nil
}

func (p *OpenRouterProvider) StreamChat(
	ctx context.Context,
	apiKey string,
	request ChatRequest,
	emit func(StreamEvent) error,
) error {
	if request.ModelAlias != "" && request.ModelAlias != ModelFreeGeneral {
		return fmt.Errorf("%w: unsupported model alias", ErrInvalidRequest)
	}
	body, err := json.Marshal(map[string]any{
		"model":      p.model,
		"messages":   request.Messages,
		"stream":     true,
		"max_tokens": p.outputLimit(request.MaxOutputTokens),
	})
	if err != nil {
		return fmt.Errorf("encode provider request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build provider chat request: %w", err)
	}
	p.setHeaders(httpRequest, apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	response, err := p.client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("%w: provider chat request: %v", ErrProviderUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return providerHTTPError(response)
	}
	return readOpenRouterStream(response.Body, emit)
}

func (p *OpenRouterProvider) CompleteChat(
	ctx context.Context,
	apiKey string,
	request ChatRequest,
) (string, error) {
	if request.ModelAlias != "" && request.ModelAlias != ModelFreeGeneral {
		return "", fmt.Errorf("%w: unsupported model alias", ErrInvalidRequest)
	}
	body, err := json.Marshal(map[string]any{
		"model":      p.model,
		"messages":   request.Messages,
		"stream":     false,
		"max_tokens": p.outputLimit(request.MaxOutputTokens),
	})
	if err != nil {
		return "", fmt.Errorf("encode provider completion request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("build provider completion request: %w", err)
	}
	p.setHeaders(httpRequest, apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := p.client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return "", fmt.Errorf("%w: provider completion request: %v", ErrProviderUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", providerHTTPError(response)
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("%w: decode provider completion", ErrProviderUnavailable)
	}
	if payload.Error != nil {
		return "", &ProviderError{
			StatusCode: payload.Error.Code,
			Message:    sanitizeProviderMessage(payload.Error.Message),
		}
	}
	if len(payload.Choices) == 0 || strings.TrimSpace(payload.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("%w: provider returned an empty completion", ErrProviderUnavailable)
	}
	return strings.TrimSpace(payload.Choices[0].Message.Content), nil
}

func (p *OpenRouterProvider) outputLimit(requested int) int {
	if requested > 0 && requested < p.maxOutputTokens {
		return requested
	}
	return p.maxOutputTokens
}

func (p *OpenRouterProvider) setHeaders(request *http.Request, apiKey string) {
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	if p.referer != "" {
		request.Header.Set("HTTP-Referer", p.referer)
	}
	if p.title != "" {
		request.Header.Set("X-OpenRouter-Title", p.title)
	}
}

type openRouterChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

func readOpenRouterStream(reader io.Reader, emit func(StreamEvent) error) error {
	buffered := bufio.NewReader(reader)
	dataLines := make([]string, 0, 1)
	for {
		line, readErr := buffered.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) > 0 {
				done, err := handleOpenRouterData(strings.Join(dataLines, "\n"), emit)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
				dataLines = dataLines[:0]
			}
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && len(dataLines) > 0 {
				done, err := handleOpenRouterData(strings.Join(dataLines, "\n"), emit)
				if err != nil || done {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return fmt.Errorf("%w: provider stream ended before done", ErrProviderUnavailable)
			}
			return fmt.Errorf("%w: read provider stream: %v", ErrProviderUnavailable, readErr)
		}
	}
}

func handleOpenRouterData(data string, emit func(StreamEvent) error) (bool, error) {
	if data == "[DONE]" {
		return true, emit(StreamEvent{Type: "done"})
	}
	var chunk openRouterChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, fmt.Errorf("%w: decode provider stream", ErrProviderUnavailable)
	}
	if chunk.Error != nil {
		return false, &ProviderError{
			StatusCode: chunk.Error.Code,
			Message:    sanitizeProviderMessage(chunk.Error.Message),
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			if err := emit(StreamEvent{Type: "delta", Delta: choice.Delta.Content}); err != nil {
				return false, err
			}
		}
	}
	if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
		if err := emit(StreamEvent{
			Type: "usage", InputTokens: chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
		}); err != nil {
			return false, err
		}
	}
	return false, nil
}

func providerHTTPError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	message := response.Status
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
		message = payload.Error.Message
	}
	return &ProviderError{
		StatusCode: response.StatusCode,
		Message:    sanitizeProviderMessage(message),
	}
}

func sanitizeProviderMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 500 {
		message = message[:500]
	}
	if parsed, err := url.QueryUnescape(message); err == nil {
		message = parsed
	}
	return message
}
