from langgraph.graph import START, StateGraph
from langgraph.prebuilt import ToolNode

from client import OpenAIClient
from tools import TOOLS

from .nodes import init_node, loop_step, should_continue
from .state import LoopState


def build_graph():
    client = OpenAIClient(tools=TOOLS)
    graph = StateGraph(LoopState)
    graph.add_node("init", init_node)
    graph.add_node("loop", lambda state: loop_step(state, client))
    graph.add_node("tools", ToolNode(TOOLS))
    
    graph.add_edge(START, "init")
    graph.add_edge("init", "loop")
    graph.add_edge("tools", "loop")
    graph.add_conditional_edges("loop", should_continue)
    return graph.compile()
