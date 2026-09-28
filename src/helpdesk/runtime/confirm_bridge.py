"""确认语义桥：把用户在「待确认」期间说的话翻成框架的事件。

框架给的是**机制**（bool + tool_call_id → 事件），不是自然语言判定：
`app/channel/_decision.py` 里的词表 {"allow","approve","accept"} / {"deny","reject"}
是钉钉**卡片按钮回调**用的，不是聊天里的一句话。用户在会话里回
「嗯嗢先这样吧」「别问了直接建单」「我还想提个别的问题」没人翻 ——
而 P0 探针③实测：park 期间塞裸 UserMsg 是 `ValueError`，不是被静默丢掉。
所以桥必须存在，且它的三条硬约束是设计出来的：

1. 每条线必须有归宿（approve / reject / restart），没有"丢掉"这个分支 —— Go 的 D2。
2. 判同意有三档，全是**显式**词表：整句归一化后等于确认词；句中出现推进短语且比否决
   短语先出现；句中有确认词且没有任何推迟/自述核实的说法。不对"确认"二字做无条件
   子串匹配 —— Go 的 D3（"先不建，等我确认下再说"曾被当成同意）。
3. 决策用的 tool_call 一律从 `state` 里读，不信任何回传值 —— agent 层不幂等。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable, Literal

from agentscope.agent import Agent
from agentscope.event import (
    ConfirmResult,
    RequireUserConfirmEvent,
    UserConfirmResultEvent,
    UserInterruptEvent,
)
from agentscope.message import UserMsg

from .textmatch import normalize

APPROVE = "approve"
REJECT = "reject"
NEW_REQUEST = "new_request"
PLAIN = "plain"

Decision = Literal["approve", "reject", "new_request", "plain"]

# 整句就是这些词（之一）才算点头。归一化后比较，所以标点、大小写、空格都不敏感。
_APPROVE_EXACT = frozenset(
    """
    确认 确定 确认建单 好的 好 行 可以 没问题 同意 批准 是 是的 对 嗯 嗯嗯
    建吧 建单吧 创建吧 提交吧 走吧 就这样 先这样吧 ok okay yes y sure
    """.split(),
)
_REJECT_EXACT = frozenset(
    """
    不 不建 别建 取消 算了 否 no deny reject
    """.split(),
)
# 句子更长时看线索：谁先出现谁定调，都判不出来就当新诉求。
_APPROVE_CUES = ("直接建单", "现在就建", "马上建", "尽快建", "确认建单", "先这样吧", "别问了")
_REJECT_CUES = ("先不建", "不要建", "别建", "不建", "先不用", "取消", "算了", "等等再说", "不同意", "拒绝")
#: 第三档：句子里点了头（而不只是整句等于一个确认词）。`normalize` 把标点全删了，
#: 所以"好的，确认，麻烦尽快"归一化成"好的确认麻烦尽快"，落在整句表和下述线索之外 ——
#: 轨迹金标里五条这样的批准原句被判成 new_request，park 直接拆掉、草案丢了。
#: 判不出来仍然 fail-closed，这一档只在"有确认词且没有任何推迟/自述核实的说法"时点头。
_CONFIRM_WORDS = ("确认", "确定", "同意", "批准", "没问题", "可以")
_DEFERRAL_WORDS = (
    "再说", "再说吧", "等等", "稍等", "等我", "我先", "先等", "看看再说",
    "先不", "先别", "还要", "还得", "不确定", "没确认",
)


def _earliest(text: str, cues: tuple[str, ...]) -> int:
    hits = [text.find(c) for c in cues if c in text]
    return min(hits) if hits else -1


def classify(text: str) -> Decision:
    """一句话 → 对**当前挂起的确认**的答复。判不出来一律走 new_request，不猜成同意。"""
    normalized = normalize(text)
    if not normalized:
        return NEW_REQUEST
    if normalized in _APPROVE_EXACT:
        return APPROVE
    if normalized in _REJECT_EXACT:
        return REJECT
    approve_at = _earliest(normalized, _APPROVE_CUES)
    reject_at = _earliest(normalized, _REJECT_CUES)
    if approve_at >= 0 and (reject_at < 0 or approve_at < reject_at):
        return APPROVE
    if reject_at >= 0:
        return REJECT
    if any(w in normalized for w in _CONFIRM_WORDS) and not any(w in normalized for w in _DEFERRAL_WORDS):
        return APPROVE
    return NEW_REQUEST


def asking_calls(agent: Agent) -> list[Any]:
    """ASKING 状态的调用只认 state 里的权威对象；重复/过期决策 agent 层会抛 ValueError。"""
    return [t for t in agent.state.get_awaiting_tool_calls(agent.name) if t.state == "asking"]


@dataclass(frozen=True)
class Delivered:
    kind: Decision
    pending: RequireUserConfirmEvent | None
    resolved: tuple[str, ...]


async def deliver(
    agent: Agent,
    text: str,
    on_event: Callable[[Any], None] | None = None,
    user_name: str = "user",
) -> Delivered:
    """喂进一句话。park 期间这句话一定变成事件，不会消失。"""
    pending: RequireUserConfirmEvent | None = None

    async def run(inputs: Any) -> RequireUserConfirmEvent | None:
        parked: RequireUserConfirmEvent | None = None
        async for event in agent.reply_stream(inputs):
            if on_event is not None:
                on_event(event)
            if isinstance(event, RequireUserConfirmEvent):
                parked = event
        return parked

    parked_calls = asking_calls(agent)
    if not parked_calls:
        return Delivered(PLAIN, await run(UserMsg(name=user_name, content=text)), ())

    decision = classify(text)
    reply_id = agent.state.reply_id
    if decision is NEW_REQUEST:
        # 拆掉 park，让那句话作为新一轮的开头进去；reply_id 会换。
        await run(UserInterruptEvent(reply_id=reply_id))
        return Delivered(
            NEW_REQUEST,
            await run(UserMsg(name=user_name, content=text)),
            tuple(t.id for t in parked_calls),
        )

    parked = await run(
        UserConfirmResultEvent(
            reply_id=reply_id,
            confirm_results=[
                ConfirmResult(confirmed=decision is APPROVE, tool_call=t)
                for t in parked_calls
            ],
        ),
    )
    return Delivered(
        decision,
        parked or _pending_of(agent),
        tuple(t.id for t in parked_calls),
    )


def _pending_of(agent: Agent) -> RequireUserConfirmEvent | None:
    """resume 之后仍然挂着调用时，用一个占位事件告诉驱动层"还要问"。

    框架只在**新**的 RequireUserConfirmEvent 上发事件，重复 park 同一批调用不会再发。
    """
    calls = asking_calls(agent)
    if not calls:
        return None
    return RequireUserConfirmEvent(reply_id=agent.state.reply_id, tool_calls=calls)


__all__ = [
    "APPROVE",
    "NEW_REQUEST",
    "PLAIN",
    "REJECT",
    "Delivered",
    "asking_calls",
    "classify",
    "deliver",
]
