package ai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestConversationRepositoryPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("ai_conversation_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob", "charlie"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO accounts(username, password_hash) VALUES($1, 'test')`,
			owner,
		); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewConversationRepository(pool, 7*24*time.Hour, time.Minute, 5, 1000)
	var waitGroup sync.WaitGroup
	var resultMutex sync.Mutex
	concurrentCreated := 0
	concurrentUnexpected := []error{}
	for index := 0; index < 10; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, createErr := repository.CreateConversation(
				ctx, "charlie", ModelFreeGeneral,
			)
			resultMutex.Lock()
			defer resultMutex.Unlock()
			switch {
			case createErr == nil:
				concurrentCreated++
			case !errors.Is(createErr, ErrConversationLimit):
				concurrentUnexpected = append(concurrentUnexpected, createErr)
			}
		}()
	}
	waitGroup.Wait()
	if concurrentCreated != 5 || len(concurrentUnexpected) != 0 {
		t.Fatalf(
			"concurrent created=%d unexpected=%v",
			concurrentCreated, concurrentUnexpected,
		)
	}
	first, err := repository.CreateConversation(ctx, "alice", ModelFreeGeneral)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repository.CreateConversation(ctx, "alice", ModelFreeGeneral)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE ai_conversations SET last_message_at=now()-interval '1 minute' WHERE id=$1`,
		first.ID,
	); err != nil {
		t.Fatal(err)
	}
	page, cursor, err := repository.ListConversations(ctx, "alice", 1, "")
	if err != nil || len(page) != 1 || page[0].ID != second.ID || cursor == "" {
		t.Fatalf("first page=%+v cursor=%q err=%v", page, cursor, err)
	}
	page, _, err = repository.ListConversations(ctx, "alice", 1, cursor)
	if err != nil || len(page) != 1 || page[0].ID != first.ID {
		t.Fatalf("second page=%+v err=%v", page, err)
	}
	var expiring Conversation
	for index := 0; index < 3; index++ {
		created, createErr := repository.CreateConversation(ctx, "alice", ModelFreeGeneral)
		if createErr != nil {
			t.Fatal(createErr)
		}
		expiring = created
	}
	if _, err := repository.CreateConversation(
		ctx, "alice", ModelFreeGeneral,
	); !errors.Is(err, ErrConversationLimit) {
		t.Fatalf("conversation limit err=%v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE ai_conversations SET expires_at=now()-interval '1 second' WHERE id=$1`,
		expiring.ID,
	); err != nil {
		t.Fatal(err)
	}
	replacement, err := repository.CreateConversation(ctx, "alice", ModelFreeGeneral)
	if err != nil {
		t.Fatalf("create after expiry: %v", err)
	}
	if err := repository.DeleteConversation(ctx, "alice", replacement.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateConversation(ctx, "alice", ModelFreeGeneral); err != nil {
		t.Fatalf("create after delete: %v", err)
	}

	clientMessageID := uuid.NewString()
	turn, err := repository.BeginTurn(
		ctx, "alice", first.ID, clientMessageID, "分析 BTC", ModelFreeGeneral,
	)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Conversation.Title != "分析 BTC" || turn.Conversation.MessageCount != 2 {
		t.Fatalf("turn=%+v", turn)
	}
	if _, err := repository.BeginTurn(
		ctx, "alice", first.ID, uuid.NewString(), "并发", ModelFreeGeneral,
	); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("concurrent err=%v", err)
	}
	if _, err := repository.FinishTurn(
		ctx, "alice", first.ID, turn.UserMessage.TurnID,
		"completed", "结论", "funding", 12, 3,
	); err != nil {
		t.Fatal(err)
	}
	duplicate, err := repository.BeginTurn(
		ctx, "alice", first.ID, clientMessageID, "分析 BTC", ModelFreeGeneral,
	)
	if err != nil || !duplicate.Duplicate || duplicate.AssistantMessage.Content != "结论" {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	messages, _, err := repository.ListMessages(ctx, "alice", first.ID, 30, "")
	if err != nil || len(messages) != 2 || messages[0].Role != "assistant" {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if _, _, err := repository.ListMessages(ctx, "bob", first.ID, 30, ""); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("owner isolation err=%v", err)
	}
	updated, err := repository.UpdateSummary(
		ctx, "alice", first.ID, 0, "摘要", turn.AssistantMessage.InternalID,
	)
	if err != nil || !updated {
		t.Fatalf("summary updated=%v err=%v", updated, err)
	}
	var summarizedMessageCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ai_messages WHERE conversation_id=$1`, first.ID,
	).Scan(&summarizedMessageCount); err != nil || summarizedMessageCount != 0 {
		t.Fatalf("summarized message count=%d err=%v", summarizedMessageCount, err)
	}
	updated, err = repository.UpdateSummary(
		ctx, "alice", first.ID, 0, "stale", turn.AssistantMessage.InternalID,
	)
	if err != nil || updated {
		t.Fatalf("stale summary updated=%v err=%v", updated, err)
	}

	bobConversation, err := repository.CreateConversation(ctx, "bob", ModelFreeGeneral)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ai_messages (
			conversation_id, turn_id, role, status, content
		)
		SELECT $1::uuid, gen_random_uuid(), 'user', 'completed', 'history'
		FROM generate_series(1, 998)`,
		bobConversation.ID,
	); err != nil {
		t.Fatal(err)
	}
	bobTurn, err := repository.BeginTurn(
		ctx, "bob", bobConversation.ID, uuid.NewString(), "第 1000 条", ModelFreeGeneral,
	)
	if err != nil {
		t.Fatalf("complete 1000 messages: %v", err)
	}
	if _, err := repository.FinishTurn(
		ctx, "bob", bobConversation.ID, bobTurn.UserMessage.TurnID,
		"completed", "回答", "funding", 1, 1,
	); err != nil {
		t.Fatal(err)
	}
	updated, err = repository.UpdateSummary(
		ctx, "bob", bobConversation.ID, 99, "stale", bobTurn.AssistantMessage.InternalID,
	)
	if err != nil || updated {
		t.Fatalf("unexpected stale summary update=%v err=%v", updated, err)
	}
	var bobMessageCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ai_messages WHERE conversation_id=$1`,
		bobConversation.ID,
	).Scan(&bobMessageCount); err != nil || bobMessageCount != 1000 {
		t.Fatalf("messages pruned after stale summary=%d err=%v", bobMessageCount, err)
	}
	if _, err := repository.BeginTurn(
		ctx, "bob", bobConversation.ID, uuid.NewString(), "超限", ModelFreeGeneral,
	); !errors.Is(err, ErrRawMessageLimit) {
		t.Fatalf("raw message limit err=%v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE ai_conversations SET expires_at=now()-interval '1 second' WHERE id=$1`,
		first.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetConversation(ctx, "alice", first.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("expired get err=%v", err)
	}
	deleted, err := repository.DeleteExpired(ctx, 500)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	var messageCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM ai_messages WHERE conversation_id=$1`, first.ID,
	).Scan(&messageCount); err != nil || messageCount != 0 {
		t.Fatalf("message count=%d err=%v", messageCount, err)
	}
}
