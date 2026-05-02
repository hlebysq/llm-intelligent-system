import logging
from typing import Any, Dict

import httpx
from openai import AsyncOpenAI

from .base import BaseDebateAgent

logger = logging.getLogger(__name__)

_YANDEX_BASE_URL = "https://llm.api.cloud.yandex.net/v1"

# Модели с thinking-режимом и способ его отключения.
# Формат: model_name -> extra_body dict для отключения рассуждений.
_DISABLE_THINKING: Dict[str, Dict[str, Any]] = {
    # Qwen3: отключается через enable_thinking
    "qwen3-235b-a22b-fp8": {"enable_thinking": False},
    "qwen3-235b-a22b":     {"enable_thinking": False},
    # DeepSeek V3.2: reasoning включён по умолчанию, отключается через reasoning_effort
    "deepseek-v3.2":       {"reasoning_effort": "none"},
    "deepseek-v3":         {"reasoning_effort": "none"},
}


class YandexAgent(BaseDebateAgent):

    def __init__(
        self,
        agent_id: str,
        model_name: str,
        api_key: str,
        max_tokens: int = 1024,
        request_timeout: float = 120.0,
    ):
        super().__init__(agent_id, model_name)
        self._client = AsyncOpenAI(
            base_url=_YANDEX_BASE_URL,
            api_key=api_key,
            http_client=httpx.AsyncClient(timeout=request_timeout),
        )
        self._max_tokens = max_tokens
        # Автоматически отключаем thinking для моделей из таблицы выше
        self._extra_body: Dict[str, Any] = _DISABLE_THINKING.get(model_name, {})

    async def answer(self, prompt: str) -> str:
        response = await self._client.chat.completions.create(
            model=self.model_name,
            messages=[{"role": "user", "content": prompt}],
            max_tokens=self._max_tokens,
            extra_body=self._extra_body or None,
        )
        choice = response.choices[0]
        content = choice.message.content
        if content is None:
            logger.warning(
                "Model %s returned None content (finish_reason=%s)",
                self.model_name,
                choice.finish_reason,
            )
            return ""
        return content
