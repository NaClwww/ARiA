from collections.abc import Callable


NodeFunc = Callable[..., object]


def describe_node(description: str):
    def decorator(func: NodeFunc) -> NodeFunc:
        setattr(func, "__node_description__", description)
        return func

    return decorator


def get_node_description(func: NodeFunc) -> str:
    return getattr(func, "__node_description__", "")
