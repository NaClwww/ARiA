from langgraph.graph import START, StateGraph
from langgraph.prebuilt import ToolNode

from client import OpenAIClient
from tools import TOOLS

from .nodes import (
    decide_todo_node,
    init_node,
    loop_step,
    reminder_node,
    todo_node,
)
from .router import route_after_init, route_after_loop, route_after_todo_decision
from .state import LoopState


def build_graph():
    client = OpenAIClient(tools=TOOLS)
    graph = StateGraph(LoopState)
    graph.add_node("init", init_node)
    graph.add_node("decide_todo", lambda state: decide_todo_node(state, client))
    graph.add_node("todo", lambda state: todo_node(state, client))
    graph.add_node("reminder", reminder_node)
    graph.add_node("loop", lambda state: loop_step(state, client))
    graph.add_node("tools", ToolNode(TOOLS))
    
    graph.add_edge(START, "init")
    graph.add_conditional_edges("init", route_after_init)
    graph.add_conditional_edges("decide_todo", route_after_todo_decision)
    graph.add_edge("todo", "loop")
    graph.add_edge("tools", "loop")
    graph.add_edge("reminder", "loop")
    graph.add_conditional_edges("loop", route_after_loop)
    return graph.compile()
