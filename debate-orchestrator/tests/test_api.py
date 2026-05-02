"""
Тесты FastAPI-эндпоинтов debate-orchestrator.

Большинство тестов используют DEBUG_BACKEND=true (выставлено в conftest.py),
чтобы не тратить токены. Тест с реальным дебатом мокирует агентов.
"""
import pytest
import pytest_asyncio

from httpx import AsyncClient, ASGITransport
from unittest.mock import patch, AsyncMock


# ─── helpers ─────────────────────────────────────────────────────────────────

@pytest_asyncio.fixture
async def client():
    """AsyncClient, подключённый к тестируемому приложению."""
    from main import app
    async with AsyncClient(
        transport=ASGITransport(app=app),
        base_url="http://testserver",
    ) as c:
        yield c


# ─── /api/v1/health ──────────────────────────────────────────────────────────

@pytest.mark.asyncio
async def test_health_returns_200(client):
    response = await client.get("/api/v1/health")
    assert response.status_code == 200


@pytest.mark.asyncio
async def test_health_contains_service_name(client):
    data = (await client.get("/api/v1/health")).json()
    assert data.get("service") == "debate-orchestrator"


@pytest.mark.asyncio
async def test_health_has_status_field(client):
    data = (await client.get("/api/v1/health")).json()
    assert "status" in data


# ─── /api/v1/generate — валидация входных данных ─────────────────────────────

@pytest.mark.asyncio
async def test_generate_empty_prompt_returns_400(client):
    response = await client.post("/api/v1/generate", json={"prompt": ""})
    assert response.status_code == 400


@pytest.mark.asyncio
async def test_generate_whitespace_prompt_returns_400(client):
    response = await client.post("/api/v1/generate", json={"prompt": "   "})
    assert response.status_code == 400


@pytest.mark.asyncio
async def test_generate_missing_prompt_returns_422(client):
    """Нет обязательного поля prompt → Pydantic вернёт 422."""
    response = await client.post("/api/v1/generate", json={})
    assert response.status_code == 422


# ─── /api/v1/generate — DEBUG_BACKEND заглушка ───────────────────────────────

@pytest.mark.asyncio
async def test_generate_debug_returns_stub(client):
    """DEBUG_BACKEND=true: ответ должен содержать stub-сообщение без LLM-вызовов."""
    import main
    # conftest выставил DEBUG_BACKEND=true, но переменная читается при старте модуля.
    # Патчим напрямую на случай, если модуль уже загружен с другим значением.
    with patch.object(main, "_DEBUG_BACKEND", True):
        response = await client.post(
            "/api/v1/generate",
            json={"prompt": "What is the meaning of life?"},
        )
    assert response.status_code == 200
    data = response.json()
    assert data["model"] == "stub"
    assert data["debate_rounds"] == 0
    assert data["debate_log"] == []


@pytest.mark.asyncio
async def test_generate_debug_response_schema(client):
    """Проверяем, что схема ответа совместима с контрактом model-proxy."""
    import main
    with patch.object(main, "_DEBUG_BACKEND", True):
        response = await client.post("/api/v1/generate", json={"prompt": "test"})

    data = response.json()
    required_fields = {"response", "model", "processing_time", "debate_rounds", "debate_log"}
    missing = required_fields - set(data.keys())
    assert not missing, f"Missing fields in response: {missing}"


# ─── /api/v1/generate — полный дебат с мок-агентами ─────────────────────────

class _MockAgent:
    """Имитирует BaseDebateAgent без сетевых вызовов."""
    def __init__(self, agent_id: str, model_name: str, answer_text: str):
        self.agent_id = agent_id
        self.model_name = model_name
        self._answer_text = answer_text

    async def answer(self, prompt: str) -> str:
        return self._answer_text


@pytest.mark.asyncio
async def test_generate_full_debate_with_mock_agents():
    """
    Полный прогон дебата (DEBUG_BACKEND=false) с замоканными агентами.
    Проверяет структуру ответа и логику graph-а без реальных LLM.
    """
    import main

    mock_debaters = [
        _MockAgent("debater_a", "mock-a", "A is correct because of X."),
        _MockAgent("debater_b", "mock-b", "B disagrees, Y is the reason."),
        _MockAgent("debater_c", "mock-c", "C agrees with A, X is clear."),
    ]
    mock_judge = _MockAgent("judge", "mock-judge", "Synthesized answer: X and Y are both valid.")

    with (
        patch("agents.get_debaters", return_value=mock_debaters),
        patch("agents.get_judge",    return_value=mock_judge),
        patch.object(main, "_DEBUG_BACKEND", False),
    ):
        async with AsyncClient(
            transport=ASGITransport(app=main.app),
            base_url="http://testserver",
        ) as client:
            response = await client.post(
                "/api/v1/generate",
                json={"prompt": "Is X or Y correct?", "max_rounds": 1},
            )

    assert response.status_code == 200
    data = response.json()

    assert data["response"] == "Synthesized answer: X and Y are both valid."
    assert data["model"] == "debate-ensemble"
    assert data["debate_rounds"] >= 1
    assert len(data["debate_log"]) > 0


@pytest.mark.asyncio
async def test_generate_debate_log_contains_all_roles():
    """Лог дебата должен содержать ответы дебатёров, критику и судью."""
    import main

    mock_debaters = [
        _MockAgent("debater_a", "mock-a", "Answer A"),
        _MockAgent("debater_b", "mock-b", "Answer B"),
    ]
    mock_judge = _MockAgent("judge", "mock-judge", "Final verdict")

    with (
        patch("agents.get_debaters", return_value=mock_debaters),
        patch("agents.get_judge",    return_value=mock_judge),
        patch.object(main, "_DEBUG_BACKEND", False),
    ):
        async with AsyncClient(
            transport=ASGITransport(app=main.app),
            base_url="http://testserver",
        ) as client:
            response = await client.post(
                "/api/v1/generate",
                json={"prompt": "test question", "max_rounds": 1},
            )

    data = response.json()
    message_types = {m["message_type"] for m in data["debate_log"]}

    # При max_rounds=1 должны быть: answer, critique, refined_answer
    assert "answer" in message_types, f"Expected 'answer' in log, got: {message_types}"
    assert "critique" in message_types, f"Expected 'critique' in log, got: {message_types}"


@pytest.mark.asyncio
async def test_generate_max_rounds_respected():
    """max_rounds=2 должен привести к двум итерациям дебата."""
    import main

    mock_debaters = [
        _MockAgent("debater_a", "mock-a", "Answer"),
        _MockAgent("debater_b", "mock-b", "Answer"),
    ]
    mock_judge = _MockAgent("judge", "mock-judge", "Final")

    with (
        patch("agents.get_debaters", return_value=mock_debaters),
        patch("agents.get_judge",    return_value=mock_judge),
        patch.object(main, "_DEBUG_BACKEND", False),
    ):
        async with AsyncClient(
            transport=ASGITransport(app=main.app),
            base_url="http://testserver",
        ) as client:
            response = await client.post(
                "/api/v1/generate",
                json={"prompt": "test", "max_rounds": 2},
            )

    assert response.status_code == 200
    assert response.json()["debate_rounds"] == 2
