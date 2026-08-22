package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrConversationNotFound = errors.New("ai conversation not found")
	ErrConversationBusy     = errors.New("ai conversation already streaming")
	ErrConversationLimit    = errors.New("ai conversation limit reached")
	ErrRawMessageLimit      = errors.New("ai conversation raw message limit reached")
)

type Conversation struct {
	ID                 string
	OwnerUsername      string
	Title              string
	LastMessagePreview string
	MessageCount       int
	ModelAlias         string
	Summary            string
	SummaryThroughID   int64
	SummaryVersion     int
	LastMessageAt      time.Time
	ExpiresAt          time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type ConversationMessage struct {
	InternalID      int64
	ID              string
	ConversationID  string
	TurnID          string
	ClientMessageID string
	Role            string
	Status          string
	Content         string
	Tool            string
	InputTokens     int
	OutputTokens    int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type BeginTurnResult struct {
	Conversation     Conversation
	UserMessage      ConversationMessage
	AssistantMessage ConversationMessage
	Duplicate        bool
}

type ConversationRepository struct {
	pool             *pgxpool.Pool
	retention        time.Duration
	lease            time.Duration
	maxConversations int
	maxRawMessages   int
}

func NewConversationRepository(
	pool *pgxpool.Pool,
	retention time.Duration,
	lease time.Duration,
	maxConversations int,
	maxRawMessages int,
) *ConversationRepository {
	return &ConversationRepository{
		pool: pool, retention: retention, lease: lease,
		maxConversations: maxConversations, maxRawMessages: maxRawMessages,
	}
}

func (r *ConversationRepository) CreateConversation(
	ctx context.Context,
	owner string,
	modelAlias string,
) (Conversation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Conversation{}, fmt.Errorf("begin create ai conversation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtext('ai-conversations:' || $1))`,
		owner,
	); err != nil {
		return Conversation{}, fmt.Errorf("lock ai conversation owner: %w", err)
	}
	var activeCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM ai_conversations
		WHERE owner_username=$1 AND expires_at > now()`,
		owner,
	).Scan(&activeCount); err != nil {
		return Conversation{}, fmt.Errorf("count active ai conversations: %w", err)
	}
	if activeCount >= r.maxConversations {
		return Conversation{}, ErrConversationLimit
	}
	conversation, err := scanConversation(tx.QueryRow(ctx, `
		INSERT INTO ai_conversations (owner_username, model_alias, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		RETURNING `+conversationReturnColumns,
		owner, modelAlias, durationInterval(r.retention),
	))
	if err != nil {
		return Conversation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversation{}, fmt.Errorf("commit ai conversation: %w", err)
	}
	return conversation, nil
}

func (r *ConversationRepository) ListConversations(
	ctx context.Context,
	owner string,
	limit int,
	cursor string,
) ([]Conversation, string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+conversationColumns+`
		FROM ai_conversations c
		WHERE c.owner_username=$1
		  AND c.expires_at > now()
		  AND (
		    $2 = '' OR
		    (c.last_message_at, c.id) < (
		      SELECT c2.last_message_at, c2.id
		      FROM ai_conversations c2
		      WHERE c2.id=NULLIF($2, '')::uuid AND c2.owner_username=$1
		    )
		  )
		ORDER BY c.last_message_at DESC, c.id DESC
		LIMIT $3`,
		owner, cursor, limit+1,
	)
	if err != nil {
		return nil, "", fmt.Errorf("list ai conversations: %w", err)
	}
	defer rows.Close()
	items := make([]Conversation, 0, limit+1)
	for rows.Next() {
		item, scanErr := scanConversation(rows)
		if scanErr != nil {
			return nil, "", scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate ai conversations: %w", err)
	}
	nextCursor := ""
	if len(items) > limit {
		nextCursor = items[limit-1].ID
		items = items[:limit]
	}
	return items, nextCursor, nil
}

func (r *ConversationRepository) GetConversation(
	ctx context.Context,
	owner string,
	id string,
) (Conversation, error) {
	return scanConversation(r.pool.QueryRow(ctx, `
		SELECT `+conversationColumns+`
		FROM ai_conversations c
		WHERE c.id=$2::uuid AND c.owner_username=$1 AND c.expires_at > now()`,
		owner, id,
	))
}

func (r *ConversationRepository) RenameConversation(
	ctx context.Context,
	owner string,
	id string,
	title string,
) (Conversation, error) {
	return scanConversation(r.pool.QueryRow(ctx, `
		UPDATE ai_conversations
		SET title=$3, updated_at=now()
		WHERE id=$2::uuid AND owner_username=$1 AND expires_at > now()
		RETURNING `+conversationReturnColumns,
		owner, id, title,
	))
}

func (r *ConversationRepository) DeleteConversation(
	ctx context.Context,
	owner string,
	id string,
) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM ai_conversations
		WHERE id=$2::uuid AND owner_username=$1 AND expires_at > now()`, owner, id)
	if err != nil {
		return fmt.Errorf("delete ai conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

func (r *ConversationRepository) ListMessages(
	ctx context.Context,
	owner string,
	conversationID string,
	limit int,
	cursor string,
) ([]ConversationMessage, string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+messageColumns+`
		FROM ai_messages m
		JOIN ai_conversations c ON c.id=m.conversation_id
		WHERE c.owner_username=$1
		  AND c.id=$2::uuid
		  AND c.expires_at > now()
		  AND ($3 = '' OR m.id < (
		    SELECT m2.id FROM ai_messages m2
		    WHERE m2.public_id=NULLIF($3, '')::uuid AND m2.conversation_id=c.id
		  ))
		ORDER BY m.id DESC
		LIMIT $4`,
		owner, conversationID, cursor, limit+1,
	)
	if err != nil {
		return nil, "", fmt.Errorf("list ai messages: %w", err)
	}
	defer rows.Close()
	items := make([]ConversationMessage, 0, limit+1)
	for rows.Next() {
		item, scanErr := scanConversationMessage(rows)
		if scanErr != nil {
			return nil, "", scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate ai messages: %w", err)
	}
	if len(items) == 0 {
		if _, err := r.GetConversation(ctx, owner, conversationID); err != nil {
			return nil, "", err
		}
	}
	nextCursor := ""
	if len(items) > limit {
		nextCursor = items[limit-1].ID
		items = items[:limit]
	}
	return items, nextCursor, nil
}

func (r *ConversationRepository) BeginTurn(
	ctx context.Context,
	owner string,
	conversationID string,
	clientMessageID string,
	prompt string,
	modelAlias string,
) (BeginTurnResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return BeginTurnResult{}, fmt.Errorf("begin ai turn: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if duplicate, found, duplicateErr := findDuplicateTurn(
		ctx, tx, owner, conversationID, clientMessageID,
	); duplicateErr != nil {
		return BeginTurnResult{}, duplicateErr
	} else if found {
		if duplicate.AssistantMessage.Status == "completed" {
			duplicate.Duplicate = true
			return duplicate, tx.Commit(ctx)
		}
		leaseTag, leaseErr := tx.Exec(ctx, `
			UPDATE ai_conversations
			SET active_turn_id=$3::uuid,
			    active_turn_expires_at=now() + $4::interval,
			    updated_at=now()
			WHERE id=$2::uuid
			  AND owner_username=$1
			  AND expires_at > now()
			  AND (active_turn_id IS NULL OR active_turn_expires_at < now())`,
			owner, conversationID, duplicate.UserMessage.TurnID, durationInterval(r.lease),
		)
		if leaseErr != nil {
			return BeginTurnResult{}, fmt.Errorf("resume ai turn lease: %w", leaseErr)
		}
		if leaseTag.RowsAffected() == 0 {
			return BeginTurnResult{}, ErrConversationBusy
		}
		assistant, resetErr := scanConversationMessage(tx.QueryRow(ctx, `
			UPDATE ai_messages
			SET status='pending', content='', tool='',
			    input_tokens=NULL, output_tokens=NULL, updated_at=now()
			WHERE conversation_id=$1::uuid AND turn_id=$2::uuid AND role='assistant'
			RETURNING `+messageReturnColumns,
			conversationID, duplicate.UserMessage.TurnID,
		))
		if resetErr != nil {
			return BeginTurnResult{}, resetErr
		}
		duplicate.AssistantMessage = assistant
		return duplicate, tx.Commit(ctx)
	}

	turnID := uuid.NewString()
	leaseTag, err := tx.Exec(ctx, `
		UPDATE ai_conversations
		SET active_turn_id=$3::uuid,
		    active_turn_expires_at=now() + $4::interval,
		    updated_at=now()
		WHERE id=$2::uuid
		  AND owner_username=$1
		  AND expires_at > now()
		  AND (active_turn_id IS NULL OR active_turn_expires_at < now())`,
		owner, conversationID, turnID, durationInterval(r.lease),
	)
	if err != nil {
		return BeginTurnResult{}, fmt.Errorf("lease ai conversation: %w", err)
	}
	if leaseTag.RowsAffected() == 0 {
		if _, getErr := r.getConversationTx(ctx, tx, owner, conversationID); getErr != nil {
			return BeginTurnResult{}, getErr
		}
		return BeginTurnResult{}, ErrConversationBusy
	}
	var rawMessageCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM ai_messages WHERE conversation_id=$1::uuid`,
		conversationID,
	).Scan(&rawMessageCount); err != nil {
		return BeginTurnResult{}, fmt.Errorf("count ai conversation messages: %w", err)
	}
	if rawMessageCount+2 > r.maxRawMessages {
		return BeginTurnResult{}, ErrRawMessageLimit
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ai_messages
		SET status='stopped', updated_at=now()
		WHERE conversation_id=$1::uuid AND role='assistant' AND status='pending'`,
		conversationID,
	); err != nil {
		return BeginTurnResult{}, fmt.Errorf("stop abandoned ai turn: %w", err)
	}

	userMessage, err := scanConversationMessage(tx.QueryRow(ctx, `
		INSERT INTO ai_messages (
			conversation_id, turn_id, client_message_id, role, status, content
		) VALUES ($1::uuid, $2::uuid, $3::uuid, 'user', 'completed', $4)
		RETURNING `+messageReturnColumns,
		conversationID, turnID, clientMessageID, prompt,
	))
	if err != nil {
		return BeginTurnResult{}, err
	}
	assistantMessage, err := scanConversationMessage(tx.QueryRow(ctx, `
		INSERT INTO ai_messages (
			conversation_id, turn_id, role, status
		) VALUES ($1::uuid, $2::uuid, 'assistant', 'pending')
		RETURNING `+messageReturnColumns,
		conversationID, turnID,
	))
	if err != nil {
		return BeginTurnResult{}, err
	}
	title := conversationTitle(prompt)
	preview := conversationPreview(prompt)
	conversation, err := scanConversation(tx.QueryRow(ctx, `
		UPDATE ai_conversations
		SET title=CASE WHEN title='' THEN $3 ELSE title END,
		    last_message_preview=$4,
		    message_count=message_count+2,
		    model_alias=$5,
		    last_message_at=now(),
		    expires_at=now() + $6::interval,
		    updated_at=now()
		WHERE id=$2::uuid AND owner_username=$1
		RETURNING `+conversationReturnColumns,
		owner, conversationID, title, preview, modelAlias, durationInterval(r.retention),
	))
	if err != nil {
		return BeginTurnResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BeginTurnResult{}, fmt.Errorf("commit ai turn: %w", err)
	}
	return BeginTurnResult{
		Conversation: conversation, UserMessage: userMessage,
		AssistantMessage: assistantMessage,
	}, nil
}

func (r *ConversationRepository) FinishTurn(
	ctx context.Context,
	owner string,
	conversationID string,
	turnID string,
	status string,
	content string,
	tool string,
	inputTokens int,
	outputTokens int,
) (ConversationMessage, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ConversationMessage{}, fmt.Errorf("begin finish ai turn: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	message, err := scanConversationMessage(tx.QueryRow(ctx, `
		UPDATE ai_messages m
		SET status=$4,
		    content=$5,
		    tool=$6,
		    input_tokens=NULLIF($7, 0),
		    output_tokens=NULLIF($8, 0),
		    updated_at=now()
		FROM ai_conversations c
		WHERE m.conversation_id=c.id
		  AND c.owner_username=$1
		  AND c.id=$2::uuid
		  AND m.turn_id=$3::uuid
		  AND m.role='assistant'
		RETURNING `+messageColumns,
		owner, conversationID, turnID, status, content, tool, inputTokens, outputTokens,
	))
	if err != nil {
		return ConversationMessage{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE ai_conversations
		SET active_turn_id=NULL,
		    active_turn_expires_at=NULL,
		    updated_at=now()
		WHERE id=$2::uuid AND owner_username=$1 AND active_turn_id=$3::uuid`,
		owner, conversationID, turnID,
	)
	if err != nil {
		return ConversationMessage{}, fmt.Errorf("release ai conversation lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ConversationMessage{}, ErrConversationNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return ConversationMessage{}, fmt.Errorf("commit ai turn result: %w", err)
	}
	return message, nil
}

func (r *ConversationRepository) LoadContext(
	ctx context.Context,
	owner string,
	conversationID string,
	limit int,
) (Conversation, []ConversationMessage, error) {
	conversation, err := r.GetConversation(ctx, owner, conversationID)
	if err != nil {
		return Conversation{}, nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+messageColumns+`
		FROM ai_messages m
		WHERE m.conversation_id=$1::uuid
		  AND m.status IN ('completed', 'stopped')
		ORDER BY m.id DESC
		LIMIT $2`,
		conversationID, limit,
	)
	if err != nil {
		return Conversation{}, nil, fmt.Errorf("load ai context messages: %w", err)
	}
	defer rows.Close()
	messages := make([]ConversationMessage, 0, limit)
	for rows.Next() {
		item, scanErr := scanConversationMessage(rows)
		if scanErr != nil {
			return Conversation{}, nil, scanErr
		}
		messages = append(messages, item)
	}
	if err := rows.Err(); err != nil {
		return Conversation{}, nil, fmt.Errorf("iterate ai context messages: %w", err)
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return conversation, messages, nil
}

func (r *ConversationRepository) LoadSummaryCandidates(
	ctx context.Context,
	owner string,
	conversationID string,
	recentMessages int,
	limit int,
) (Conversation, []ConversationMessage, error) {
	conversation, err := r.GetConversation(ctx, owner, conversationID)
	if err != nil {
		return Conversation{}, nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+messageColumns+`
		FROM ai_messages m
		WHERE m.conversation_id=$1::uuid
		  AND m.status='completed'
		  AND m.id > $2
		  AND m.id < COALESCE((
		    SELECT recent.id
		    FROM ai_messages recent
		    WHERE recent.conversation_id=$1::uuid AND recent.status='completed'
		    ORDER BY recent.id DESC
		    OFFSET GREATEST($3-1, 0) LIMIT 1
		  ), 9223372036854775807)
		ORDER BY m.id ASC
		LIMIT $4`,
		conversationID, conversation.SummaryThroughID, recentMessages, limit,
	)
	if err != nil {
		return Conversation{}, nil, fmt.Errorf("load ai summary candidates: %w", err)
	}
	defer rows.Close()
	messages := make([]ConversationMessage, 0, limit)
	for rows.Next() {
		item, scanErr := scanConversationMessage(rows)
		if scanErr != nil {
			return Conversation{}, nil, scanErr
		}
		messages = append(messages, item)
	}
	if err := rows.Err(); err != nil {
		return Conversation{}, nil, fmt.Errorf("iterate ai summary candidates: %w", err)
	}
	return conversation, messages, nil
}

func (r *ConversationRepository) UpdateSummary(
	ctx context.Context,
	owner string,
	conversationID string,
	expectedVersion int,
	summary string,
	throughID int64,
) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin ai conversation summary update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE ai_conversations
		SET summary=$4,
		    summary_through_id=$5,
		    summary_version=summary_version+1,
		    updated_at=now()
		WHERE id=$2::uuid
		  AND owner_username=$1
		  AND summary_version=$3
		  AND expires_at > now()`,
		owner, conversationID, expectedVersion, summary, throughID,
	)
	if err != nil {
		return false, fmt.Errorf("update ai conversation summary: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM ai_messages
		WHERE conversation_id=$1::uuid
		  AND id <= $2
		  AND status='completed'`,
		conversationID, throughID,
	); err != nil {
		return false, fmt.Errorf("prune summarized ai messages: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit ai conversation summary update: %w", err)
	}
	return true, nil
}

func (r *ConversationRepository) DeleteExpired(
	ctx context.Context,
	limit int,
) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM ai_conversations
		WHERE ctid IN (
			SELECT ctid FROM ai_conversations
			WHERE expires_at < now()
			ORDER BY expires_at ASC
			LIMIT $1
		)`, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired ai conversations: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *ConversationRepository) getConversationTx(
	ctx context.Context,
	tx pgx.Tx,
	owner string,
	id string,
) (Conversation, error) {
	return scanConversation(tx.QueryRow(ctx, `
		SELECT `+conversationColumns+`
		FROM ai_conversations c
		WHERE c.id=$2::uuid AND c.owner_username=$1 AND c.expires_at > now()`,
		owner, id,
	))
}

func findDuplicateTurn(
	ctx context.Context,
	tx pgx.Tx,
	owner string,
	conversationID string,
	clientMessageID string,
) (BeginTurnResult, bool, error) {
	user, err := scanConversationMessage(tx.QueryRow(ctx, `
		SELECT `+messageColumns+`
		FROM ai_messages m
		JOIN ai_conversations c ON c.id=m.conversation_id
		WHERE c.owner_username=$1
		  AND c.id=$2::uuid
		  AND c.expires_at > now()
		  AND m.client_message_id=$3::uuid`,
		owner, conversationID, clientMessageID,
	))
	if errors.Is(err, ErrConversationNotFound) {
		return BeginTurnResult{}, false, nil
	}
	if err != nil {
		return BeginTurnResult{}, false, err
	}
	assistant, err := scanConversationMessage(tx.QueryRow(ctx, `
		SELECT `+messageColumns+`
		FROM ai_messages m
		WHERE conversation_id=$1::uuid AND turn_id=$2::uuid AND role='assistant'`,
		conversationID, user.TurnID,
	))
	if err != nil {
		return BeginTurnResult{}, false, err
	}
	conversation, err := scanConversation(tx.QueryRow(ctx, `
		SELECT `+conversationColumns+`
		FROM ai_conversations c WHERE c.id=$1::uuid AND c.owner_username=$2`,
		conversationID, owner,
	))
	if err != nil {
		return BeginTurnResult{}, false, err
	}
	return BeginTurnResult{
		Conversation: conversation, UserMessage: user, AssistantMessage: assistant,
	}, true, nil
}

const conversationColumns = `
	c.id::text, c.owner_username, c.title, c.last_message_preview,
	c.message_count, c.model_alias, c.summary, c.summary_through_id,
	c.summary_version, c.last_message_at, c.expires_at, c.created_at, c.updated_at`

const conversationReturnColumns = `
	id::text, owner_username, title, last_message_preview,
	message_count, model_alias, summary, summary_through_id,
	summary_version, last_message_at, expires_at, created_at, updated_at`

const messageColumns = `
	m.id, m.public_id::text, m.conversation_id::text, m.turn_id::text,
	m.client_message_id::text, m.role, m.status, m.content, m.tool,
	COALESCE(m.input_tokens, 0), COALESCE(m.output_tokens, 0),
	m.created_at, m.updated_at`

const messageReturnColumns = `
	id, public_id::text, conversation_id::text, turn_id::text,
	client_message_id::text, role, status, content, tool,
	COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	created_at, updated_at`

type rowScanner interface {
	Scan(...any) error
}

func scanConversation(row rowScanner) (Conversation, error) {
	var item Conversation
	var summaryThroughID *int64
	err := row.Scan(
		&item.ID, &item.OwnerUsername, &item.Title, &item.LastMessagePreview,
		&item.MessageCount, &item.ModelAlias, &item.Summary, &summaryThroughID,
		&item.SummaryVersion, &item.LastMessageAt, &item.ExpiresAt,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, ErrConversationNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("scan ai conversation: %w", err)
	}
	if summaryThroughID != nil {
		item.SummaryThroughID = *summaryThroughID
	}
	return item, nil
}

func scanConversationMessage(row rowScanner) (ConversationMessage, error) {
	var item ConversationMessage
	var clientMessageID *string
	err := row.Scan(
		&item.InternalID, &item.ID, &item.ConversationID, &item.TurnID,
		&clientMessageID, &item.Role, &item.Status, &item.Content, &item.Tool,
		&item.InputTokens, &item.OutputTokens, &item.CreatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConversationMessage{}, ErrConversationNotFound
	}
	if err != nil {
		return ConversationMessage{}, fmt.Errorf("scan ai message: %w", err)
	}
	if clientMessageID != nil {
		item.ClientMessageID = *clientMessageID
	}
	return item, nil
}

func durationInterval(value time.Duration) string {
	return fmt.Sprintf("%f seconds", value.Seconds())
}

func conversationTitle(prompt string) string {
	return truncateRunes(strings.TrimSpace(prompt), 32)
}

func conversationPreview(prompt string) string {
	return truncateRunes(strings.TrimSpace(prompt), 80)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
