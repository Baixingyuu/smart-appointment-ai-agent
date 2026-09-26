"""P2 内核的离线断言：状态机、去重闸、追问安全阀、层 2 的枚举接缝。"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.dispatch import (  # noqa: E402
    AssignmentDecision,
    assignment_prompt,
    render_services,
    to_hit,
)
from helpdesk.domain import (  # noqa: E402
    Category,
    Draft,
    ExtractedSlot,
    InvalidTransition,
    Priority,
    ProgressKind,
    TicketStatus,
    blocking_slots,
    missing_blocking_slots,
    slot_label,
)
from helpdesk.runtime.textmatch import char_coverage, is_substring, matches, normalize  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402


def make(store: TicketStore, title="下单接口 500", description="线上下单接口持续返回 500，订单无法创建。"):
    return store.create(
        title=title,
        description=description,
        category=Category.INCIDENT,
        priority=Priority.P1,
    )


def decision(assignee="101", rationale="owner 且有余量") -> AssignmentDecision:
    return AssignmentDecision(assignee=assignee, rationale=rationale, confidence=0.8)


# --- 状态机 -----------------------------------------------------------------


def test_ticket_lifecycle_is_explicit() -> None:
    store = TicketStore()
    ticket = make(store).ticket
    assert ticket.status is TicketStatus.PENDING
    with pytest.raises(InvalidTransition):
        store.accept(ticket.id)
    store.assign(ticket.id, decision())
    assert ticket.assignee_id == 101
    store.accept(ticket.id, author_id=101)
    assert ticket.status is TicketStatus.IN_PROGRESS
    store.resolve(ticket.id, author_id=101)
    assert ticket.status is TicketStatus.DONE
    with pytest.raises(InvalidTransition):
        store.assign(ticket.id, decision())


def test_escalate_leaves_ticket_unassigned_but_logged() -> None:
    store = TicketStore()
    ticket = make(store).ticket
    store.assign(ticket.id, decision(assignee="ESCALATE_HUMAN", rationale="无人可派"))
    assert ticket.assignee_id is None
    log = store.assignments[0]
    assert log.escalated and log.outcome == "escalated" and log.assignee_id is None
    kinds = [p.kind for p in store.progress_of(ticket.id)]
    assert kinds == [ProgressKind.CREATED, ProgressKind.ASSIGNED]


# --- 去重 -------------------------------------------------------------------


def test_duplicate_report_appends_progress_instead_of_new_ticket() -> None:
    store = TicketStore()
    first = make(store).ticket
    again = make(
        store,
        title="下单接口 500",
        description="线上下单接口持续返回 500，订单无法创建，客户投诉。",
    )
    assert again.deduped and again.ticket.id == first.id
    assert len(store.tickets) == 1
    assert store.progress_of(first.id)[-1].kind is ProgressKind.DEDUPE


def test_dedup_respects_category_and_distance() -> None:
    store = TicketStore()
    make(store)
    other = store.create(
        title="账号被锁定",
        description="连续输错密码导致账号锁定十五分钟，无法登录。",
        category=Category.INCIDENT,
        priority=Priority.P2,
    )
    assert not other.deduped and len(store.tickets) == 2


def test_resolved_ticket_no_longer_absorbs_duplicates() -> None:
    store = TicketStore()
    ticket = make(store).ticket
    store.assign(ticket.id, decision())
    store.accept(ticket.id)
    store.resolve(ticket.id)
    later = make(store)
    assert not later.deduped and later.ticket.id != ticket.id


# --- 文本闸 -----------------------------------------------------------------


def test_textmatch_gate_behaviour() -> None:
    source = "从今天上午 10:20 起，线上下单接口持续返回 500。"
    assert is_substring("下单接口持续返回 500", source)
    assert normalize("ＡＰＩ　接口") == "api接口"
    assert matches("接口持续返回500错误", source)
    assert char_coverage("报销单在哪里提交", source) < 0.5
    assert not matches("报销单在哪里提交", source)


# --- 追问槽位 ---------------------------------------------------------------


def test_blocking_slots_and_missing_math() -> None:
    assert blocking_slots(Category.CHANGE) == ("target_service", "planned_window")
    assert blocking_slots(Category.INCIDENT) == ("affected_system",)
    assert slot_label("affected_system") == "是哪个系统/服务出的问题"
    filled = (ExtractedSlot("affected_system", "下单接口"),)
    assert missing_blocking_slots(Category.INCIDENT, filled) == ()
    assert missing_blocking_slots(
        Category.CHANGE,
        (ExtractedSlot("target_service", ""),),
    ) == ("target_service", "planned_window")


def test_ask_progress_stops_repeating_and_hits_the_valve() -> None:
    draft = Draft(ticket=make(TicketStore()).ticket)
    missing = ("target_service", "planned_window")
    assert draft.intake.should_ask(missing) == missing
    # 一次追问可以同问两个槽位，但只烧掉一轮额度
    assert draft.intake.ask(missing) == missing
    assert draft.intake.ask_rounds == 1
    assert draft.intake.should_ask(missing) == ()
    fresh = Draft(ticket=make(TicketStore()).ticket)
    fresh.intake.ask_rounds = 2
    assert fresh.intake.should_ask(("affected_system",)) == ()


# --- 层 2 的接缝 ------------------------------------------------------------


def test_assignment_schema_rejects_invented_employee() -> None:
    with pytest.raises(Exception):
        AssignmentDecision(assignee="999", rationale="名册里没有这个人", confidence=0.9)
    assert AssignmentDecision(assignee="109", rationale="名册里离职的人", confidence=0.9).employee_id == 109


def test_store_refuses_inactive_assignee() -> None:
    store = TicketStore()
    ticket = make(store).ticket
    with pytest.raises(InvalidTransition):
        store.assign(ticket.id, decision(assignee="109", rationale="已离职但仍被模型选中"))
    assert ticket.assignee_id is None and store.assignments == []


def test_assignment_json_schema_exposes_the_enum() -> None:
    schema = AssignmentDecision.model_json_schema()
    props = schema["properties"]["assignee"]
    enum = props.get("enum") or props["anyOf"][0]["enum"]
    assert "ESCALATE_HUMAN" in enum and "101" in enum and len(enum) == 10


def test_prompt_carries_facts_not_predictions() -> None:
    hits = (to_hit(2001, 0.712), to_hit(2012, 0.44))
    text = assignment_prompt("下单接口 500", hits)
    assert "owner=101" in text and "无 backup" in text
    assert "109" in text and "已离职（不可派单）" in text
    assert "相似度=0.712" in text
    assert render_services(()) == "服务字典检索无命中。"
