import os

from langchain_openai import ChatOpenAI


MODEL_NAME = "qwen/qwen3-next-80b-a3b-instruct"
MAX_CONTEXT_MESSAGES = 12


class OpenAIClient:
    def __init__(self, tools: list[dict]):
        api_key = os.getenv("OPENAI_API_KEY")
        base_url = os.getenv("OPENAI_BASE_URL")
        if not api_key:
            raise RuntimeError("Missing OPENAI_API_KEY")

        client_kwargs: dict[str, str | int] = {
            "api_key": api_key,
            "model": MODEL_NAME,
            "temperature": 0,
        }
        if base_url:
            client_kwargs["base_url"] = base_url

        self.model = ChatOpenAI(**client_kwargs).bind_tools(tools)

    def __call__(self, messages: list):
        response = self.model.invoke(messages[-MAX_CONTEXT_MESSAGES:])
        print("\n[debug] model content:", repr(response.content))
        print("[debug] model tool_calls:", response.tool_calls)
        return response
