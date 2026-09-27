"""模型可见工具面：把 schema 列表收窄回业务六件套。

两条路径的差异只有这一件事是"框架给的"：console 路径的 Toolkit 是我们自己装的
（`agent_factory.make_agent`），托管路径每轮由 `app/_service/_toolkit.py` 重组，
除我们的 extras 之外还附送 workspace builtin（Bash/Edit/Glob/Grep/Read/Write）、
计划四件套、ToolStop、团队与调度工具。真机实测（P5 探针 B 段）同一句话、temp=0：
console 一步到 create_ticket 并停在待确认；托管路径连调 8 次 search_knowledge
把 max_iters 烧完，以 RUN_ERROR(exceed_max_iters) 收场，业务工具一个没碰。

所以这里不是"防模型手滑"的兜底，而是把工具面这个自变量重新钉住：
浏览器驱动的 agent 拿到 Bash/Write 本身也不该发生。

缝用 `on_model_call` 直接改 `tools`，不用 `tool_choice.tools`：后者在 Ollama 这条
路上只做 schema 过滤且 mode 被丢（`model/_ollama/_model.py:193`），既然要的是
"模型看不到"，就按 `AskOutLoudMiddleware` 同样的办法抽列表。
"""
from __future__ import annotations

from typing import Any, Awaitable, Callable

from agentscope.middleware import MiddlewareBase

BUSINESS_TOOLS = (
    "ask_user",
    "create_ticket",
    "find_open_ticket",
    "match_service",
    "assign_ticket",
)
#: RAGMiddleware 在 agentic 模式下自己注册的那一个。
RETRIEVAL_TOOL = "search_knowledge"
MODEL_VISIBLE_TOOLS = (*BUSINESS_TOOLS, RETRIEVAL_TOOL)


def _schema_name(schema: dict) -> str:
    """OpenAI 形制的 schema 取名（`_ollama/_model.py:218` 同一形状）。"""
    return str(schema.get("function", {}).get("name") or schema.get("name") or "")


class ToolSurfaceMiddleware(MiddlewareBase):
    """每次模型调用只递交白名单内的 schema，并记下被摘掉的名字供对账。"""

    def __init__(self, allowed: tuple[str, ...] = MODEL_VISIBLE_TOOLS) -> None:
        self._allowed = set(allowed)
        self.dropped: set[str] = set()

    async def on_model_call(
        self,
        agent: object,
        input_kwargs: dict,
        next_handler: Callable[..., Awaitable[Any]],
    ) -> Any:
        tools = input_kwargs.get("tools")
        if not tools:
            # 上游（AskOutLoudMiddleware）已经清空了这一次调用的工具。
            return await next_handler()
        kept = [t for t in tools if _schema_name(t) in self._allowed]
        self.dropped.update(_schema_name(t) for t in tools if _schema_name(t) not in self._allowed)
        return await next_handler(tools=kept)


__all__ = [
    "BUSINESS_TOOLS",
    "MODEL_VISIBLE_TOOLS",
    "ToolSurfaceMiddleware",
]
