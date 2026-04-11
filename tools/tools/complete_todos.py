from typing import Annotated
import json
import logging

from langchain_core.messages import SystemMessage, ToolMessage
from langchain_core.tools import InjectedToolCallId, tool
from langgraph.prebuilt import InjectedState
from langgraph.types import Command

from tools.base import register_tool
from todo_context import TODO_CONTEXT_MESSAGE_ID, build_todo_context
logger = logging.getLogger(__name__)


@register_tool
@tool
def complete_todos(
    todo_ids: list[str],
    tool_call_id: Annotated[str, InjectedToolCallId] = "",
    todo_list: Annotated[dict, InjectedState("todo_list")] = None,
    todo_stall_count: Annotated[int, InjectedState("todo_stall_count")] = 0,
) -> Command:
    """批量将多个 todo 标记为 done。"""
    if not todo_list or "items" not in todo_list:
        return Command(update={})

    logger.info(
        "complete_todos before=%s",
        json.dumps(todo_list, indent=2, ensure_ascii=False),
    )

    todo_id_set = set(todo_ids)
    items = []
    changed = False
    matched_ids: set[str] = set()

    for item in todo_list["items"]:
        if item["id"] not in todo_id_set:
            items.append(item)
            continue

        matched_ids.add(item["id"])
        updated_item = {
            "id": item["id"],
            "title": item["title"],
            "content": item["content"],
            "status": "done",
        }
        changed = changed or updated_item != item
        items.append(updated_item)

    next_todo_list = {"items": items}
    todo_context = build_todo_context(next_todo_list)

    missing_ids = [todo_id for todo_id in todo_ids if todo_id not in matched_ids]
    if missing_ids:
        logger.info("complete_todos missing_ids=%s", missing_ids)

    logger.info(
        "complete_todos after=%s",
        json.dumps({"items": items}, indent=2, ensure_ascii=False),
    )

    return Command(
        update={
            "todo_list": next_todo_list,
            "todo_stall_count": 0 if changed else todo_stall_count + 1,
            "last_todo_update_called": True,
            "messages": [
                ToolMessage(
                    content=f"Completed todos: {', '.join(todo_ids)}",
                    tool_call_id=tool_call_id,
                    name="complete_todos",
                ),
                SystemMessage(content=todo_context, id=TODO_CONTEXT_MESSAGE_ID),
            ],
        }
    )
