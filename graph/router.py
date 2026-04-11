from langgraph.graph import END
import logging

from .base import describe_node
from .state import LoopState


logger = logging.getLogger(__name__)


@describe_node("根据用户开关决定是否生成 ToDoList。")
def route_after_init(state: LoopState) -> str:
    if state["allow_todo_list"]:
        return "decide_todo"
    return "loop"


@describe_node("根据 LLM 对任务复杂度的判断，决定是否进入 ToDoList。")
def route_after_todo_decision(state: LoopState) -> str:
    if state["enable_todo_list"]:
        return "todo"
    return "loop"


@describe_node("根据本轮是否调用工具以及循环次数上限，决定继续 tools 还是结束。")
def route_after_loop(state: LoopState) -> str:
    has_todos = state["enable_todo_list"] and bool(state["todo_list"]["items"])
    todo_done_by_state = (
        all(str(item.get("status", "")).strip().lower() == "done" for item in state["todo_list"]["items"])
        if has_todos
        else False
    )
    todo_done = todo_done_by_state

    logger.info(
        "route decision=%s",
        {
            "count": state["count"],
            "last_tool_called": state["last_tool_called"],
            "last_todo_update_called": state["last_todo_update_called"],
            "has_todos": has_todos,
            "todo_done_by_state": todo_done_by_state,
            "todo_done": todo_done,
            "todo_stall_count": state["todo_stall_count"],
            "todo_items": [
                {"id": item["id"], "status": item["status"]}
                for item in state["todo_list"]["items"]
            ],
        },
    )

    if state["last_tool_called"]:
        logger.info("routing=tools reason=tool_call")
        return "tools"

    if has_todos and todo_done:
        logger.info("routing=end reason=all_todos_done")
        return END

    if has_todos and not todo_done:
        if state["todo_stall_count"] >= 3:
            logger.info("routing=reminder reason=todo_stalled")
            return "reminder"
        logger.info("routing=loop reason=todos_pending")
        return "loop"
    return END
