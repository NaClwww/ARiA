import operator
from typing import Annotated
from typing import TypedDict

from langgraph.graph.message import add_messages


class TODOItem(TypedDict):
    id: str
    title: str
    content: str
    status: str


class TODOList(TypedDict):
    items: list[TODOItem]


class LoopState(TypedDict):
    count: int
    user_input: str
    allow_todo_list: bool
    enable_todo_list: bool
    messages: Annotated[list, add_messages]
    todo_list: TODOList
    todo_stall_count: int
    history: Annotated[list[str], operator.add]
    last_tool_called: bool
    last_todo_update_called: bool
