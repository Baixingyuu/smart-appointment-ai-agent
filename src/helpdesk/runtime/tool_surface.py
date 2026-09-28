"""模型可见工具面：把 schema 列表收窄回业务那一组。

两条路径的差异只有这一件事是"框架给的"：console 路径的 Toolkit 是我们自己装的
（`agent_factory.make_agent`），托管路径每轮由 `app/_service/_toolkit.py` 重组，
除我们的 extras 之外还附送 workspace builtin（Bash/Edit/Glob/Grep/Read/Write）、
计划四件套、ToolStop、团队与调度工具。真机实测（P5 探针 B 段）同一句话、temp=0：
console 一步到 create_ticket 并停在待确认；托管路径连调 8 次 search_knowledge
把 max_iters 烧完，以 RUN_ERROR(exceed_max_iters) 收场，业务工具一个没碰。

所以这里不是"防模型手滑"的兜底，而是把工具面这个自变量重新钉住：
浏览器驱动的 agent 拿到 Bash/Write 本身也不该发生。它筛的是"递给模型什么 schema"，
拦不住模型凭训练记忆喊出一个同名调用 —— 要真的禁掉某个动作，得再配工具自己的
`permission=DENY`，见 `MODEL_INVISIBLE_TOOLS`。

缝用 `on_model_call` 直接改 `tools`，不用 `tool_choice.tools`：后者在 Ollama 这条
路上只做 schema 过滤且 mode 被丢（`model/_ollama/_model.py:193`），既然要的是
"模型看不到"，就按 `AskOutLoudMiddleware` 同样的办法抽列表。
"""
from __future__ import annotations

from typing import Any, Awaitable, Callable

from agentscope.middleware import MiddlewareBase

from .specialists import SPECIALIST_TOOLS

BUSINESS_TOOLS = (
    "ask_user",
    "create_ticket",
    "assign_ticket",
    "suggest_assignment",
    "find_open_ticket",
    "match_service",
    # P6-b 上门预约：查时段是只读的，落约与改期走确认闸，登记回访记的是已发生的事。
    "propose_appointments",
    "book_appointment",
    "reschedule_appointment",
    "close_appointment",
    "upcoming_appointments",
    # 长期记忆的两个口：读偏好是业务件；`run_autodream` 只在挂了落盘事实源的上下文里
    # 才注册（`tools.build_tools` 末尾），但白名单必须事先有它 —— 漏了的话调度任务
    # 醒来看不到这个工具，整条 AutoDream 链就变成了"模型想调却调不着"。
    "recall_preferences",
    "run_autodream",
)
#: RAGMiddleware 在 agentic 模式下自己注册的那一个。
RETRIEVAL_TOOL = "search_knowledge"
#: 注册着、但不递交给模型的业务件。
#:
#: 口径已从"指派由人在系统外做"改成"助手推荐候选、用户选定后经确认闸落指派"，
#: 所以这份名单现在是空的。它留着占位：哪天要再禁掉一个动作（比如越权直派），
#: 把工具名加进来 + 工具侧配 `permission=_DENY`，两道封就都回来了。
MODEL_INVISIBLE_TOOLS: frozenset[str] = frozenset()
#: P6-a③ 的专业子 Agent。只有 `SPECIALISTS_ENABLED` 时才真的注册进 Toolkit；
#: 白名单里多一个没注册的名字不会多出任何事，反过来漏了则专线会被这道闸摘掉，
#: 于是"拆编排"在线上的表现变成"模型突然看不见那几个工具"。
MODEL_VISIBLE_TOOLS = (*BUSINESS_TOOLS, RETRIEVAL_TOOL, *SPECIALIST_TOOLS)


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
    "MODEL_INVISIBLE_TOOLS",
    "MODEL_VISIBLE_TOOLS",
    "ToolSurfaceMiddleware",
]
