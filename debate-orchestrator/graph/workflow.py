from langgraph.graph import END, StateGraph

from .nodes import (
    check_convergence,
    critique_peers,
    generate_initial_answers,
    refine_positions,
    synthesize,
)
from .state import DebateState


def build_debate_graph():
    """
    Граф дебатов:

        generate → critique → refine → [check_convergence]
                       ↑_______________|  (если round < max_rounds)
                                          ↓ (иначе)
                                       synthesize → END
    """
    workflow = StateGraph(DebateState)

    workflow.add_node("generate", generate_initial_answers)
    workflow.add_node("critique", critique_peers)
    workflow.add_node("refine", refine_positions)
    workflow.add_node("synthesize", synthesize)

    workflow.set_entry_point("generate")
    workflow.add_edge("generate", "critique")
    workflow.add_edge("critique", "refine")
    workflow.add_conditional_edges(
        "refine",
        check_convergence,
        {
            "continue": "critique",
            "done": "synthesize",
        },
    )
    workflow.add_edge("synthesize", END)

    return workflow.compile()


# Singleton — компилируем граф один раз при импорте модуля
debate_graph = build_debate_graph()
