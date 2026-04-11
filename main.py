import json

from graph import build_graph


def main():
    user_input = input("请输入你的问题: ").strip()
    if not user_input:
        raise RuntimeError("User input cannot be empty")

    app = build_graph()
    result = app.invoke(
        {
            "count": 0,
            "max_count": 3,
            "user_input": user_input,
            "messages": [],
            "todo_list": {"items": []},
            "history": [],
            "last_tool_called": False,
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
