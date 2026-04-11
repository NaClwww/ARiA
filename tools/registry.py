import importlib
import pkgutil

from .base import get_tools
from . import tools as tool_modules


def _autoload_tools() -> None:
    for module_info in pkgutil.iter_modules(tool_modules.__path__):
        importlib.import_module(f"{tool_modules.__name__}.{module_info.name}")


_autoload_tools()

TOOLS = get_tools()


def describe_tools() -> str:
    return ", ".join(tool.name for tool in TOOLS)
