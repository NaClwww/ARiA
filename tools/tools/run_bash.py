import json
import os
import subprocess
import time
from pathlib import Path

from langchain_core.tools import tool

from tools.base import register_tool


@register_tool
@tool
def run_bash(command: str, background: bool = False, log_path: str = "") -> str:
    """运行本地 bash 命令；支持同步执行或后台运行。"""
    command = command.strip()
    if not command:
        return json.dumps({"ok": False, "error": "Missing command"}, ensure_ascii=False)

    if background:
        resolved_log_path = Path(log_path).expanduser().resolve() if log_path else Path(
            f"/tmp/aria_run_bash_{int(time.time() * 1000)}.log"
        )
        resolved_log_path.parent.mkdir(parents=True, exist_ok=True)

        with resolved_log_path.open("a", encoding="utf-8") as log_file:
            process = subprocess.Popen(
                command,
                shell=True,
                executable="/bin/bash",
                stdout=log_file,
                stderr=log_file,
                text=True,
                preexec_fn=os.setsid,
            )

        return json.dumps(
            {
                "ok": True,
                "background": True,
                "pid": process.pid,
                "log_path": str(resolved_log_path),
            },
            ensure_ascii=False,
        )

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
