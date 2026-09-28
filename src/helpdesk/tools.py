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

import logging
import re
from dataclasses import dataclass, field
from datetime import datetime
from typing import Annotated, Any, Literal

logger = logging.getLogger(__name__)

from pydantic import BaseModel, Field

from agentscope.message import TextBlock, ToolResultState
from agentscope.permission import PermissionBehavior, PermissionDecision
from agentscope.tool import FunctionTool, ToolChunk

from . import autodream
from .appointment import (
    BLOCKING_SLOTS as APPT_BLOCKING,
    AppointmentStore,
    InvalidBooking,
    booking_prompt,
    render_appointment,
)
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
from .ledger import Ledger
from .memory import MemoryStore
from .runtime import intake as intake_state
from .runtime.traceability import user_texts, verify_slots
from .ticket_store import TicketStore, UnknownTicket

_ALLOW = PermissionDecision(
    behavior=PermissionBehavior.ALLOW,
    message="只读查询，或用户已经确认过的写操作",
)
_ASK = PermissionDecision(behavior=PermissionBehavior.ASK, message="写操作前要用户确认")

SlotName = Literal[
    "affected_system",
    "asked_topic",
    "requested_action",
    "target_service",
    "planned_window",
    # 预约的三个槽位。复用同一个 SlotEvidence：逐字证据这套闸不该有两份实现。
    "visit_time",
    "visit_duration",
    "service_preference",
]


@dataclass
class HelpdeskContext:
    """工具共享的业务事实；会话与 AgentState 的持久化归框架。"""

    index: HelpdeskIndex
    store: TicketStore = field(default_factory=TicketStore)
    appointments: AppointmentStore = field(default_factory=AppointmentStore)
    last_hits: tuple = ()
    evidence: DispatchEvidence = field(default_factory=DispatchEvidence)
    #: 跨会话的两件套：原始事实（日志）与折出来的结论（偏好表）。
    #: 默认 None = 这份上下文不落盘（console、单测、评测都是）：`run_autodream` 干脆
    #: 不注册（见 `build_tools` 末尾），`recall_preferences` 留着但如实说"没有表可读"。
    ledger: Ledger | None = None
    memory: MemoryStore | None = None


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


class Selection(BaseModel):
    """用户在卡片上的选择：前端 resolve 通过 AG-UI payload 回填，模型不要填。"""

    engineer_id: str = Field(description="用户选定的员工号")
    time_slot: str | None = Field(default=None, description="用户选定的上门时段 'YYYY-MM-DD HH:MM'，没选则为空")


class SuggestAssignmentInput(BaseModel):
    """推荐候选 + 可选上门时段，弹卡片让用户点选。

    candidate_ids 是员工号列表，候选的姓名和理由由系统从最近一次 match_service 的
    召回里补齐 —— 模型只需抄员工号，不需要自己写画像/理由。
    """

    ticket_id: int = Field(description="create_ticket 返回的工单号")
    candidate_ids: Annotated[list[str], Field(min_length=1, max_length=4)] = Field(
        description="推荐的 2-3 个候选的员工号，从 match_service 召回候选人里抄",
    )
    time_slots: list[str] = Field(
        default_factory=list,
        description="可选的上门时段（逐字抄 propose_appointments 返回的时刻），不需要上门就留空",
    )
    need: str = Field(default="", description="上门要解决什么，选上门时填")
    duration_minutes: Literal[30, 60, 120] = Field(default=60, description="上门时长")
    confidence: float = Field(default=0.8, ge=0.0, le=1.0, description="推荐置信度")
    selection: Selection | None = Field(default=None, description="用户的选择，前端回填，模型不要填")


class ProposeVisitInput(BaseModel):
    need: str = Field(min_length=1, description="这次上门要解决什么，尽量保留用户原话")
    earliest: str = Field(description="用户能接受的最早时间，本地时刻 'YYYY-MM-DD HH:MM'")
    latest: str = Field(description="用户能接受的最晚时间，本地时刻 'YYYY-MM-DD HH:MM'")
    duration_minutes: Literal[30, 60, 120] = Field(default=60, description="预计上门时长，只有 30/60/120 三档")
    service_id: int | None = Field(default=None, description="match_service 命中的服务号，没有就留空")
    engineer_ids: Annotated[list[int], Field(default_factory=list)] = Field(
        description="用户点名要的人的员工号，没有就留空",
    )


class BookAppointmentInput(BaseModel):
    engineer_id: int = Field(description="时段里给出的员工号")
    start: str = Field(description="上门开始时间，本地时刻 'YYYY-MM-DD HH:MM'")
    duration_minutes: Literal[30, 60, 120]
    need: str = Field(min_length=1, description="这次上门要做什么")
    service_id: int | None = None
    ticket_id: int | None = Field(default=None, description="若这次上门是为某张已有工单，填工单号")
    slots: Annotated[list[SlotEvidence], Field(default_factory=list)] = Field(
        default_factory=list,
        description="已填的预约槽位及逐字证据（visit_time / visit_duration / service_preference）",
    )


class RescheduleInput(BaseModel):
    appointment_id: int
    start: str = Field(description="新的开始时间，本地时刻 'YYYY-MM-DD HH:MM'")
    duration_minutes: Literal[30, 60, 120] | None = None
    engineer_id: int | None = Field(default=None, description="换人时才填")


class CloseVisitInput(BaseModel):
    appointment_id: int
    resolved: bool = Field(description="这一趟上门有没有把问题解掉")
    followup_note: str = Field(min_length=2, description="回访记录，一句话即可")


_TIME_FORMATS = ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d %H:%M", "%Y-%m-%d %H:%M:%S.%f")


def _parse_time(value: str, label: str) -> datetime:
    """模型给的时间总是带方言：ISO 的 T、秒、+08:00 后缀。

    这里只剥掉时区后缀、不做换算 —— 上门排班比的是本地墙钟，悄悄换算会把人挪八小时。
    """
    text = value.strip().replace("T", " ")
    text = re.split(r"(?:Z|[+-]\d{2}:?\d{2})$", text)[0].strip()
    for fmt in _TIME_FORMATS:
        try:
            return datetime.strptime(text, fmt)
        except ValueError:
            continue
    raise InvalidBooking(
        f"{label} 要写成 'YYYY-MM-DD HH:MM' 这种本地时刻（不带时区后缀），收到的是「{value}」"
    )


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
        """推荐取数：一次给齐服务定位（top-3）、召回候选人、以及可参考的已结历史工单。

        Args:
            query (str): 完整句子，形如「标题。描述」
        """
        extraction = await extract_services(ctx.index, query)
        candidates = await recall_employees(
            ctx.index,
            query,
            extraction.hits,
            roster=ctx.appointments.staff(),
        )
        history = await recall_history(ctx.index, query)
        ctx.last_hits = extraction.hits
        ctx.evidence = DispatchEvidence(candidates=candidates, history=history)
        logger.warning(
            "[工具] match_service 执行：services=%s candidates=%s",
            [h.service_id for h in extraction.hits],
            [c.id for c in candidates],
        )
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
            "相似度只说明「像不像」，不说明「该不该推荐」；这里没有全局阈值可用，"
            "候选人为空就照实说没有合适的工程师、建议转人工认领。"
            "推荐也可以落在候选人之外（枚举是全名册），但那会记成召回漏检。",
            "",
            "下一步：从【召回候选人】里挑 2-3 个（每个带一句理由），"
            "直接调 suggest_assignment 把候选弹成卡片让用户点选，不要只念成文字就停。",
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
        logger.warning("[工具] ask_user 执行：slots=%s", asked)
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
        logger.warning(
            "[工具] create_ticket 执行：ticket=#%s deduped=%s missing=%s",
            result.ticket.id,
            result.deduped,
            missing,
        )
        if result.deduped:
            return _text(
                f"这段描述与已有工单 #{result.ticket.id} 重复，已作为进展追加，没有新建。",
                {"ticket_id": result.ticket.id, "deduped": True},
            )
        lines = [
            f"工单 #{result.ticket.id} 已创建（{kind.value}/{priority}）。"
            "系统提醒：工单已经建好，不要再调 create_ticket。"
        ]
        if missing:
            lines.append("缺失的阻塞槽位已记入 missing_info：" + "、".join(missing))
        if unverifiable:
            lines.append(
                "这些槽位的引文没能在用户原话里查到，按未填处理：" + "；".join(unverifiable)
            )
        lines.append(
            f"下一步只做一件事：调 match_service 拿推荐材料，"
            f"再调 suggest_assignment 把候选弹成卡片让用户点选"
            f"（工单号 {result.ticket.id}）。需要上门时先 propose_appointments 拿时段一并填进卡片。"
        )
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

    async def suggest_assignment(
        ticket_id: int,
        candidate_ids: list[str],
        time_slots: list[str] = [],
        need: str = "",
        duration_minutes: int = 60,
        confidence: float = 0.8,
        selection: Selection | None = None,
        _agent_state: Any = None,
    ) -> ToolChunk:
        """推荐候选 + 可选上门时段，弹卡片让用户点选；用户选定后落指派 + 预约。

        这是写操作（会 park 成卡片等用户点选）：卡片渲染 candidate_ids 和 time_slots，
        用户点选后前端通过 AG-UI payload 把 selection 回填进来，本函数据 selection 落
        assign_ticket + book_appointment。模型只负责从 match_service 召回里抄员工号填
        candidate_ids，不负责替用户选 —— selection 永远来自用户点选。
        """
        sel = selection if isinstance(selection, Selection) else (
            Selection(**selection) if isinstance(selection, dict) else None
        )
        if sel is None or not sel.engineer_id:
            return _error("没有收到用户的选择", {"refused": "no_selection", "ticket_id": ticket_id})
        engineer_id = sel.engineer_id
        time_slot = sel.time_slot
        by_id = {str(c.id): c for c in ctx.evidence.candidates}
        chosen = by_id.get(engineer_id)
        rationale = f"{chosen.employee.name}：{'、'.join(chosen.reasons)}" if chosen else "用户选定"

        decision = AssignmentDecision(
            assignee=engineer_id,  # type: ignore[arg-type]
            rationale=rationale,
            confidence=confidence,
        )
        try:
            ticket = ctx.store.assign(ticket_id, decision, ctx.last_hits, ctx.evidence)
        except (UnknownTicket, InvalidTransition) as exc:
            return _error(f"指派被拒绝：{exc}", {"ticket_id": ticket_id, "refused": str(exc)})
        await _record_history(ctx, ticket)

        lines = [f"工单 #{ticket.id} → 员工 {ticket.assignee_id}。理由：{rationale}"]
        if not ctx.evidence.recalled(decision.employee_id):
            lines.append(
                f"提醒：{decision.assignee} 不在召回候选人里（"
                f"{'、'.join(str(i) for i in ctx.evidence.candidate_ids)}），记为召回漏检。",
            )

        if time_slot:
            try:
                moment = _parse_time(time_slot, "start")
                appt = ctx.appointments.book(
                    engineer_id=ticket.assignee_id,
                    start=moment,
                    duration_minutes=duration_minutes,
                    need=need,
                    service_id=None,
                    ticket_id=ticket_id,
                    conversation_id=getattr(_agent_state, "session_id", None),
                    missing_info=(),
                    now=datetime.now(),
                )
                lines.append(render_appointment(appt))
            except InvalidBooking as exc:
                lines.append(f"上门预约没落成：{exc}")

        return _text(
            "\n".join(lines),
            {
                "ticket_id": ticket.id,
                "assignee": ticket.assignee_id,
                "time_slot": time_slot,
                "in_recall": ctx.evidence.recalled(decision.employee_id),
            },
        )

    def propose_appointments(
        need: str,
        earliest: str,
        latest: str,
        duration_minutes: int = 60,
        service_id: int | None = None,
        engineer_ids: list[int] | None = None,
    ) -> ToolChunk:
        try:
            low = _parse_time(earliest, "earliest")
            high = _parse_time(latest, "latest")
            openings = ctx.appointments.propose(
                need=need,
                earliest=low,
                latest=high,
                duration_minutes=duration_minutes,
                service_id=service_id,
                engineer_ids=tuple(engineer_ids or ()),
                now=datetime.now(),
            )
        except InvalidBooking as exc:
            return _error(f"查可约时段失败：{exc}", {"openings": []})
        if not openings:
            return _text(
                "这个窗口里没人能上门。可行的三条路：把时间窗放宽（换一天或放宽到全天）、"
                "改成远程处理（先建单派人，不约上门）、或明确告诉用户暂时约不出来。"
                "不要自己编一个时段出来。",
                {"openings": [], "empty": True},
            )
        return _text(
            booking_prompt(
                need=need,
                earliest=low,
                latest=high,
                duration_minutes=duration_minutes,
                openings=openings,
            ),
            {
                "openings": [
                    [o.engineer.id, f"{o.start:%Y-%m-%d %H:%M}", o.duration_minutes] for o in openings
                ],
                "empty": False,
            },
        )

    def book_appointment(
        engineer_id: int,
        start: str,
        duration_minutes: int,
        need: str,
        service_id: int | None = None,
        ticket_id: int | None = None,
        slots: list[dict[str, str]] | None = None,
        _agent_state: Any = None,
    ) -> ToolChunk:
        try:
            moment = _parse_time(start, "start")
        except InvalidBooking as exc:
            return _error(f"预约没落成：{exc}", {"refused": str(exc)})
        repeat = ctx.appointments.same_slot(engineer_id, moment, duration_minutes) is not None
        extracted = tuple(
            ExtractedSlot(name=s["name"], quote=s.get("quote", "")) for s in (slots or [])
        )
        verdicts, filled = verify_slots(extracted, user_texts(_agent_state))
        unverifiable = tuple(
            f"{v.slot}（引文「{v.quote}」在用户原话里查不到）" for v in verdicts if not v.accepted
        )
        missing = tuple(s for s in APPT_BLOCKING if s not in filled)
        try:
            appt = ctx.appointments.book(
                engineer_id=engineer_id,
                start=moment,
                duration_minutes=duration_minutes,
                need=need,
                service_id=service_id,
                ticket_id=ticket_id,
                conversation_id=getattr(_agent_state, "session_id", None),
                missing_info=missing,
                now=datetime.now(),
            )
        except InvalidBooking as exc:
            return _error(f"预约没落成：{exc}", {"refused": str(exc)})
        lines = [render_appointment(appt)]
        if repeat:
            lines.append("这条之前已经约过了，没有二次占用，返回的是同一条预约。")
        if missing:
            lines.append("缺失的预约槽位已记进 missing_info：" + "、".join(missing))
        if unverifiable:
            lines.append("这些槽位的引文没能在用户原话里查到，按未填处理：" + "；".join(unverifiable))
        return _text(
            "\n".join(lines),
            {
                "appointment_id": appt.id,
                "engineer_id": appt.engineer_id,
                "start": f"{appt.start:%Y-%m-%d %H:%M}",
                "duration_minutes": appt.duration_minutes,
                "already_booked": repeat,
                "missing_info": list(missing),
                "unverified": [v.slot for v in verdicts if not v.accepted],
            },
        )

    def reschedule_appointment(
        appointment_id: int,
        start: str,
        duration_minutes: int | None = None,
        engineer_id: int | None = None,
    ) -> ToolChunk:
        try:
            appt = ctx.appointments.reschedule(
                appointment_id,
                start=_parse_time(start, "start"),
                duration_minutes=duration_minutes,
                engineer_id=engineer_id,
                now=datetime.now(),
            )
        except InvalidBooking as exc:
            return _error(f"改期没落成：{exc}", {"appointment_id": appointment_id, "refused": str(exc)})
        return _text(
            render_appointment(appt) + "\n变更日志里留着原来的时段。",
            {
                "appointment_id": appt.id,
                "engineer_id": appt.engineer_id,
                "start": f"{appt.start:%Y-%m-%d %H:%M}",
            },
        )

    def close_appointment(appointment_id: int, resolved: bool, followup_note: str) -> ToolChunk:
        try:
            appt = ctx.appointments.visit(
                appointment_id,
                resolved=resolved,
                followup_note=followup_note,
                now=datetime.now(),
            )
        except InvalidBooking as exc:
            return _error(f"上门登记没落成：{exc}", {"appointment_id": appointment_id, "refused": str(exc)})
        linked = ""
        if appt.ticket_id is not None:
            try:
                ctx.store.comment(
                    appt.ticket_id,
                    f"上门回访：{appt.followup_note}（预约 #{appt.id}）",
                )
                linked = f"\n回访已作为进展追加到工单 #{appt.ticket_id}。"
            except KeyError:
                linked = f"\n注意：关联的工单 #{appt.ticket_id} 不存在，回访没并进工单。"
        return _text(
            render_appointment(appt) + linked,
            {
                "appointment_id": appt.id,
                "resolved": appt.resolved,
                "ticket_id": appt.ticket_id,
                "freed_load": True,
            },
        )

    def upcoming_appointments(within_days: int = 7) -> ToolChunk:
        now = datetime.now()
        appointments = ctx.appointments.upcoming(now=now, within_days=within_days)
        if not appointments:
            return _text(
                f"未来 {within_days} 天没有待上门的预约。别凭记忆答用户，也没必要自己编一个。",
                {"appointments": []},
            )
        return _text(
            "\n".join(render_appointment(a) for a in appointments),
            {"appointments": [[a.id, a.engineer_id, f"{a.start:%Y-%m-%d %H:%M}"] for a in appointments]},
        )

    def recall_preferences() -> ToolChunk:
        """读这位用户的长期偏好：从历史事实折出来的结论（约在几点、常由谁上门、总缺哪个槽位）。

        只在"要给这个人安排上门时间"这类判断上用；它说的是历来如此，不是这次的要求。
        """
        if ctx.memory is None:
            return _text(
                "没有偏好表可读：这次运行不落盘。只按当前会话里用户说过的话判断。",
                {"empty": True, "wired": False},
            )
        prefs = ctx.memory.preferences()
        return _text(
            prefs.render(),
            {
                "empty": prefs.empty,
                "bookings": prefs.bookings,
                "tickets": prefs.tickets,
                "seq": prefs.seq,
                "wired": True,
            },
        )

    def run_autodream(force: bool = False) -> ToolChunk:
        """后台整理：把事件日志里比检查点更新的事实增量折进长期偏好表，并把结果念回来。

        Args:
            force (bool): 跳过「新会话满 5 次、距上次满 24 小时」两道条件照常折叠。
                只有人要你立刻重算时才给 True。
        """
        result = autodream.run(ledger=ctx.ledger, memory=ctx.memory, force=force)
        prefs = result.preferences
        return _text(
            result.text(),
            {
                "ran": result.ran,
                "reason": result.reason,
                "rows": result.rows,
                "sessions": result.sessions,
                "until_seq": result.until_seq,
                "empty": None if prefs is None else prefs.empty,
            },
        )

    tools = [
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
            permission=_ASK,
            description="把工单指派给用户选定的人，或填 ESCALATE_HUMAN 转人工（写操作，会等用户确认）。",
            input_schema=AssignTicketInput,
        ),
        FunctionTool(
            suggest_assignment,
            is_state_injected=True,
            is_concurrency_safe=False,
            permission=_ASK,
            description=(
                "推荐候选工程师（可带上门时段）给用户挑：把 match_service 召回的候选和 "
                "propose_appointments 的可约时段填进来，弹卡片让用户点选，选定后自动落指派"
                "和预约（写操作，会等用户点选）。"
            ),
            input_schema=SuggestAssignmentInput,
        ),
        FunctionTool(
            propose_appointments,
            is_read_only=True,
            permission=_ALLOW,
            description="查可上门时段（只读，不占人）。返回若干 (员工, 时刻) 组合给用户挑。",
            input_schema=ProposeVisitInput,
        ),
        FunctionTool(
            book_appointment,
            is_state_injected=True,
            is_concurrency_safe=False,
            permission=_ASK,
            description="落一个上门预约（写操作，必须用户确认）。会占住这个人和这段时间。",
            input_schema=BookAppointmentInput,
        ),
        FunctionTool(
            reschedule_appointment,
            is_concurrency_safe=False,
            permission=_ASK,
            description="改期或换人（写操作，必须用户确认）。",
            input_schema=RescheduleInput,
        ),
        FunctionTool(
            close_appointment,
            permission=_ALLOW,
            description=(
                "登记这次上门的结果与回访。它记的是已经发生的事，不是对用户的承诺，"
                "所以不再走确认闸。"
            ),
            input_schema=CloseVisitInput,
        ),
        FunctionTool(
            recall_preferences,
            is_read_only=True,
            permission=_ALLOW,
            description="读长期偏好（历来的上门时段/常约的人/总缺的槽位）。只读，结论都带样本数。",
        ),
        FunctionTool(
            upcoming_appointments,
            is_read_only=True,
            permission=_ALLOW,
            description="查最近待上门的预约。用户问「约的什么时候」「上次谁来过」时必须查了再答。",
        ),
    ]
    if ctx.ledger is not None and ctx.memory is not None:
        # 没挂落盘事实源的上下文（console、单测、评测）不递交这个工具：让模型看得见一个
        # 无处可写的工具，只会多烧一轮。它也不进 SYSTEM_PROMPT —— 托管路径的调度任务
        # 只能递一段 `description` 文本进来，"做什么"就写在那段文本里（`autodream.py`）。
        #
        # 权限必须是 ALLOW 而不是 ASK：调度器给的 session 跑在 `permission_mode=DONT_ASK`
        # 下，那个模式把每一条 ASK 换成 DENY（`permission/_types.py`）。一个要确认的
        # AutoDream 工具等于 AutoDream 永远跑不了 —— 而 DENY 正是我们想要的形状：
        # 同一轮里 create_ticket / book_appointment 这些要人确认的写操作物理上执行不了。
        tools.append(
            FunctionTool(
                run_autodream,
                is_concurrency_safe=False,
                permission=_ALLOW,
                description="后台整理：把事件日志增量折成长期偏好表（幂等，有检查点与任务锁）。",
            ),
        )
    return tools
