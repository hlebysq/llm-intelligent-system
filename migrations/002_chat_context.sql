-- Добавляем telegram_id к пользователям (nullable — у testuser его нет)
ALTER TABLE users ADD COLUMN IF NOT EXISTS telegram_id BIGINT UNIQUE;

-- Таблица истории чата
CREATE TABLE IF NOT EXISTS chat_messages (
    id          UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        VARCHAR(10) NOT NULL CHECK (role IN ('user', 'assistant')),
    content     TEXT        NOT NULL,
    created_at  TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_user_id_created
    ON chat_messages(user_id, created_at DESC);

-- Сервисный аккаунт для Telegram-бота (пароль тот же: "password123")
INSERT INTO users (username, email, password_hash)
VALUES (
    'telegram_bot',
    'bot@internal.local',
    '$2a$10$t.xd7zn2hWpx93DMNl6G8ep2zNuP0j.u64Z3t7YhVEbJioO2121DS'
) ON CONFLICT (username) DO NOTHING;
