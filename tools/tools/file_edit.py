from pathlib import Path

from langchain_core.tools import tool

from tools.base import register_tool


@register_tool
@tool
def file_edit(
    action: str,
    path: str,
    content: str = "",
    old_text: str = "",
    new_text: str = "",
) -> str:
    """本地文件操作，支持 path、read、write、edit。"""
    file_path = Path(path).expanduser().resolve()

    if action == "path":
        return f"Path: {file_path} | Exists: {file_path.exists()} | Is file: {file_path.is_file()}"

    if action == "read":
        return file_path.read_text(encoding="utf-8")

    if action == "write":
        file_path.parent.mkdir(parents=True, exist_ok=True)
        file_path.write_text(content, encoding="utf-8")
        return f"Wrote {len(content)} characters to {file_path}"

    if action == "edit":
        current = file_path.read_text(encoding="utf-8")
        if old_text not in current:
            return f"Target text not found in {file_path}"
        updated = current.replace(old_text, new_text, 1)
        file_path.write_text(updated, encoding="utf-8")
        return f"Edited {file_path}"

    return f"Unsupported action: {action}"
