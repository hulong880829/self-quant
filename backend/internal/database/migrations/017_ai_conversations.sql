CREATE TABLE IF NOT EXISTS ai_conversations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT '',
    last_message_preview TEXT NOT NULL DEFAULT '',
    message_count INTEGER NOT NULL DEFAULT 0 CHECK (message_count >= 0),
    model_alias TEXT NOT NULL DEFAULT 'free-general',
    summary TEXT NOT NULL DEFAULT '',
    summary_through_id BIGINT,
    summary_version INTEGER NOT NULL DEFAULT 0 CHECK (summary_version >= 0),
    active_turn_id UUID,
    active_turn_expires_at TIMESTAMPTZ,
    last_message_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '7 days'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ai_conversations_owner_last_idx
    ON ai_conversations (owner_username, last_message_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS ai_conversations_expires_idx
    ON ai_conversations (expires_at);

CREATE TABLE IF NOT EXISTS ai_messages (
    id BIGSERIAL PRIMARY KEY,
    public_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    conversation_id UUID NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
    turn_id UUID NOT NULL,
    client_message_id UUID,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'stopped', 'failed')),
    content TEXT NOT NULL DEFAULT '',
    tool TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, turn_id, role)
);

CREATE UNIQUE INDEX IF NOT EXISTS ai_messages_client_message_idx
    ON ai_messages (conversation_id, client_message_id)
    WHERE client_message_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS ai_messages_conversation_id_idx
    ON ai_messages (conversation_id, id DESC);
