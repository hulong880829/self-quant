WITH ranked AS (
    SELECT
        id,
        row_number() OVER (
            PARTITION BY owner_username
            ORDER BY last_message_at DESC, id DESC
        ) AS position
    FROM ai_conversations
    WHERE expires_at > now()
)
UPDATE ai_conversations AS conversation
SET expires_at = now(),
    updated_at = now()
FROM ranked
WHERE conversation.id = ranked.id
  AND ranked.position > 5;
