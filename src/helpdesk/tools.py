"""注册给 ReAct 的业务工具。

知识库检索不在这里：框架的 `RAGMiddleware(mode="agentic")` 白送一个 `search_knowledge`
工具，绑**只含知识语料**的那个 KnowledgeBase —— 服务字典不进去，否则派单用的 top-3
服务会污染答案检索。

派单 v4 的三源召回（服务 / 员工画像 / 已结历史）**反过来**放在 `match_service` 里固定
执行，不留给模型自愿调用。理由是一次实测：模糊问法的 kb_vague 场景里模型一次调用、
零工具直接反问（`docs/P3_AGENT.md`），而"跳过检索"不会报错，只会产出一条没有证据的
自信回答 —— 下游无从分辨。让必做的步骤长在工具里，比在提示词里请求它更便宜。

一处接缝证据：`is_state_injected=True` 的工具**不能**靠函数注解生成 schema。
`tool/_utils.py:113` 会把所有参数（含注入用的 `_agent_state`）建成 pydantic field，
而 pydantic 拒绝下划线开头的字段名（NameError: Fields must not use names with leading
underscores）。所以这些工具显式传 `input_schema=`。

另一处：本地工具的返回只认 `ToolChunk`。`tool/_adapters.py:179` 对其它返回一律
`str(result)` 兜底并写死 `state=RUNNING` —— 于是 `_error()` 的 ERROR 被抹平成
success、`metadata` 整包丢进字符串，评测侧读不到 ticket_id/missing_info，"指派被
在职闸拒绝"也不再是错误结果。看着最像对的 `ToolResponse` 恰恰是错的：它是框架
自己**汇总之后**的类型（`_response.py:50`），不是被调函数的返回类型。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Annotated, Any, Literal

from pydantic import BaseModel, Field

from agentscope.message import TextBlock, ToolResultState
from agentscope.permission import PermissionBehavior, PermissionDecision
from agentscope.tool import FunctionTool, ToolChunk

from .dispatch import (
    AssignmentDecision,
    DispatchEvidence,
    extract_services,
    recall_employees,
    recall_history,
    render_candidates,
    render_history,
    render_services,
)
from .domain import (
    MAX_ASK_ROUNDS,
    Category,
    ExtractedSlot,
    IntakeProgress,
    InvalidTransition,
    Priority,
    Ticket,
    TicketStatus,
    blocking_slots,
    slot_label,
)
from .knowledge import HelpdeskIndex
from .runtime import intake as intake_state
from .runtime.traceability import user_texts, verify_slots
from .ticket_store import TicketStore, UnknownTicket

_ALLOW = PermissionDecision(
    behavior=PermissionBehavior.ALLOW,
    message="只读查询，或用户已经确认过的写操作",
)
_ASK = PermissionDecision(behavior=PermissionBehavior.ASK, message="建单前要人确认")

SlotName = Literal[
    "affected_system",
    "asked_topic",
    "requested_action",
    "target_service",
    "planned_window",
]


@dataclass
class HelpdeskContext:
    """工具共享的业务事实；会话与 AgentState 的持久化归框架。"""

    index: HelpdeskIndex
    store: TicketStore = field(default_factory=TicketStore)
    last_hits: tuple = ()
    evidence: DispatchEvidence = field(default_factory=DispatchEvidence)


class SlotEvidence(BaseModel):
    name: SlotName = Field(description="要填的阻塞槽位")
    quote: str = Field(min_length=1, description="逐字摘自用户原话的证据，不要自己改写")


class CreateTicketInput(BaseModel):
    title: str = Field(min_length=1, description="简短标题")
    description: str = Field(min_length=1, description="现象描述，尽量保留用户原话")
    category: Literal["incident", "consultation", "request", "change"]
    priority: Literal["P0", "P1", "P2", "P3"]
    slots: Annotated[list[SlotEvidence], Field(default_factory=list)] = Field(
        description="已填的阻塞槽位及逐字证据",
    )


class AskUserInput(BaseModel):
    slots: Annotated[list[SlotName], Field(min_length=1)] = Field(
        description="这一轮要追问的阻塞槽位",
    )


class AssignTicketInput(AssignmentDecision):
    """指派入参：复用层 2 的结构化输出定义，枚举与评测轴共用一份。"""

    ticket_id: int = Field(description="create_ticket 返回的工单号")


def _text(text: str, metadata: dict[str, Any] | None = None) -> ToolChunk:
    return ToolChunk(
        content=[TextBlock(type="text", text=text)],
        metadata=metadata or {},
    )


def _error(text: str, metadata: dict[str, Any] | None = None) -> ToolChunk:
    return ToolChunk(
        content=[TextBlock(type="text", text=text)],
        metadata=metadata or {},
        state=ToolResultState.ERROR,
    )


async def _record_history(ctx: HelpdeskContext, ticket: Ticket) -> None:
    """把这张单写进历史集合（document_id=工单号，重复写即覆盖）。

    服务归属取本次 `match_service` 的 top-1 —— 它是系统当初怎么归类的记录，
    不是真值。未结的单由读取句柄的 `metadata_filter` 挡在召回之外，不靠调用方自觉。
    """
    top = ctx.last_hits[0] if ctx.last_hits else None
    await ctx.index.index_ticket(
        ticket_id=ticket.id,
        title=ticket.title,
        description=ticket.description,
        assignee_id=ticket.assignee_id,
        service_id=None if top is None else top.service_id,
        resolved=ticket.status is TicketStatus.DONE,
        category=ticket.category.value,
        priority=ticket.priority.value,
    )


def build_tools(ctx: HelpdeskContext) -> list[FunctionTool]:
    async def match_service(query: str) -> ToolChunk:
        """派单取数：一次给齐服务定位（top-3）、召回候选人、以及可参考的已结历史工单。

        Args:
            query (str): 完整句子，形如「标题。描述」
        """
        extraction = await extract_services(ctx.index, query)
        candidates = await recall_employees(ctx.index, query, extraction.hits)
        history = await recall_history(ctx.index, query)
        ctx.last_hits = extraction.hits
        ctx.evidence = DispatchEvidence(candidates=candidates, history=history)
        top = extraction.top1
        lines = [
            "【服务字典命中】",
            render_services(extraction.hits),
            "",
            "【召回候选人】",
            render_candidates(candidates),
            "",
            "【已结历史工单（仅作证据，不作投票）】",
            render_history(history),
            "",
            "相似度只说明「像不像」，不说明「能不能派」；判空由你在指派时填 "
            "ESCALATE_HUMAN 来表达，这里没有全局阈值可用。"
            "指派也可以选候选人之外的人（枚举是全名册），但那会记成召回漏检。",
        ]
        return _text(
            "\n".join(lines),
            {
                "services": [h.service_id for h in extraction.hits],
                "top1Score": top.score if top else None,
                "employeeCandidates": [c.id for c in candidates],
                "history": [c.ticket_id for c in history],
            },
        )

    def find_open_ticket(text: str) -> ToolChunk:
        """查重：看看是否已有描述相近的未关闭工单（字元覆盖率闸，不调模型）。

        Args:
            text (str): 待查的工单描述
        """
        duplicate = ctx.store.find_duplicate(text)
        if duplicate is None:
            return _text("没有相近的未关闭工单，可以继续建单。", {"duplicate": False})
        return _text(
            f"已有工单 #{duplicate.id}「{duplicate.title}」（{duplicate.category.value}，"
            f"{duplicate.status.value}）与这段描述重复。不要重复建单，"
            f"直接把这条上报作为进展追加到 #{duplicate.id}。",
            {"duplicate": True, "ticket_id": duplicate.id},
        )

    def ask_user(slots: list[str], _agent_state: Any = None) -> ToolChunk:
        """追问阻塞槽位。会按安全阀计数：到顶之后必须直接建单并记录缺失。

        Args:
            slots (list): 这一轮要问的槽位
        """
        progress = intake_state.load(_agent_state)
        allowed = progress.should_ask(tuple(slots))
        if not allowed:
            reason = (
                f"追问额度已用尽（{progress.ask_rounds}/{MAX_ASK_ROUNDS}）"
                if progress.ask_rounds >= MAX_ASK_ROUNDS
                else "这些槽位都已经问过了"
            )
            return _text(
                f"{reason}。不要再问，直接调用 create_ticket：缺的槽位会由系统写进 "
                f"missing_info，宁建缺信息工单也不丢单。",
                {"asked": [], "valve": "closed"},
            )
        asked = progress.ask(allowed)
        intake_state.save(_agent_state, progress)
        return _text(
            "请按下面的原话向用户提问，一次问清，不要复述已知信息：\n"
            + "\n".join(f"- {slot_label(s)}" for s in asked)
            + f"\n（本轮之后已追问 {progress.ask_rounds}/{MAX_ASK_ROUNDS} 轮）",
            {"asked": list(asked), "askRounds": progress.ask_rounds},
        )

    def create_ticket(
        title: str,
        description: str,
        category: str,
        priority: str,
        slots: list[dict[str, str]],
        _agent_state: Any = None,
    ) -> ToolChunk:
        """创建工单（需要用户确认后才会执行）。缺的阻塞槽位由系统记进 missing_info。

        Args:
            title (str): 简短标题
            description (str): 现象描述
            category (str): incident/consultation/request/change
            priority (str): P0/P1/P2/P3
            slots (list): 已填槽位，每项 {name, quote}，quote 必须逐字摘自用户原话
        """
        extracted = tuple(
            ExtractedSlot(name=s["name"], quote=s.get("quote", "")) for s in slots
        )
        verdicts, filled = verify_slots(extracted, user_texts(_agent_state))
        unverifiable = tuple(f"{v.slot}（引文「{v.quote}」在用户原话里查不到）" for v in verdicts if not v.accepted)
        kind = Category(category)
        missing = tuple(s for s in blocking_slots(kind) if s not in filled)
        result = ctx.store.create(
            title=title,
            description=description,
            category=kind,
            priority=Priority(priority),
            missing_info=missing,
            conversation_id=getattr(_agent_state, "session_id", None),
        )
        intake_state.save(_agent_state, IntakeProgress())
        if result.deduped:
            return _text(
                f"这段描述与已有工单 #{result.ticket.id} 重复，已作为进展追加，没有新建。",
                {"ticket_id": result.ticket.id, "deduped": True},
            )
        lines = [f"工单 #{result.ticket.id} 已创建（{kind.value}/{priority}）。"]
        if missing:
            lines.append("缺失的阻塞槽位已记入 missing_info：" + "、".join(missing))
        if unverifiable:
            lines.append(
                "这些槽位的引文没能在用户原话里查到，按未填处理：" + "；".join(unverifiable)
            )
        lines.append(f"下一步：调用 match_service 再用 assign_ticket 派单（工单号 {result.ticket.id}）。")
        return _text(
            "\n".join(lines),
            {
                "ticket_id": result.ticket.id,
                "deduped": False,
                "missing_info": list(missing),
                "unverified": [v.slot for v in verdicts if not v.accepted],
            },
        )

    async def assign_ticket(
        ticket_id: int,
        assignee: str,
        rationale: str,
        confidence: float,
    ) -> ToolChunk:
        """指派工单：assignee 只能是名册里的员工号或 ESCALATE_HUMAN。

        Args:
            ticket_id (int): 工单号
            assignee (str): 员工号或 ESCALATE_HUMAN
            rationale (str): 一句话理由；引用历史时写出「工单 N」
            confidence (float): 0~1
        """
        decision = AssignmentDecision(
            assignee=assignee,  # type: ignore[arg-type]
            rationale=rationale,
            confidence=confidence,
        )
        try:
            ticket = ctx.store.assign(ticket_id, decision, ctx.last_hits, ctx.evidence)
        except (UnknownTicket, InvalidTransition) as exc:
            return _error(f"指派被拒绝：{exc}", {"ticket_id": ticket_id, "refused": str(exc)})
        await _record_history(ctx, ticket)
        target = "转人工待认领" if decision.escalated else f"员工 {ticket.assignee_id}"
        cited = ctx.evidence.cites(decision.rationale)
        lines = [f"工单 #{ticket.id} → {target}。理由：{decision.rationale}"]
        if not ctx.evidence.recalled(decision.employee_id):
            lines.append(
                f"提醒：{decision.assignee} 不在刚才召回的候选人里（"
                f"{'、'.join(str(i) for i in ctx.evidence.candidate_ids)}），"
                f"本次指派已记为召回漏检，不拦截。",
            )
        if ctx.evidence.history and not cited:
            lines.append("提醒：召回到历史工单，但理由里没引用任何一条。")
        return _text(
            "\n".join(lines),
            {
                "ticket_id": ticket.id,
                "assignee": ticket.assignee_id,
                "escalated": decision.escalated,
                "candidates": [[h.service_id, round(h.score, 4)] for h in ctx.last_hits],
                "employeeCandidates": list(ctx.evidence.candidate_ids),
                "in_recall": ctx.evidence.recalled(decision.employee_id),
                "citedTickets": list(cited),
            },
        )

    return [
        FunctionTool(match_service, is_read_only=True, permission=_ALLOW),
        FunctionTool(find_open_ticket, is_read_only=True, permission=_ALLOW),
        FunctionTool(
            ask_user,
            is_state_injected=True,
            permission=_ALLOW,
            description="追问阻塞槽位，按安全阀计数；到顶后必须直接建单。",
            input_schema=AskUserInput,
        ),
        FunctionTool(
            create_ticket,
            is_state_injected=True,
            is_concurrency_safe=False,
            permission=_ASK,
            description="创建工单（写操作，必须用户确认）。",
            input_schema=CreateTicketInput,
        ),
        FunctionTool(
            assign_ticket,
            is_concurrency_safe=False,
            permission=_ALLOW,
            description="把工单指派给唯一负责人，或填 ESCALATE_HUMAN 转人工。",
            input_schema=AssignTicketInput,
        ),
    ]
