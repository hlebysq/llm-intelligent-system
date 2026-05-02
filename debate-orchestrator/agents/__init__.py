import os
from functools import lru_cache
from typing import List

from .base import BaseDebateAgent
from .yandex_agent import YandexAgent


def _require_key() -> str:
    key = os.getenv("YANDEX_API_KEY")
    if not key:
        raise RuntimeError("Переменная YANDEX_API_KEY не задана.")
    return key


@lru_cache(maxsize=1)
def get_debaters() -> List[BaseDebateAgent]:
    """
    Три агента-участника дебатов, каждый с разной моделью.

    По умолчанию:
      debater_a  — YandexGPT 5 Lite   (нативная Яндекс-модель, быстрый)
      debater_b  — DeepSeek V3.2      (reasoning отключён, сильная в анализе)
      debater_c  — YandexGPT 5 Lite   (второй нативный агент)

    Переопределяются через DEBATER_A/B/C_MODEL в .env.
    """
    key = _require_key()
    max_tokens = int(os.getenv("DEBATER_MAX_TOKENS", "1024"))

    debater_a = YandexAgent(
        agent_id="debater_a",
        model_name=os.getenv("DEBATER_A_MODEL", "yandexgpt-lite"),
        api_key=key,
        max_tokens=max_tokens,
    )
    debater_b = YandexAgent(
        agent_id="debater_b",
        model_name=os.getenv("DEBATER_B_MODEL", "deepseek-v3.2"),
        api_key=key,
        max_tokens=max_tokens,
    )
    debater_c = YandexAgent(
        agent_id="debater_c",
        model_name=os.getenv("DEBATER_C_MODEL", "yandexgpt-lite"),
        api_key=key,
        max_tokens=max_tokens,
    )

    return [debater_a, debater_b, debater_c]


@lru_cache(maxsize=1)
def get_judge() -> BaseDebateAgent:
    """
    Судья-синтезатор — YandexGPT 5.1 Pro.
    Переопределяется через JUDGE_MODEL.
    """
    return YandexAgent(
        agent_id="judge",
        model_name=os.getenv("JUDGE_MODEL", "yandexgpt"),
        api_key=_require_key(),
        max_tokens=int(os.getenv("JUDGE_MAX_TOKENS", "2048")),
    )


@lru_cache(maxsize=1)
def get_simple_agent() -> BaseDebateAgent:
    """
    Агент для обычного режима — один быстрый вызов без дебатов.
    По умолчанию yandexgpt-lite, переопределяется через SIMPLE_MODEL.
    Лимит токенов берётся из SIMPLE_MAX_TOKENS (default 1000),
    чтобы не превысить ограничение yandexgpt-lite (~2000 токенов вывода).
    """
    return YandexAgent(
        agent_id="simple",
        model_name=os.getenv("SIMPLE_MODEL", "yandexgpt-lite"),
        api_key=_require_key(),
        max_tokens=int(os.getenv("SIMPLE_MAX_TOKENS", "1000")),
    )
