"""P2 内核的离线断言：状态机、去重闸、追问安全阀、层 2 的枚举接缝。"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.catalog import ROSTER, employees  # noqa: E402
from helpdesk.dispatch import (  # noqa: E402
    MAX_EXTRA_CANDIDATES,
    AssignmentDecision,
    DispatchEvidence,
    EmployeeCandidate,
    HistoryCase,
    assignment_prompt,
    recall_employees,
    recall_history,
    render_candidates,
    render_history,
    render_services,
    to_hit,
)
from helpdesk.eval.fakes import FakeIndex, history_hit  # noqa: E402
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


def _employee(emp_id: int):
    return next(e for e in employees() if e.id == emp_id)


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
    assert "ESCALATE_HUMAN" in enum and "101" in enum
    assert len(enum) == len(ROSTER) + 1
    assert {"109", "124"} <= set(enum), "离职者必须在枚举里，否则在职闸无法观测"


def test_prompt_carries_facts_not_predictions() -> None:
    hits = (to_hit(2001, 0.712), to_hit(2012, 0.44))
    candidates = (
        EmployeeCandidate(_employee(101), 0.66, ("owner@2001", "semantic")),
        EmployeeCandidate(_employee(109), None, ("semantic",)),
    )
    text = assignment_prompt("下单接口 500", hits, candidates)
    assert "owner=101" in text and "无 backup" in text
    assert "109" in text and "已离职（不可派单）" in text
    assert "相似度=0.712" in text
    assert "召回理由=owner@2001+semantic" in text
    assert render_services(()) == "服务字典检索无命中。"


def test_evidence_flags_over_selection_and_citations() -> None:
    hits = (to_hit(2001, 0.7),)
    candidates = (
        EmployeeCandidate(_employee(101), 0.7, ("owner@2001",)),
        EmployeeCandidate(_employee(107), None, ("backup@2001",)),
    )
    history = (HistoryCase(12, "下单接口 500", 101, 2001, 0.62),)
    ev = DispatchEvidence(candidates=candidates, history=history)
    assert ev.candidate_ids == (101, 107)
    assert ev.recalled(101) is True
    assert ev.recalled(105) is False
    assert ev.recalled(None) is True, "转人工不是越选"
    assert ev.cites("参照工单 12 的处置，派 owner") == (12,)
    assert ev.cites("派 101，服务 2001 的 owner") == (), "服务号不该被当成工单引用"


# --- 派单 v4：三源召回都长在工具里 -------------------------------------------


async def test_ownership_candidates_are_never_truncated() -> None:
    hits = (to_hit(2001, 0.71), to_hit(2002, 0.6))
    index = FakeIndex(employee_ids=("103", "104", "105", "106", "108"))
    ids = [c.id for c in await recall_employees(index, "下单接口 500", hits)]
    assert ids[:3] == [101, 107, 102], "归属候选排在前面，也不占语义名额"
    assert len(ids) == 3 + MAX_EXTRA_CANDIDATES


async def test_semantic_hit_on_an_owner_merges_reasons() -> None:
    hits = (to_hit(2001, 0.71),)
    candidates = await recall_employees(FakeIndex(employee_ids=("101", "105")), "下单接口 500", hits)
    assert [c.id for c in candidates] == [101, 107, 105]
    assert candidates[0].reasons == ("owner@2001", "semantic")
    assert candidates[0].structural and not candidates[2].structural


async def test_history_recall_reads_the_chunk_metadata() -> None:
    index = FakeIndex(history=(history_hit(7, "下单接口 500", 101, 2001, 0.62),))
    cases = await recall_history(index, "下单接口 500")
    assert (cases[0].ticket_id, cases[0].assignee_id, cases[0].service_id) == (7, 101, 2001)
    assert "工单 7" in render_history(cases)
    assert render_history(()) == "无可参考的已结历史工单。"
    assert render_candidates(()) == "无候选人。"


def test_inactive_candidate_stays_visible() -> None:
    """拦他的是 domain.assign 的在职闸，不是召回 —— 删掉就等于把闸变成死代码。"""
    text = render_candidates((EmployeeCandidate(_employee(109), 0.5, ("semantic",)),))
    assert "109" in text and "已离职（不可派单）" in text


def test_assignment_log_records_recall_and_citations() -> None:
    hits = (to_hit(2001, 0.71),)
    evidence = DispatchEvidence(
        candidates=(
            EmployeeCandidate(_employee(101), 0.7, ("owner@2001",)),
            EmployeeCandidate(_employee(107), None, ("backup@2001",)),
        ),
        history=(HistoryCase(7, "下单接口 500", 101, 2001, 0.62),),
    )
    store = TicketStore()
    ticket = make(store).ticket
    store.assign(ticket.id, decision(assignee="105", rationale="前端更熟"), hits, evidence)
    assert ticket.assignee_id == 105, "越选只记录，不拦截"
    assert store.assignments[-1].in_recall is False
    assert store.assignments[-1].employee_candidates == (101, 107)
    store.assign(ticket.id, decision(assignee="101", rationale="同工单 7，当初也是他"), hits, evidence)
    assert store.assignments[-1].in_recall is True
    assert store.assignments[-1].cited_tickets == (7,)
    store.assign(ticket.id, decision(assignee="ESCALATE_HUMAN", rationale="无人可派"), hits, evidence)
    assert store.assignments[-1].in_recall is True
