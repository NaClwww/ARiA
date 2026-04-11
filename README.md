# Aria

一个最小的 LangGraph loop 示例。

现在这个版本直接调用 OpenAI 兼容 API，不依赖 `langchain-openai`。
为了兼容更多 OpenAI 风格网关，当前使用的是 `chat.completions` 接口。

运行：

```bash
export OPENAI_API_KEY=your_key
# 如果你用的是 OpenAI 兼容网关，再设置这个
# export OPENAI_BASE_URL=https://your-endpoint/v1

uv run python main.py
```

模型直接在代码里改：

```python
MODEL_NAME = "gpt-4o-mini"
```

运行后会先从终端读取一次用户输入，并把这段输入作为 LangGraph 的初始状态传入。

逻辑：

- 从 `START` 进入 `loop` 节点
- 每次执行把 `count + 1`
- 每次循环调用一次 OpenAI `chat.completions.create(...)`，并把工具定义传给模型
- 如果模型发起了 tool call，就执行工具，再继续进入下一轮 loop
- 如果模型没有发起 tool call，就直接结束
- 即使一直调用工具，达到 `max_count` 后也会结束
