from langgraph.graph import END
from langchain_core.messages import HumanMessage, SystemMessage

from client import OpenAIClient
from tools import describe_tools

from .base import describe_node
from .state import LoopState


@describe_node("第一次输入，初始化循环状态。")
def init_node(state: LoopState) -> LoopState:
    system_prompt = (
        f"你是工具型助手。可用工具: {describe_tools()}。"
        "先复用已有结果，缺信息才调工具；不要重复调用同一工具。"
        "回答简短。"
    )

    return {
        "count": 0,
        "messages": [
            SystemMessage(content=system_prompt),
            HumanMessage(content=state["user_input"]),
        ],
        "todo_list": {"items": []},
        "history": [],
        "last_tool_called": False,
    }


@describe_node("执行一轮 LLM 调用；如有 tool call 则执行工具并更新会话上下文。")
def loop_step(state: LoopState, client: OpenAIClient) -> LoopState:
    next_count = state["count"] + 1
    response = client(state["messages"])
    text = response.text if hasattr(response, "text") else str(response.content)

    return {
        "count": next_count,
        "messages": [response],
        "history": [text],
        "last_tool_called": bool(response.tool_calls),
    }


@describe_node("根据本轮是否调用工具以及循环次数上限，决定继续 loop 还是结束。")
def should_continue(state: LoopState) -> str:
    print(
        "[debug] route decision:",
        {
            "count": state["count"],
            "max_count": state["max_count"],
            "last_tool_called": state["last_tool_called"],
        },
    )
    if state["last_tool_called"] and state["count"] < state["max_count"]:
        return "tools"
    return END
