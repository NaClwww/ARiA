import uuid
import logging

from langchain_core.messages import HumanMessage, SystemMessage

from client import OpenAIClient
from tools import describe_tools
from todo_context import TODO_CONTEXT_MESSAGE_ID, build_todo_context

from .base import describe_node
from .state import LoopState
import json


logger = logging.getLogger(__name__)


def attach_todo_context(messages: list, todo_list: dict) -> list:
    request_messages = [
        message
        for message in messages
        if getattr(message, "id", None) != TODO_CONTEXT_MESSAGE_ID
    ]
    todo_message = SystemMessage(
        content=build_todo_context(todo_list),
        id=TODO_CONTEXT_MESSAGE_ID,
    )

    insert_at = 0
    while insert_at < len(request_messages) and isinstance(request_messages[insert_at], SystemMessage):
        insert_at += 1
    request_messages.insert(insert_at, todo_message)
    return request_messages


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
        "allow_todo_list": state["allow_todo_list"],
        "enable_todo_list": state["enable_todo_list"],
        "todo_list": {"items": []},
        "todo_stall_count": 0,
        "history": [],
        "last_tool_called": False,
        "last_todo_update_called": False,
    }


@describe_node("让 LLM 判断当前任务是否值得先生成 ToDoList。")
def decide_todo_node(state: LoopState, client: OpenAIClient) -> LoopState:
    prompt = (
        '判断这个任务是否需要先生成 todo list。'
        '多步骤、编码实现、需要工具协作或验证的任务返回 yes；'
        '简单问答或一步即可完成的任务返回 no。'
        '只输出 JSON: {"use_todo_list": true}'
    )
    response = client.invoke_raw(
        [
            SystemMessage(content=prompt),
            HumanMessage(content=state["user_input"]),
        ]
    )
    text = response.text if hasattr(response, "text") else str(response.content)

    try:
        parsed = json.loads(text)
        use_todo_list = bool(parsed.get("use_todo_list", False))
    except Exception:
        use_todo_list = False

    logger.info(
        "todo decision=%s",
        {"allow_todo_list": state["allow_todo_list"], "use_todo_list": use_todo_list},
    )

    update = {
        "enable_todo_list": use_todo_list,
    }
    if use_todo_list:
        update["messages"] = [
            SystemMessage(
                content="已启用 ToDo List。todo 只用于跟踪用户明确要求的工作；不要新增额外任务。当前上下文已包含最新 todo summary，通常不要调用 read_todo。完成任务时只使用 complete_todos，可一次传一个或多个 todo id；不要调用其他 todo 更新工具。",
                id="todo-policy",
            )
        ]

    return update


@describe_node("根据用户输入生成初始 ToDoList。")
def todo_node(state: LoopState, client: OpenAIClient) -> LoopState:
    prompt = (
        '请只根据用户明确要求生成 todo，不要新增 README、文档、重构、额外测试等衍生任务。'
        '简单任务生成 1-3 个 todo；只有明显复杂时才到 4-5 个。'
        'todo 要覆盖用户要求本身，不要把同一件事拆得过细。'
        '只输出 JSON: {"items":[{"id":"短id","title":"标题","content":"说明","status":"pending"}]}'
    )
    response = client.invoke_raw(
        [
            SystemMessage(content=prompt),
            HumanMessage(content=state["user_input"]),
        ]
    )
    text = response.text if hasattr(response, "text") else str(response.content)

    try:
        todo_list = json.loads(text)
        items = todo_list.get("items", [])
        normalized_items = []
        for item in items:
            normalized_items.append(     
                {
                    "id": item.get("id") or str(uuid.uuid4())[:8],
                    "title": item.get("title", ""),
                    "content": item.get("content", ""),
                    "status": item.get("status", "pending"),
                }
            )
        todo_list = {"items": normalized_items}
    except Exception:
        todo_list = {
            "items": [
                {
                    "id": str(uuid.uuid4())[:8],
                    "title": state["user_input"][:30],
                    "content": state["user_input"],
                    "status": "pending",
                }
            ]
        }

    logger.info("created todo_list=%s", json.dumps(todo_list, ensure_ascii=False))

    return {
        "todo_list": todo_list,
        "todo_stall_count": 0,
        "messages": [
            SystemMessage(
                content=build_todo_context(todo_list),
                id=TODO_CONTEXT_MESSAGE_ID,
            )
        ],
    }


@describe_node("将当前 ToDoList 压成简短 reminder 写入消息上下文。")
def reminder_node(state: LoopState) -> LoopState:
    if not state["enable_todo_list"] or not state["todo_list"]["items"]:
        return {}

    return {
        "messages": [
            SystemMessage(
                content=build_todo_context(state["todo_list"]),
                id=TODO_CONTEXT_MESSAGE_ID,
            )
        ],
    }


@describe_node("执行一轮 LLM 调用；如有 tool call 则执行工具并更新会话上下文。")
def loop_step(state: LoopState, client: OpenAIClient) -> LoopState:
    next_count = state["count"] + 1
    request_messages = state["messages"]
    if state["enable_todo_list"] and state["todo_list"]["items"]:
        request_messages = attach_todo_context(state["messages"], state["todo_list"])

    response = client(request_messages)
    text = response.text if hasattr(response, "text") else str(response.content)
    tool_calls = getattr(response, "tool_calls", []) or []
    last_todo_update_called = any(
        tool_call.get("name") == "complete_todos"
        for tool_call in tool_calls
    )
    has_todos = state["enable_todo_list"] and bool(state["todo_list"]["items"])
    next_stall_count = (
        0
        if last_todo_update_called
        else state["todo_stall_count"] + 1 if has_todos else state["todo_stall_count"]
    )

    return {
        "count": next_count,
        "messages": [response],
        "history": [text],
        "todo_stall_count": next_stall_count,
        "last_tool_called": bool(tool_calls),
        "last_todo_update_called": last_todo_update_called,
    }
