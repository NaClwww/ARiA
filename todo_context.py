def build_todo_context(todo_list: dict) -> str:
    items = todo_list.get("items", [])
    lines = [
        "TODO summary:",
        *[
            f"- {item['id']} | {item['title']} | {item['status']}"
            for item in items
        ],
        "Use the real todo id from this summary.",
        "Use complete_todos with one or more real todo ids to mark completed items.",
        "End only when all items are done.",
    ]
    return "\n".join(lines)


TODO_CONTEXT_MESSAGE_ID = "todo-summary"
