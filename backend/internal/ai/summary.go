package ai

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func estimateMessagesTokens(messages []Message) int {
	total := 0
	for _, message := range messages {
		total += estimateMessageTokens(message)
	}
	return total
}

func estimateMessageTokens(message Message) int {
	return estimateTokens(message.Content) + 6
}

func estimateTokens(value string) int {
	tokens := 0
	asciiRunes := 0
	for _, current := range value {
		if current <= unicode.MaxASCII {
			asciiRunes++
			continue
		}
		if asciiRunes > 0 {
			tokens += (asciiRunes + 3) / 4
			asciiRunes = 0
		}
		if unicode.Is(unicode.Han, current) ||
			unicode.Is(unicode.Hiragana, current) ||
			unicode.Is(unicode.Katakana, current) ||
			unicode.Is(unicode.Hangul, current) {
			tokens++
		} else {
			tokens += maxInt(1, utf8.RuneLen(current)/2)
		}
	}
	if asciiRunes > 0 {
		tokens += (asciiRunes + 3) / 4
	}
	return tokens
}

func truncateToTokenBudget(value string, budget int) string {
	if estimateTokens(value) <= budget {
		return value
	}
	runes := []rune(value)
	left, right := 0, len(runes)
	for left < right {
		middle := (left + right + 1) / 2
		if estimateTokens(string(runes[:middle])) <= budget {
			left = middle
		} else {
			right = middle - 1
		}
	}
	return string(runes[:left])
}

func (s *Service) summarizeConversation(
	parent context.Context,
	_ string,
	owner string,
	conversationID string,
	apiKey string,
	provider ModelProvider,
) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	conversation, candidates, err := s.conversations.LoadSummaryCandidates(
		ctx, owner, conversationID, s.config.RecentMessagePairs*2, 50,
	)
	if err != nil || len(candidates) == 0 {
		return
	}
	candidateMessages := make([]Message, 0, len(candidates))
	for _, item := range candidates {
		candidateMessages = append(candidateMessages, Message{
			Role: item.Role, Content: item.Content,
		})
	}
	if estimateTokens(conversation.Summary)+estimateMessagesTokens(candidateMessages) <
		s.config.SummaryTriggerTokens {
		return
	}

	var transcript strings.Builder
	if conversation.Summary != "" {
		transcript.WriteString("已有摘要：\n")
		transcript.WriteString(truncateRunes(conversation.Summary, 2000))
		transcript.WriteString("\n\n新增对话：\n")
	}
	includedThroughID := int64(0)
	inputTokenLimit := maxInt(s.config.InputTokenBudget-256, 1000)
	for _, item := range candidates {
		role := "用户"
		if item.Role == "assistant" {
			role = "助手"
		}
		line := role + "：" + truncateRunes(item.Content, 4000) + "\n"
		if estimateTokens(transcript.String())+estimateTokens(line) > inputTokenLimit {
			break
		}
		transcript.WriteString(line)
		includedThroughID = item.InternalID
	}
	if includedThroughID == 0 {
		return
	}
	summary, err := provider.CompleteChat(ctx, apiKey, ChatRequest{
		ModelAlias:      ModelFreeGeneral,
		MaxOutputTokens: s.config.SummaryMaxOutputTokens,
		Messages: []Message{
			{
				Role:    "system",
				Content: "你负责压缩投研助手历史对话。保留用户目标、约束、关键事实、数字、结论和未解决问题；删除寒暄与重复内容。只输出简洁中文摘要，不添加事实。",
			},
			{Role: "user", Content: transcript.String()},
		},
	})
	if err != nil {
		return
	}
	summary = truncateRunes(strings.TrimSpace(summary), s.config.SummaryMaxChars)
	if summary == "" {
		return
	}
	_, _ = s.conversations.UpdateSummary(
		ctx, owner, conversationID, conversation.SummaryVersion,
		summary, includedThroughID,
	)
}
