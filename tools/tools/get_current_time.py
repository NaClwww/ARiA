import json
from datetime import datetime

from langchain_core.tools import tool

from tools.base import register_tool


@register_tool
@tool
def get_current_time() -> str:
    """获取当前时间。"""
    return json.dumps({"now": datetime.now().isoformat()}, ensure_ascii=False)
