import json

from graph import build_graph
from logging_config import setup_logging


def main():
    setup_logging()
    user_input = input("请输入你的问题: ").strip()
    if not user_input:
        raise RuntimeError("User input cannot be empty")
    allow_todo_list = input("开启自动 Plan? [Y/n]: ").strip().lower() not in {"n", "no"}

    app = build_graph()
    result = app.invoke(
        {
            "count": 0,
            "user_input": user_input,
            "allow_todo_list": allow_todo_list,
            "enable_todo_list": False,
            "messages": [],
            "todo_list": {"items": []},
            "todo_stall_count": 0,
            "history": [],
            "last_tool_called": False,
            "last_todo_update_called": False,
        }
    )

    print("\nFinal state:")
    print(
        json.dumps(
            result,
            indent=2,
            ensure_ascii=False,
            default=lambda obj: obj.model_dump() if hasattr(obj, "model_dump") else str(obj),
        )
    )


if __name__ == "__main__":
    main()
