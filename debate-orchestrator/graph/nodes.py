import asyncio
import logging
import textwrap
from typing import Dict, List

from .state import AgentMessage, DebateState

logger = logging.getLogger("debate")


# ---------------------------------------------------------------------------
# Prompt templates
# ---------------------------------------------------------------------------

_INITIAL_ANSWER_PROMPT = """You are an AI assistant participating in a structured multi-agent debate.
Your goal is to provide the most accurate and well-reasoned answer possible.

Question: {query}

Provide a thorough, well-reasoned answer. Be specific and back your claims."""


_CRITIQUE_PROMPT = """You are participating in a multi-agent debate as a critical reviewer.

Original question: {query}

Another agent's answer:
\"\"\"
{answer}
\"\"\"

Critically evaluate this answer. Identify:
1. Factual errors or inaccuracies
2. Missing important information
3. Logical flaws or weak reasoning
4. What is correct and should be preserved

Be constructive, specific, and concise."""


_REFINE_PROMPT = """You are participating in a multi-agent debate. You previously gave an answer,
and other agents have critiqued it. Use their feedback to improve your response.

Original question: {query}

Your previous answer:
\"\"\"
{previous_answer}
\"\"\"

Critiques from other agents:
{critiques}

Refine your answer: address valid criticisms, correct errors, and defend positions
you still believe are correct. Output only the improved answer."""


_SYNTHESIZE_PROMPT = """You are the judge in a multi-agent debate that ran for {rounds} round(s).
Your task is to synthesize the agents' final positions into one definitive answer.

Original question: {query}

Final answers from each agent:
{final_answers}

Instructions:
- Merge the strongest, best-supported points from all agents
- Resolve disagreements using your own judgment
- Produce a clear, comprehensive, accurate final answer
- Do NOT reference the debate process itself — just answer the question"""


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _short(text: str, max_len: int = 200) -> str:
    """Обрезает текст для лога, добавляет многоточие если длиннее."""
    text = text.strip().replace("\n", " ")
    return text[:max_len] + "…" if len(text) > max_len else text


def _get_latest_answers(messages: List[AgentMessage]) -> Dict[str, AgentMessage]:
    """Return the most recent answer/refined_answer per agent."""
    latest: Dict[str, AgentMessage] = {}
    for msg in messages:
        if msg["message_type"] in ("answer", "refined_answer"):
            latest[msg["agent_id"]] = msg
    return latest


def _get_critiques_for_agent(
    agent_id: str, messages: List[AgentMessage], current_round: int
) -> List[AgentMessage]:
    """Return all critiques directed at agent_id in the given round."""
    return [
        m for m in messages
        if m["message_type"] == "critique"
        and m["agent_id"].endswith(f"->>{agent_id}")
        and m["round"] == current_round
    ]


async def _identity(value: str) -> str:
    """Вспомогательная корутина — просто возвращает значение без изменений."""
    return value


# ---------------------------------------------------------------------------
# LangGraph nodes
# ---------------------------------------------------------------------------

async def generate_initial_answers(state: DebateState) -> Dict:
    """Round 0 — каждый агент отвечает на вопрос независимо."""
    from agents import get_debaters

    debaters = get_debaters()

    logger.info("=" * 60)
    logger.info("DEBATE START")
    logger.info("Query: %s", _short(state["query"], 120))
    logger.info("Debaters: %s", ", ".join(f"{d.agent_id}({d.model_name})" for d in debaters))
    logger.info("Max rounds: %d", state["max_rounds"])
    logger.info("=" * 60)
    logger.info("[Round 0] Generating initial answers...")

    tasks = [agent.answer(_INITIAL_ANSWER_PROMPT.format(query=state["query"])) for agent in debaters]
    results = await asyncio.gather(*tasks)

    new_messages: List[AgentMessage] = []
    for agent, content in zip(debaters, results):
        logger.info(
            "  [%s / %s] → %s",
            agent.agent_id, agent.model_name, _short(content),
        )
        new_messages.append(AgentMessage(
            agent_id=agent.agent_id,
            model=agent.model_name,
            content=content,
            round=0,
            message_type="answer",
        ))

    return {"messages": new_messages, "round": 0}


async def critique_peers(state: DebateState) -> Dict:
    """Каждый агент читает ответы остальных и пишет критику."""
    from agents import get_debaters

    debaters = get_debaters()
    current_round = state["round"]
    latest_answers = _get_latest_answers(state["messages"])

    logger.info("[Round %d] Critiquing peers...", current_round)

    tasks = []
    task_meta = []

    for critic in debaters:
        for answerer_id, answer_msg in latest_answers.items():
            if answerer_id == critic.agent_id:
                continue
            prompt = _CRITIQUE_PROMPT.format(
                query=state["query"],
                answer=answer_msg["content"],
            )
            tasks.append(critic.answer(prompt))
            task_meta.append((critic, answerer_id))

    results = await asyncio.gather(*tasks)

    new_messages: List[AgentMessage] = []
    for (critic, target_id), content in zip(task_meta, results):
        logger.info(
            "  [%s] → critiques [%s]: %s",
            critic.agent_id, target_id, _short(content),
        )
        new_messages.append(AgentMessage(
            agent_id=f"{critic.agent_id}->>{target_id}",
            model=critic.model_name,
            content=content,
            round=current_round,
            message_type="critique",
        ))

    return {"messages": new_messages}


async def refine_positions(state: DebateState) -> Dict:
    """Каждый агент обновляет свой ответ с учётом полученной критики."""
    from agents import get_debaters

    debaters = get_debaters()
    current_round = state["round"]
    latest_answers = _get_latest_answers(state["messages"])

    logger.info("[Round %d] Refining positions...", current_round)

    tasks = []
    agents_with_critiques = []

    for agent in debaters:
        previous = latest_answers.get(agent.agent_id)
        if previous is None:
            continue

        critiques = _get_critiques_for_agent(agent.agent_id, state["messages"], current_round)
        if not critiques:
            agents_with_critiques.append((agent, None))
            tasks.append(_identity(previous["content"]))
            continue

        critique_text = "\n\n".join(
            f"[Agent {i+1}]: {c['content']}" for i, c in enumerate(critiques)
        )
        prompt = _REFINE_PROMPT.format(
            query=state["query"],
            previous_answer=previous["content"],
            critiques=critique_text,
        )
        tasks.append(agent.answer(prompt))
        agents_with_critiques.append((agent, True))

    results = await asyncio.gather(*tasks)
    next_round = current_round + 1

    new_messages: List[AgentMessage] = []
    for (agent, had_critiques), content in zip(agents_with_critiques, results):
        tag = "refined" if had_critiques else "unchanged"
        logger.info(
            "  [%s / %s] (%s) → %s",
            agent.agent_id, agent.model_name, tag, _short(content),
        )
        new_messages.append(AgentMessage(
            agent_id=agent.agent_id,
            model=agent.model_name,
            content=content,
            round=next_round,
            message_type="refined_answer",
        ))

    return {"messages": new_messages, "round": next_round}


async def synthesize(state: DebateState) -> Dict:
    """Judge-модель синтезирует финальный ответ из всех позиций агентов."""
    from agents import get_judge

    judge = get_judge()
    latest_answers = _get_latest_answers(state["messages"])

    logger.info("[Synthesis] Judge %s(%s) is synthesizing...", judge.agent_id, judge.model_name)

    final_answers_text = "\n\n".join(
        f"[{msg['agent_id']} / {msg['model']}]:\n{msg['content']}"
        for msg in latest_answers.values()
    )

    prompt = _SYNTHESIZE_PROMPT.format(
        rounds=state["round"],
        query=state["query"],
        final_answers=final_answers_text,
    )

    final_answer = await judge.answer(prompt)

    logger.info("[Synthesis] Judge verdict → %s", _short(final_answer))

    return {"final_answer": final_answer}


def check_convergence(state: DebateState) -> str:
    """Conditional edge: продолжать дебаты или переходить к синтезу."""
    if state["round"] >= state["max_rounds"]:
        return "done"
    return "continue"
