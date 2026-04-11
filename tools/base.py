from langchain_core.tools import BaseTool

_TOOLS: dict[str, BaseTool] = {}


def register_tool(tool_obj: BaseTool) -> BaseTool:
    if tool_obj.name in _TOOLS:
        raise ValueError(f"Duplicate tool registration: {tool_obj.name}")
    _TOOLS[tool_obj.name] = tool_obj
    return tool_obj


def get_tools() -> list[BaseTool]:
    return list(_TOOLS.values())
