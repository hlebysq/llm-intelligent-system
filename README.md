# LLM Intelligent System

Локальный стенд для Telegram-бота и API Gateway, который отправляет запросы в debate orchestrator, хранит историю диалога в PostgreSQL, использует Redis для rate limiting и отдает Swagger UI.

Система пока не задеплоена, но ближе к защите обязательно будет возможность протестировать бота без локального запуска.

## Состав проекта

- `api-gateway` - Go HTTP API, авторизация, Swagger, история, rate limiting, контекст диалога.
- `telegram-bot` - Go Telegram bot, который ходит в API Gateway.
- `debate-orchestrator` - Python/FastAPI сервис, который вызывает LLM-модели.
- `postgres` - хранит пользователей, логи запросов, сообщения и summary контекста.
- `redis` - используется для rate limiting.

## Требования

- Docker Desktop.
- Docker Compose v2 (`docker compose ...`).
- Go нужен только для локального запуска тестов без Docker.
- Yandex AI Studio API key для реальных LLM-вызовов.

## Первый запуск

1. Создать `.env`

2. Заполнить основные переменные в `.env`:

```env
DB_USER=llm_user
DB_PASSWORD=your_secure_password_here
DB_NAME=llm_system
JWT_SECRET=jwt_secret_key
API_GATEWAY_PORT=8085
YANDEX_API_KEY=your_yandex_api_key_here
TELEGRAM_BOT_TOKEN=your_telegram_bot_token
TELEGRAM_API_GATEWAY_URL=http://api-gateway:8080
```

3. Сборка и запуск сервисов:

```powershell
docker compose up -d --build
```

4. Проверка состояние контейнеров:

```powershell
docker compose ps
```

## Как открыть API и Swagger

Если в `.env` стоит `API_GATEWAY_PORT=8085`, то:

- Swagger UI: `http://localhost:8085/docs`
- OpenAPI YAML: `http://localhost:8085/openapi.yaml`
- Health check: `http://localhost:8085/api/v1/health`

Проверка через curl:

```powershell
curl -i http://localhost:8085/api/v1/health
curl -i http://localhost:8085/openapi.yaml
```

## Миграции

Папка `migrations` подключена в контейнер Postgres как `/docker-entrypoint-initdb.d`.

Postgres автоматически выполняет эти SQL-файлы только при первом создании пустого volume. Если база уже была создана раньше, новые миграции нужно применять вручную.

Прогнать конкретную новую миграцию:

```powershell
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -f /docker-entrypoint-initdb.d/005_chat_summaries.sql'
```

Проверить, что таблица появилась:

```powershell
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "\dt chat_summaries"'
```

Полностью пересоздать базу и применить все init SQL заново:

```powershell
docker compose down -v
docker compose up -d --build
```

Осторожно: `down -v` удаляет Docker volumes, то есть данные PostgreSQL и Redis.

## Rate limiting

Rate limiting настраивается в `.env`:

```env
QUERY_RATE_LIMIT_REQUESTS=20
QUERY_RATE_LIMIT_WINDOW_SECONDS=60
```

## Частые команды

Поднять проект:

```powershell
docker compose up -d
```

Пересобрать и поднять:

```powershell
docker compose up -d --build
```

Остановить:

```powershell
docker compose down
```

Остановить и удалить данные:

```powershell
docker compose down -v
```

Логи конкретного сервиса:

```powershell
docker compose logs -f api-gateway
docker compose logs -f telegram-bot
docker compose logs -f debate-orchestrator
docker compose logs -f postgres
docker compose logs -f redis
```

Запустить Go-тесты:

```powershell
go test ./...
```
