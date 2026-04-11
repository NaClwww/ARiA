from typing import Annotated

from langchain_core.messages import ToolMessage
from langchain_core.tools import InjectedToolCallId, tool
from langgraph.prebuilt import InjectedState

from tools.base import register_tool
from todo_context import build_todo_context


@register_tool
@tool
def read_todo(
    tool_call_id: Annotated[str, InjectedToolCallId] = "",
    todo_list: Annotated[dict, InjectedState("todo_list")] = None,
) -> ToolMessage:
    """读取当前 todo list。"""
    if not todo_list or "items" not in todo_list:
        return ToolMessage(
            content=build_todo_context({"items": []}),
            tool_call_id=tool_call_id,
            name="read_todo",
        )

    return ToolMessage(
        content=build_todo_context(todo_list),
        tool_call_id=tool_call_id,
        name="read_todo",
    )
