"""
Тесты для чистых функций-хелперов в graph/nodes.py.
Не требуют LLM-вызовов и сетевого доступа.
"""
import pytest

from graph.nodes import _short, _get_latest_answers, _get_critiques_for_agent
from graph.nodes import check_convergence
from graph.state import AgentMessage, DebateState


# ─── _short ──────────────────────────────────────────────────────────────────

def test_short_no_truncation():
    text = "Hello world"
    assert _short(text, max_len=100) == "Hello world"


def test_short_truncation():
    text = "A" * 300
    result = _short(text, max_len=200)
    assert len(result) == 201  # 200 символов + "…"
    assert result.endswith("…")


def test_short_exact_boundary():
    text = "B" * 200
    result = _short(text, max_len=200)
    # Ровно 200 — не обрезается
    assert result == text
    assert not result.endswith("…")


def test_short_strips_newlines():
    text = "line one\nline two"
    result = _short(text, max_len=100)
    assert "\n" not in result
    assert "line one" in result
    assert "line two" in result


def test_short_strips_whitespace():
    text = "  hello  "
    result = _short(text, max_len=100)
    assert result == "hello"


# ─── _get_latest_answers ─────────────────────────────────────────────────────

def _msg(agent_id: str, message_type: str, content: str = "content", round_: int = 0) -> AgentMessage:
    return AgentMessage(
        agent_id=agent_id,
        model="test-model",
        content=content,
        round=round_,
        message_type=message_type,
    )


def test_get_latest_answers_empty():
    assert _get_latest_answers([]) == {}


def test_get_latest_answers_returns_answers():
    messages = [
        _msg("agent_a", "answer", "first answer"),
        _msg("agent_b", "answer", "second answer"),
    ]
    result = _get_latest_answers(messages)
    assert set(result.keys()) == {"agent_a", "agent_b"}
    assert result["agent_a"]["content"] == "first answer"


def test_get_latest_answers_ignores_critiques():
    messages = [
        _msg("agent_a", "answer"),
        _msg("agent_a->>agent_b", "critique"),  # critique не считается ответом
    ]
    result = _get_latest_answers(messages)
    assert "agent_a" in result
    assert "agent_a->>agent_b" not in result


def test_get_latest_answers_prefers_refined_over_answer():
    """refined_answer должен заменить answer для того же агента."""
    messages = [
        _msg("agent_a", "answer",          "initial",  round_=0),
        _msg("agent_a", "refined_answer",  "refined",  round_=1),
    ]
    result = _get_latest_answers(messages)
    assert result["agent_a"]["content"] == "refined"
    assert result["agent_a"]["message_type"] == "refined_answer"


def test_get_latest_answers_last_message_wins():
    """Если у агента несколько refined_answer, берётся последний по списку."""
    messages = [
        _msg("agent_a", "refined_answer", "round 1", round_=1),
        _msg("agent_a", "refined_answer", "round 2", round_=2),
    ]
    result = _get_latest_answers(messages)
    assert result["agent_a"]["content"] == "round 2"


# ─── _get_critiques_for_agent ─────────────────────────────────────────────────

def _critique(critic: str, target: str, round_: int = 1) -> AgentMessage:
    return AgentMessage(
        agent_id=f"{critic}->>{target}",
        model="test-model",
        content=f"critique from {critic}",
        round=round_,
        message_type="critique",
    )


def test_get_critiques_for_agent_finds_critiques():
    messages = [
        _critique("agent_a", "agent_b", round_=1),
        _critique("agent_c", "agent_b", round_=1),
    ]
    result = _get_critiques_for_agent("agent_b", messages, current_round=1)
    assert len(result) == 2


def test_get_critiques_for_agent_ignores_other_targets():
    messages = [
        _critique("agent_a", "agent_b", round_=1),
        _critique("agent_a", "agent_c", round_=1),  # цель — другой агент
    ]
    result = _get_critiques_for_agent("agent_b", messages, current_round=1)
    assert len(result) == 1
    assert result[0]["agent_id"] == "agent_a->>agent_b"


def test_get_critiques_for_agent_ignores_wrong_round():
    messages = [
        _critique("agent_a", "agent_b", round_=1),
        _critique("agent_a", "agent_b", round_=2),  # другой раунд
    ]
    result = _get_critiques_for_agent("agent_b", messages, current_round=1)
    assert len(result) == 1
    assert result[0]["round"] == 1


def test_get_critiques_for_agent_empty_messages():
    assert _get_critiques_for_agent("agent_a", [], current_round=1) == []


# ─── check_convergence ───────────────────────────────────────────────────────

def _state(round_: int, max_rounds: int) -> DebateState:
    return DebateState(
        query="test query",
        round=round_,
        max_rounds=max_rounds,
        messages=[],
        final_answer=None,
        processing_time_ms=None,
    )


def test_check_convergence_continue():
    assert check_convergence(_state(round_=0, max_rounds=2)) == "continue"


def test_check_convergence_done_at_max():
    assert check_convergence(_state(round_=2, max_rounds=2)) == "done"


def test_check_convergence_done_over_max():
    """Если раунд превысил максимум — тоже done (защита от edge-case)."""
    assert check_convergence(_state(round_=5, max_rounds=2)) == "done"


def test_check_convergence_single_round():
    assert check_convergence(_state(round_=0, max_rounds=1)) == "continue"
    assert check_convergence(_state(round_=1, max_rounds=1)) == "done"
