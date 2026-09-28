"""意图路由：分类结果 → 工具面裁剪。

这就是"多 agent 分派"在本框架的落地形态：每个意图一套工具面（咨询只给
search_knowledge、故障给全量写工具、转人工/闲聊/越界给空工具面）。效果上等价于
"路由到专业 subagent"，实现上是同一 session 内按意图切换 —— 因为真正的多 agent
（SubAgentTemplate 或 FunctionTool 包子 agent）在写操作上会 HITL 死锁
（`specialists.py` 第一条约束）。

只裁工具面、不注入流程提示。真机实测：把"先 create_ticket 建单 → match_service"
这类**行动指令**写进 system prompt 并每轮注入，模型会在"确认建单"的后续轮里又看见
"create_ticket"，误以为要重新建单、凭空幻觉出一张新工单；只注入首轮又让确认轮丢
锚点。流程引导交给 SYSTEM_PROMPT + 工具结果（`create_ticket` 的返回、`match_service`
的【推荐】块），意图只决定"手上有什么工具"，不做行动指令。

一条关键的连续性规则：意图只在"新诉求"出现时重新分类。追问进行中（ask_rounds>0）或
有未指派的在建工单时，用户的话是流程内消息（回答追问、选定工程师、补充信息），延续
上一轮意图、不重分类 —— 否则"选 101"会被误判成 chitchat、工具面被收窄、指派链路
直接断掉。这层判断是启发式的，边界见模块末尾。
"""
from __future__ import annotations

import logging
import os
from typing import Any, AsyncGenerator, Awaitable, Callable

from agentscope.event import ExternalExecutionResultEvent, UserConfirmResultEvent
from agentscope.middleware import MiddlewareBase

from ..intent import classify_intent
from ..ticket_store import TicketStore
from .tool_surface import _schema_name

logger = logging.getLogger(__name__)

INTENT_KEY = "intent"

#: 各意图允许的工具名。None = 不裁剪（原样）。空集 = 清空工具面（纯文本回应）。
INTENT_ALLOWED_TOOLS: dict[str, frozenset[str] | None] = {
    "knowledge": frozenset({"search_knowledge"}),
    "incident": None,
    "handoff": frozenset(),
    "chitchat": frozenset(),
    "out_of_scope": frozenset(),
}


def _extract_user_text(inputs: Any) -> str | None:
    """从 on_reply 的 inputs 里取新用户消息的文本；确认恢复/外部结果返回 None。"""
    if inputs is None:
        return None
    for m in inputs if isinstance(inputs, list) else [inputs]:
        if isinstance(m, (UserConfirmResultEvent, ExternalExecutionResultEvent)):
            return None
        if getattr(m, "role", None) == "user":
            text = (m.get_text_content() or "").strip()
            if text:
                return text
    return None


class IntentRouterMiddleware(MiddlewareBase):
    """每条新诉求先分类，再按意图裁工具面。"""

    def __init__(self, store: TicketStore | None = None) -> None:
        self._store = store
        #: 一键关掉整条路由（对照实验用）：分类/裁剪全部透传，回到无路由的现状。
        self._enabled = os.environ.get("HELPDESK_INTENT_ROUTER", "1") != "0"

    def _in_active_flow(self, agent: object) -> bool:
        """是否处于"已定意图、流程进行中"：追问中 / 有未指派的在建工单。

        这两种情况下的用户消息是流程内消息（回答追问、选定工程师），不该重分类。
        """
        intake_raw = getattr(agent.state, "middle_context", {}).get("intake") or {}  # type: ignore[attr-defined]
        if intake_raw.get("ask_rounds", 0) > 0:
            return True
        if self._store is not None:
            if any(t.assignee_id is None for t in self._store.open_tickets()):
                return True
        return False

    async def on_reply(self, agent: object, input_kwargs: dict, next_handler: Callable[..., AsyncGenerator]) -> AsyncGenerator:
        if not self._enabled:
            async for event in next_handler():
                yield event
            return
        inputs = input_kwargs.get("inputs")
        text = _extract_user_text(inputs)
        logger.warning("[IntentRouter] inputs=%s text=%r", type(inputs).__name__, (text or "")[:40])
        if text is not None and not self._in_active_flow(agent):
            try:
                intent = await classify_intent(agent.model, text)  # type: ignore[attr-defined]
                agent.state.middle_context[INTENT_KEY] = intent.model_dump()  # type: ignore[attr-defined]
                logger.warning(
                    "意图路由：%s（置信度 %.2f）← %s",
                    intent.intent,
                    intent.confidence,
                    text[:50],
                )
            except Exception:  # noqa: BLE001
                # 分类失败降级为不路由（保持原工具面），别让一次分类把整轮对话带崩。
                logger.warning("意图分类失败，降级为不路由", exc_info=True)
                getattr(agent.state, "middle_context", {}).pop(INTENT_KEY, None)  # type: ignore[attr-defined]
        async for event in next_handler():
            yield event

    async def on_model_call(self, agent: object, input_kwargs: dict, next_handler: Callable[..., Awaitable[Any]]) -> Any:
        if not self._enabled:
            return await next_handler()
        raw = getattr(agent.state, "middle_context", {}).get(INTENT_KEY)  # type: ignore[attr-defined]
        tools = input_kwargs.get("tools")
        if not tools:
            return await next_handler()
        narrowed = False
        # 硬约束：工单簿里已有未指派的在建工单，就把工具面收窄到"推荐 + 预约"这一组。
        # 8B 模型在 HITL 确认恢复后会重新走"建单"流程（真机实测：确认后 create_ticket
        # 又 park 出一张新工单）、或在 match_service 后直接文本收尾（不弹卡片），提示词
        # 拦不住，只能从工具面物理收窄：只留能推进推荐/预约的工具。
        if self._store is not None and any(t.assignee_id is None for t in self._store.open_tickets()):
            keep = {"match_service", "suggest_assignment", "propose_appointments", "book_appointment", "reschedule_appointment", "close_appointment", "upcoming_appointments", "recall_preferences"}
            tools = [t for t in tools if _schema_name(t) in keep]
            narrowed = True
        if raw:
            allowed = INTENT_ALLOWED_TOOLS.get(raw.get("intent", ""))
            if allowed is not None:
                tools = [t for t in tools if _schema_name(t) in allowed]
                narrowed = True
        return await next_handler(tools=tools) if narrowed else await next_handler()


__all__ = [
    "INTENT_ALLOWED_TOOLS",
    "INTENT_KEY",
    "IntentRouterMiddleware",
]
