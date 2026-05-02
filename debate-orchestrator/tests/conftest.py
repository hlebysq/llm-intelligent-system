"""
Глобальная конфигурация pytest для тестов debate-orchestrator.

Переменные окружения выставляются ДО любого импорта кода приложения,
чтобы модульные переменные (например, _DEBUG_BACKEND) получили нужные значения.
"""
import os

os.environ.setdefault("YANDEX_API_KEY", "test-api-key-for-unit-tests")

os.environ.setdefault("DEBUG_BACKEND", "true")

os.environ.setdefault("DEBATE_TIMEOUT_SECONDS", "30")
