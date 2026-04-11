import json
import subprocess

from langchain_core.tools import tool

from tools.base import register_tool


@register_tool
@tool
def run_bash(command: str) -> str:
    """运行本地 bash 命令并返回 stdout/stderr。"""
    command = command.strip()
    if not command:
        return json.dumps({"ok": False, "error": "Missing command"}, ensure_ascii=False)

    try:
        result = subprocess.run(
            command,
            shell=True,
            executable="/bin/bash",
            capture_output=True,
            text=True,
            timeout=10,
        )
    except subprocess.TimeoutExpired:
        return json.dumps(
            {"ok": False, "error": "Command timed out after 10 seconds"},
            ensure_ascii=False,
        )

    return json.dumps(
        {
            "ok": result.returncode == 0,
            "returncode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
        },
        ensure_ascii=False,
    )
