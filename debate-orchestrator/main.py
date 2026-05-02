import asyncio
import logging
import os
import time
from contextlib import asynccontextmanager

from dotenv import load_dotenv
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

load_dotenv()

from graph import DebateState, debate_graph

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
    datefmt="%H:%M:%S",
)
debate_logger = logging.getLogger("debate")

_DEBATE_TIMEOUT = int(os.getenv("DEBATE_TIMEOUT_SECONDS", "300"))
_DEBUG_BACKEND = os.getenv("DEBUG_BACKEND", "false").lower() in ("true", "1", "yes")


# ---------------------------------------------------------------------------
# Schemas — совместимы с контрактом Go model-proxy для бесшовной замены
# ---------------------------------------------------------------------------

class GenerateRequest(BaseModel):
    prompt: str
    max_rounds: int = Field(default=1, ge=1, le=5)


class AgentMessageOut(BaseModel):
    agent_id: str
    model: str
    content: str
    round: int
    message_type: str


class GenerateResponse(BaseModel):
    response: str                        # итоговый ответ (совместимо с model-proxy)
    model: str                           # "debate-ensemble"
    processing_time: int                 # мс
    debate_rounds: int
    debate_log: list[AgentMessageOut]    # полная история дебатов


# ---------------------------------------------------------------------------
# App
# ---------------------------------------------------------------------------

@asynccontextmanager
async def lifespan(app: FastAPI):
    from agents import get_debaters, get_judge, get_simple_agent
    try:
        get_debaters()
        get_judge()
        get_simple_agent()
    except RuntimeError as e:
        print(f"[WARNING] Agent init: {e}")
    yield


app = FastAPI(
    title="Debate Orchestrator",
    description="Multi-LLM debate service — агенты спорят, судья синтезирует ответ",
    version="1.0.0",
    lifespan=lifespan,
)


def _log_debate_digest(result: DebateState, elapsed_ms: int) -> None:
    """Печатает итоговый дайджест дебатов одним читаемым блоком."""
    sep = "─" * 60
    lines = [
        "",
        sep,
        "DEBATE DIGEST",
        f"Query   : {result['query'][:120]}",
        f"Rounds  : {result['round']}  |  Time: {elapsed_ms}ms",
        sep,
    ]

    # Финальные позиции каждого дебатёра
    from graph.nodes import _get_latest_answers
    latest = _get_latest_answers(result["messages"])
    for agent_id, msg in latest.items():
        lines.append(f"[{agent_id} / {msg['model']}]")
        # Первые 300 символов ответа
        preview = msg["content"].strip().replace("\n", " ")[:300]
        lines.append(f"  {preview}{'…' if len(msg['content']) > 300 else ''}")

    # Вердикт судьи
    lines.append(sep)
    lines.append("[JUDGE VERDICT]")
    if result["final_answer"]:
        verdict = result["final_answer"].strip().replace("\n", " ")[:400]
        lines.append(f"  {verdict}{'…' if len(result['final_answer']) > 400 else ''}")
    lines.append(sep)
    lines.append("")

    debate_logger.info("\n".join(lines))


@app.post("/api/v1/generate", response_model=GenerateResponse)
async def generate(req: GenerateRequest):
    if not req.prompt.strip():
        raise HTTPException(status_code=400, detail="prompt cannot be empty")

    initial_state: DebateState = {
        "query": req.prompt,
        "round": 0,
        "max_rounds": req.max_rounds,
        "messages": [],
        "final_answer": None,
        "processing_time_ms": None,
    }

    start = time.monotonic()

    if _DEBUG_BACKEND:
        debate_logger.info("[DEBUG_BACKEND] Stub mode active — skipping LLM calls")
        return GenerateResponse(
            response="⚙️ Сейчас ведутся технические работы, подождите...",
            model="stub",
            processing_time=0,
            debate_rounds=0,
            debate_log=[],
        )

    try:
        result: DebateState = await asyncio.wait_for(
            debate_graph.ainvoke(initial_state),
            timeout=_DEBATE_TIMEOUT,
        )
    except asyncio.TimeoutError:
        raise HTTPException(
            status_code=504,
            detail=f"Debate timed out after {_DEBATE_TIMEOUT}s. "
                   "Try reducing max_rounds or check API availability.",
        )
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

    elapsed_ms = int((time.monotonic() - start) * 1000)
    _log_debate_digest(result, elapsed_ms)

    return GenerateResponse(
        response=result["final_answer"] or "",
        model="debate-ensemble",
        processing_time=elapsed_ms,
        debate_rounds=result["round"],
        debate_log=[AgentMessageOut(**m) for m in result["messages"]],
    )


class SimpleRequest(BaseModel):
    prompt: str


class SimpleResponse(BaseModel):
    response: str
    model: str
    processing_time: int  # мс


@app.post("/api/v1/simple", response_model=SimpleResponse)
async def simple_generate(req: SimpleRequest):
    """Обычный режим: один вызов к модели без дебатов."""
    if not req.prompt.strip():
        raise HTTPException(status_code=400, detail="prompt cannot be empty")

    if _DEBUG_BACKEND:
        debate_logger.info("[DEBUG_BACKEND] Simple stub mode active")
        return SimpleResponse(
            response="⚙️ Сейчас ведутся технические работы, подождите...",
            model="stub",
            processing_time=0,
        )

    from agents import get_simple_agent
    agent = get_simple_agent()

    start = time.monotonic()
    try:
        result = await asyncio.wait_for(
            agent.answer(req.prompt),
            timeout=_DEBATE_TIMEOUT,
        )
    except asyncio.TimeoutError:
        raise HTTPException(
            status_code=504,
            detail=f"Request timed out after {_DEBATE_TIMEOUT}s.",
        )
    except Exception as e:
        debate_logger.error("[Simple] Exception: %s", str(e), exc_info=True)
        raise HTTPException(status_code=500, detail=str(e))

    elapsed_ms = int((time.monotonic() - start) * 1000)
    debate_logger.info(
        "[Simple] model=%s time=%dms response=%s",
        agent.model_name, elapsed_ms, result[:200].replace("\n", " "),
    )
    return SimpleResponse(
        response=result,
        model=agent.model_name,
        processing_time=elapsed_ms,
    )


@app.get("/api/v1/health")
async def health():
    from agents import get_debaters, get_judge

    checks: dict = {}
    try:
        debaters = get_debaters()
        checks["debaters"] = [f"{a.agent_id}:{a.model_name}" for a in debaters]
        checks["judge"] = f"{get_judge().agent_id}:{get_judge().model_name}"
        status = "healthy"
    except RuntimeError as e:
        checks["error"] = str(e)
        status = "unhealthy"

    return {"status": status, "service": "debate-orchestrator", "checks": checks}
