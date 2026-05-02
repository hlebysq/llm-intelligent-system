-- Сохраняем выбранный режим обработки запросов для каждого пользователя
ALTER TABLE users ADD COLUMN IF NOT EXISTS preferred_mode VARCHAR(20) DEFAULT 'analytics';
