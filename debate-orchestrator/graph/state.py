from typing import Annotated, Dict, List, Optional
import operator
from typing_extensions import TypedDict


class AgentMessage(TypedDict):
    agent_id: str
    model: str
    content: str
    round: int
    message_type: str  # "answer" | "critique" | "refined_answer"


class DebateState(TypedDict):
    query: str
    round: int
    max_rounds: int
    # Annotated с operator.add означает, что новые сообщения добавляются к списку,
    # а не заменяют его — LangGraph вызывает reducer при каждом обновлении state
    messages: Annotated[List[AgentMessage], operator.add]
    final_answer: Optional[str]
    processing_time_ms: Optional[int]
