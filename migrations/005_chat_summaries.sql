-- Stores compact per-user conversation memory used together with recent messages.
CREATE TABLE IF NOT EXISTS chat_summaries (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    summary TEXT NOT NULL DEFAULT '',
    message_count INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
