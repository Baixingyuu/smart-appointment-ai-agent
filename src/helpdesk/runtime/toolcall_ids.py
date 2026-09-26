"""给 tool_call id 补唯一性：Ollama 适配器合成的 id 跨轮会撞车。

`OllamaChatModel` 用 `f"{idx}_{name}"` 当 id（`model/_ollama/_model.py:324`），只在一次
模型响应内唯一；而同一 reply 的所有块被 `append_context` 塞进同一条 assistant 消息
（`state/_state.py:310-316`），框架又拿"本条消息里已有的 tool_result id"判"这条调用
执行过了"（`state/_state.py:398-403`、`agent/_agent.py:3499-3507`）。于是**同一 reply
内第二次调同一个工具**（校验失败后改正重试、换关键词再检索一次）会被判成已完成，
连 tool_result 都不会有 —— 真机上表现为建单成功但指派静默丢失。

`probes/p3_tool_call_id_collision.py` 是这条的离线复现与修复后的回归锚。

在 `on_model_call` 上修，不在自研 model 子类里修：id 是适配器的事，重写它的解析
等于把 ollama 的流式累加再实现一遍。
"""
from __future__ import annotations

from itertools import count
from typing import Any, AsyncGenerator, Callable

from agentscope.message import ToolCallBlock
from agentscope.middleware import MiddlewareBase
from agentscope.model import ChatResponse


class UniqueToolCallIds(MiddlewareBase):
    """给每次模型调用的 tool_call id 加一个单调递增的命名空间前缀。"""

    def __init__(self) -> None:
        self._seq = count()

    async def on_model_call(
        self,
        agent: Any,
        input_kwargs: dict,
        next_handler: Callable,
    ) -> ChatResponse | AsyncGenerator[ChatResponse, None]:
        tag = f"m{next(self._seq)}"
        res = await next_handler()
        if isinstance(res, ChatResponse):
            _retag(res.content, tag)
            return res

        async def wrap() -> AsyncGenerator[ChatResponse, None]:
            """流式：增量块与最后那份汇总块都过一遍（同一 tag，所以仍然对齐）。"""
            async for chunk in res:
                _retag(chunk.content, tag)
                yield chunk

        return wrap()


def _retag(blocks: Any, tag: str) -> None:
    for block in blocks or []:
        if isinstance(block, ToolCallBlock) and not block.id.startswith(f"{tag}_"):
            block.id = f"{tag}_{block.id}"


__all__ = ["UniqueToolCallIds"]
