from abc import ABC, abstractmethod


class BaseDebateAgent(ABC):
    """Базовый интерфейс для любого агента-участника дебатов."""

    def __init__(self, agent_id: str, model_name: str):
        self.agent_id = agent_id
        self.model_name = model_name

    @abstractmethod
    async def answer(self, prompt: str) -> str:
        """Отправить промпт модели и вернуть текстовый ответ."""
        ...

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(id={self.agent_id}, model={self.model_name})"
