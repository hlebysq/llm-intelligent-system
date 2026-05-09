.PHONY: help init deps build up down logs clean test migrate

help: ## Показать эту справку
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

init: ## Инициализация проекта
	@echo "Initializing project..."
	go mod download
	go mod tidy
	cp .env.example .env
	@echo "Done! Please edit .env file with your credentials"

build: ## Сборка всех сервисов
	docker-compose build

up: ## Запуск всех сервисов
	docker-compose up -d

down: ## Остановка всех сервисов
	docker-compose down

logs: ## Показать логи всех сервисов
	docker-compose logs -f

clean: ## Очистка (удаление контейнеров и volumes)
	docker-compose down -v
	rm -rf vendor

restart: down up ## Перезапуск всех сервисов

test: ## Запуск тестов
	go test -v ./...

migrate: ## Apply pending database migrations
	docker-compose run --rm migrate
