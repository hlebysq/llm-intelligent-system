.PHONY: help init deps build up down logs clean test migrate

help: ## Показать эту справку
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

init: ## Инициализация проекта
	@echo "Initializing project..."
	go mod download
	go mod tidy
	cp .env.example .env
	@echo "Done! Please edit .env file with your credentials"

deps: ## Установка зависимостей
	go mod download
	go mod tidy

build: ## Сборка всех сервисов
	docker-compose build

up: ## Запуск всех сервисов
	docker-compose up -d

down: ## Остановка всех сервисов
	docker-compose down

logs: ## Показать логи всех сервисов
	docker-compose logs -f

logs-api: ## Показать логи API Gateway
	docker-compose logs -f api-gateway

logs-proxy: ## Показать логи Model Proxy
	docker-compose logs -f model-proxy

logs-bot: ## Показать логи Telegram Bot
	docker-compose logs -f telegram-bot

clean: ## Очистка (удаление контейнеров и volumes)
	docker-compose down -v
	rm -rf vendor

restart: down up ## Перезапуск всех сервисов

ollama-pull: ## Скачать модель llama3.2 в Ollama
	docker exec llm-ollama ollama pull llama3.2

test: ## Запуск тестов
	go test -v ./...

dev-api: ## Запуск API Gateway локально (для разработки)
	cd cmd/api-gateway && go run main.go

dev-proxy: ## Запуск Model Proxy локально (для разработки)
	cd cmd/model-proxy && go run main.go

dev-bot: ## Запуск Telegram Bot локально (для разработки)
	cd cmd/telegram-bot && go run main.go
