"""工单域模型：状态流转、建单槽位、追问进度。

派单已删成两层，所以这里没有打分器，只有"业务事实 + 不丢单的那条承诺"：
缺阻塞槽位可以先建单并记 missing_info，而不是卡住用户。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum

from .catalog import employees

MAX_ASK_ROUNDS = 2


class Category(StrEnum):
    INCIDENT = "incident"
    CONSULTATION = "consultation"
    REQUEST = "request"
    CHANGE = "change"


class Priority(StrEnum):
    P0 = "P0"
    P1 = "P1"
    P2 = "P2"
    P3 = "P3"


class TicketStatus(StrEnum):
    PENDING = "pending"
    IN_PROGRESS = "in_progress"
    DONE = "done"


class ProgressKind(StrEnum):
    CREATED = "created"
    ASSIGNED = "assigned"
    ACCEPTED = "accepted"
    ESCALATED = "escalated"
    RESOLVED = "resolved"
    COMMENT = "comment"
    DEDUPE = "dedupe"


_BLOCKING_SLOTS: dict[Category, tuple[str, ...]] = {
    Category.INCIDENT: ("affected_system",),
    Category.CONSULTATION: ("asked_topic",),
    Category.REQUEST: ("requested_action",),
    # 变更：改哪 + 什么时候，缺任一都无法评估风险。
    Category.CHANGE: ("target_service", "planned_window"),
}

_SLOT_LABELS = {
    "affected_system": "是哪个系统/服务出的问题",
    "asked_topic": "您想咨询的具体问题是什么",
    "requested_action": "您希望我们执行什么操作",
    "target_service": "这次变更针对哪个服务",
    "planned_window": "计划的变更时间窗口",
}


def blocking_slots(category: Category) -> tuple[str, ...]:
    return _BLOCKING_SLOTS.get(category, ())


def slot_label(name: str) -> str:
    return _SLOT_LABELS.get(name, f"请补充：{name}")


@dataclass(frozen=True)
class ExtractedSlot:
    """模型声称某槽位已填时给的证据：quote 必须逐字摘自用户原话。

    quote 是否真出自原文由 runtime/traceability 判定，这里只管结构。
    """

    name: str
    quote: str


def missing_blocking_slots(
    category: Category,
    extracted: tuple[ExtractedSlot, ...],
) -> tuple[str, ...]:
    filled = {s.name for s in extracted if s.quote.strip()}
    return tuple(s for s in blocking_slots(category) if s not in filled)


@dataclass
class Ticket:
    id: int
    title: str
    description: str
    category: Category
    priority: Priority
    status: TicketStatus = TicketStatus.PENDING
    assignee_id: int | None = None
    missing_info: tuple[str, ...] = ()
    deduped_from: int | None = None
    source_channel: str = "console"
    conversation_id: str | None = None

    def validate(self) -> None:
        if self.id <= 0:
            raise ValueError("ticket id must be positive")
        if not self.title.strip():
            raise ValueError(f"ticket {self.id}: title is required")
        if not self.description.strip():
            raise ValueError(f"ticket {self.id}: description is required")

    @property
    def text(self) -> str:
        return f"{self.title}。{self.description}"


@dataclass(frozen=True)
class Progress:
    id: int
    ticket_id: int
    kind: ProgressKind
    content: str
    author_id: int | None


class InvalidTransition(ValueError):
    pass


def assign(ticket: Ticket, assignee_id: int | None) -> Ticket:
    """assignee_id=None 表示转人工待认领池，状态仍是 pending。

    在职是删掉打分器之后唯一留下的硬事实闸：枚举里保留 109，
    这样模型真选了离职员工时能被观测到并拒绝，而不是静默写进工单。
    """
    if ticket.status is TicketStatus.DONE:
        raise InvalidTransition("已完成的工单不能重新指派")
    if assignee_id is not None and assignee_id not in {
        e.id for e in employees() if e.active
    }:
        raise InvalidTransition(f"{assignee_id} 不在职，不能被指派")
    ticket.assignee_id = assignee_id
    return ticket


def accept(ticket: Ticket) -> Ticket:
    if ticket.status is not TicketStatus.PENDING:
        raise InvalidTransition(f"{ticket.status.value} 的工单不能接单")
    if ticket.assignee_id is None:
        raise InvalidTransition("未指派的工单不能接单")
    ticket.status = TicketStatus.IN_PROGRESS
    return ticket


def resolve(ticket: Ticket) -> Ticket:
    if ticket.status is not TicketStatus.IN_PROGRESS:
        raise InvalidTransition("只有处理中的工单能标记完成")
    ticket.status = TicketStatus.DONE
    return ticket


@dataclass
class IntakeProgress:
    """追问进度：安全阀 + 不重复问同一个槽位。

    措辞由模型自己组织（框架的 extra_fields 注入），这里只回答"还该不该问"。
    """

    asked_slots: list[str] = field(default_factory=list)
    ask_rounds: int = 0

    def asked(self, slot: str) -> bool:
        return slot in self.asked_slots

    def ask(self, slots: tuple[str, ...]) -> tuple[str, ...]:
        """一次追问记**一轮**（变更类可以一次问两个槽位），返回真正新问到的槽位。"""
        fresh = tuple(s for s in slots if not self.asked(s))
        self.asked_slots.extend(fresh)
        self.ask_rounds += 1
        return fresh

    def should_ask(self, missing: tuple[str, ...]) -> tuple[str, ...]:
        """还能追问的槽位；安全阀到顶或全部问过 → 空，调用方据此直接建单。"""
        if self.ask_rounds >= MAX_ASK_ROUNDS:
            return ()
        return tuple(s for s in missing if not self.asked(s))


@dataclass
class Draft:
    """待确认的建单草案：确认桥与 intake 计数都挂在它上面。"""

    ticket: Ticket
    intake: IntakeProgress = field(default_factory=IntakeProgress)
