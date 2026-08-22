package ai

import "testing"

func TestEstimateTokensTreatsCJKConservatively(t *testing.T) {
	if got := estimateTokens("资金费率"); got != 4 {
		t.Fatalf("CJK tokens=%d", got)
	}
	if got := estimateTokens("abcdefgh"); got != 2 {
		t.Fatalf("ASCII tokens=%d", got)
	}
}

func TestAssembleProviderMessagesKeepsCompleteRecentPairs(t *testing.T) {
	stored := []ConversationMessage{
		{TurnID: "old", Role: "user", Content: "old question"},
		{TurnID: "old", Role: "assistant", Content: "old answer"},
		{TurnID: "recent", Role: "user", Content: "recent question"},
		{TurnID: "recent", Role: "assistant", Content: "recent answer"},
		{TurnID: "current", Role: "user", Content: "now"},
	}
	messages := assembleProviderMessages(
		"funding", "summary", stored, "current", "now", 100, 1,
	)
	if len(messages) != 5 ||
		messages[0].Role != "system" ||
		messages[1].Content != "较早对话摘要：\nsummary" ||
		messages[2].Content != "recent question" ||
		messages[3].Content != "recent answer" ||
		messages[4].Content != "now" {
		t.Fatalf("messages=%+v", messages)
	}
}

func TestAssembleProviderMessagesNeverSplitsPairWhenBudgetIsSmall(t *testing.T) {
	stored := []ConversationMessage{
		{TurnID: "pair", Role: "user", Content: "a very long historical question"},
		{TurnID: "pair", Role: "assistant", Content: "a very long historical answer"},
	}
	messages := assembleProviderMessages(
		"funding", "", stored, "current", "now", 20, 3,
	)
	if len(messages) != 2 || messages[1].Content != "now" {
		t.Fatalf("messages=%+v", messages)
	}
}
