"""工单存储：工单 / 进展 / 指派审计 / 去重。

进程内字典即可 —— 会话与 AgentState 的持久化由框架的 app.storage 负责，
这里只保管业务事实。去重走文本闸（不调模型）：命中重复时追加进展而不是新建。
"""
from __future__ import annotations

from dataclasses import dataclass, field

from .dispatch import AssignmentDecision, DispatchEvidence, ServiceHit
from .domain import (
    Category,
    Priority,
    Progress,
    ProgressKind,
    Ticket,
    TicketStatus,
)
from .domain import accept as accept_ticket
from .domain import assign as assign_ticket
from .domain import resolve as resolve_ticket
from .runtime.textmatch import COVERAGE_GATE, char_coverage


@dataclass(frozen=True)
class AssignmentLog:
    ticket_id: int
    assignee_id: int | None
    escalated: bool
    rationale: str
    confidence: float
    candidates: tuple[tuple[int, float], ...]
    sequence: int
    employee_candidates: tuple[int, ...] = ()
    in_recall: bool | None = None
    cited_tickets: tuple[int, ...] = ()

    @property
    def outcome(self) -> str:
        return "escalated" if self.escalated else "assigned"


@dataclass(frozen=True)
class CreateResult:
    ticket: Ticket
    deduped_from: int | None

    @property
    def deduped(self) -> bool:
        return self.deduped_from is not None


class UnknownTicket(KeyError):
    pass


@dataclass
class TicketStore:
    tickets: dict[int, Ticket] = field(default_factory=dict)
    progress: list[Progress] = field(default_factory=list)
    assignments: list[AssignmentLog] = field(default_factory=list)
    _next_ticket: int = 1
    _next_progress: int = 1
    _next_seq: int = 1

    def get(self, ticket_id: int) -> Ticket:
        try:
            return self.tickets[ticket_id]
        except KeyError:
            raise UnknownTicket(ticket_id) from None

    def open_tickets(self) -> list[Ticket]:
        return [t for t in self.tickets.values() if t.status is not TicketStatus.DONE]

    def find_duplicate(self, text: str, category: Category | None = None) -> Ticket | None:
        best: tuple[float, Ticket] | None = None
        for t in self.open_tickets():
            if category is not None and t.category is not category:
                continue
            score = min(char_coverage(text, t.text), char_coverage(t.text, text))
            if score >= COVERAGE_GATE and (best is None or score > best[0]):
                best = (score, t)
        return best[1] if best else None

    def create(
        self,
        *,
        title: str,
        description: str,
        category: Category,
        priority: Priority,
        missing_info: tuple[str, ...] = (),
        conversation_id: str | None = None,
    ) -> CreateResult:
        ticket = Ticket(
            id=self._next_ticket,
            title=title,
            description=description,
            category=category,
            priority=priority,
            missing_info=missing_info,
            conversation_id=conversation_id,
        )
        ticket.validate()
        duplicate = self.find_duplicate(ticket.text, category)
        if duplicate is not None:
            self._add_progress(
                duplicate.id,
                ProgressKind.DEDUPE,
                f"重复上报，追加到已有工单：{title}",
            )
            return CreateResult(ticket=duplicate, deduped_from=duplicate.id)
        self.tickets[ticket.id] = ticket
        self._next_ticket += 1
        self._add_progress(ticket.id, ProgressKind.CREATED, f"{title}｜{priority.value}")
        return CreateResult(ticket=ticket, deduped_from=None)

    def assign(
        self,
        ticket_id: int,
        decision: AssignmentDecision,
        hits: tuple[ServiceHit, ...] = (),
        evidence: DispatchEvidence | None = None,
    ) -> Ticket:
        ticket = self.get(ticket_id)
        assign_ticket(ticket, decision.employee_id)
        self.assignments.append(
            AssignmentLog(
                ticket_id=ticket_id,
                assignee_id=decision.employee_id,
                escalated=decision.escalated,
                rationale=decision.rationale,
                confidence=decision.confidence,
                candidates=tuple((h.service_id, h.score) for h in hits),
                sequence=self._next_seq,
                employee_candidates=() if evidence is None else evidence.candidate_ids,
                in_recall=None if evidence is None else evidence.recalled(decision.employee_id),
                cited_tickets=() if evidence is None else evidence.cites(decision.rationale),
            ),
        )
        self._next_seq += 1
        self._add_progress(
            ticket_id,
            ProgressKind.ASSIGNED,
            decision.rationale,
            author_id=decision.employee_id,
        )
        return ticket

    def accept(self, ticket_id: int, author_id: int | None = None) -> Ticket:
        ticket = accept_ticket(self.get(ticket_id))
        self._add_progress(ticket_id, ProgressKind.ACCEPTED, "处理人接单", author_id)
        return ticket

    def resolve(self, ticket_id: int, author_id: int | None = None) -> Ticket:
        ticket = resolve_ticket(self.get(ticket_id))
        self._add_progress(ticket_id, ProgressKind.RESOLVED, "处理完成", author_id)
        return ticket

    def comment(self, ticket_id: int, text: str, author_id: int | None = None) -> Progress:
        return self._add_progress(ticket_id, ProgressKind.COMMENT, text, author_id)

    def progress_of(self, ticket_id: int) -> list[Progress]:
        return [p for p in self.progress if p.ticket_id == ticket_id]

    def _add_progress(
        self,
        ticket_id: int,
        kind: ProgressKind,
        content: str,
        author_id: int | None = None,
    ) -> Progress:
        item = Progress(
            id=self._next_progress,
            ticket_id=ticket_id,
            kind=kind,
            content=content,
            author_id=author_id,
        )
        self.progress.append(item)
        self._next_progress += 1
        return item
