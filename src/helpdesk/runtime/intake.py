"""追问进度：跨轮记住"问过哪些槽位、还允许问几轮"，并把状态回灌给模型。

框架里没有任何跨回合机制阻止无限追问，而这条对应硬承诺「宁建缺信息工单也不丢单」，
所以计数留在自研；措辞仍然由模型自己组织。本模块两个中间件：
`IntakeInjectionMiddleware`（进度回灌）与 `AskOutLoudMiddleware`（追问必须说出口，
真机实测 qwen3:8b 会 ask_user 完接着在同一条 reply 里建单，用户一个字都没被问到）。

为什么走 on_system_prompt 而不是方案里写的 InjectionConfig.extra_fields（读源码改的决定）：
`_agent.py:1606` 的分支是 `if injections:` —— extra_fields 只在**已经有别的注入触发**
（首次时间注入、任务状态、连续工具失败）时才被附上，自己**不会**触发注入。
时间注入默认 0.5 小时一次，所以进度在多数轮次里根本不会进上下文。
`on_system_prompt` 每次模型调用都跑（`_get_system_prompt` 由 `_prepare_model_input` 调用），
是确定性的接缝，代价是这段文字进的是 system 消息而不是累积的 hint 块。
"""
from __future__ import annotations

from typing import Any, Awaitable, Callable

from agentscope.message import ToolResultBlock
from agentscope.middleware import MiddlewareBase
from agentscope.tool import ToolChoice

from ..domain import MAX_ASK_ROUNDS, IntakeProgress

MIDDLE_KEY = "intake"


def load(state: object) -> IntakeProgress:
    raw = getattr(state, "middle_context", {}).get(MIDDLE_KEY) or {}
    return IntakeProgress(
        asked_slots=list(raw.get("asked_slots", [])),
        ask_rounds=int(raw.get("ask_rounds", 0)),
    )


def save(state: object, progress: IntakeProgress) -> None:
    """存 dict 而不是 dataclass：AgentState 要能被 model_dump 序列化。"""
    state.middle_context[MIDDLE_KEY] = {  # type: ignore[attr-defined]
        "asked_slots": list(progress.asked_slots),
        "ask_rounds": progress.ask_rounds,
    }


def render(progress: IntakeProgress) -> str:
    asked = "、".join(progress.asked_slots) or "无"
    left = max(0, MAX_ASK_ROUNDS - progress.ask_rounds)
    return (
        f"已追问 {progress.ask_rounds}/{MAX_ASK_ROUNDS} 轮（还能追问 {left} 轮）；"
        f"已问过的槽位：{asked}。"
        "若阻塞槽位仍缺且还有追问额度，就问还没问过的那个；"
        "额度用尽时必须停止追问，直接建单并把缺的槽位记进 missing_info——不能丢单。"
        "同一个槽位不要重复问。"
    )


class IntakeInjectionMiddleware(MiddlewareBase):
    """把追问进度追加进系统提示，每轮都是当前值。"""

    async def on_system_prompt(self, agent: object, current_prompt: str) -> str:
        progress = load(agent.state)  # type: ignore[attr-defined]
        if not progress.asked_slots and progress.ask_rounds == 0:
            return current_prompt
        return f"{current_prompt}\n\n<intake-progress>\n{render(progress)}\n</intake-progress>"


def _awaiting_question(agent: object) -> bool:
    """本条 reply 的最后一个块是不是 ask_user 的结果（= 问题还没说出口）。

    用状态判定而不是加标志位：工具结果与工具调用落在同一条 assistant 消息里
    （`state/_state.py:310`），尾部块就是最近一次执行完的那个。
    """
    context = agent.state.context  # type: ignore[attr-defined]
    if not context:
        return False
    blocks = context[-1].get_content_blocks()
    if not blocks or context[-1].role != "assistant":
        return False
    last = blocks[-1]
    return isinstance(last, ToolResultBlock) and last.name == "ask_user"


class AskOutLoudMiddleware(MiddlewareBase):
    """ask_user 之后紧跟着的那次模型调用不给任何工具，逼它把问题讲给人听。

    真机实测：qwen3:8b 会在一轮里连调 ask_user ×2 然后直接 create_ticket，
    用户一个字都没被问到，下一句话就被桥判成 new_request 并把建单取消。

    框架给的缝是 `ToolChoice(mode="none")`（`tool/_types.py:185`），但**它在 Ollama
    这条路上是空转的**：`model/_ollama/_model.py:223` 打一句
    "Ollama ignores tool_choice.mode" 就把 mode 丢了，实跑里模型照旧建单。
    所以这里直接抽掉那一次调用的 schema 列表（`tools=[]`），provider 支不支持
    tool_choice 都不再影响结果；`tool_choice=none` 仍然附上，让支持的 provider
    拿到明确语义。
    """

    async def on_model_call(
        self,
        agent: object,
        input_kwargs: dict,
        next_handler: Callable[..., Awaitable[Any]],
    ) -> Any:
        if _awaiting_question(agent):
            return await next_handler(tools=[], tool_choice=ToolChoice(mode="none"))
        return await next_handler()


__all__ = [
    "AskOutLoudMiddleware",
    "IntakeInjectionMiddleware",
    "MIDDLE_KEY",
    "load",
    "render",
    "save",
]
