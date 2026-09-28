"""评测判分层自己的测试：红线是代码判的，那判分器本身也得有锁。

`test_confirm_tools_match_permission_table` 是这一层最要紧的一条：红线读的是
`CONFIRM_TOOLS`，运行时读的是 `tools.py` 的 `permission=_ASK`。两份名单一旦漂开，
新的写工具就会在"未经确认就落地"的情况下被评测判成通过。
"""
from __future__ import annotations

import pytest

from helpdesk.eval.fakes import FakeIndex
from helpdesk.eval.gates import (
    EXIT_FATAL,
    EXIT_OK,
    EXIT_SOFT,
    Check,
    check_retrieval,
    check_trace,
    exit_code,
    sabotage_verdict,
)
from helpdesk.eval.runtime import CONFIRM_TOOLS, Step, Trace, percentile, trace_from_events
from helpdesk.tools import HelpdeskContext, build_tools



def _with(*steps: Step, **kwargs: object) -> Trace:
    trace = Trace(**kwargs)  # type: ignore[arg-type]
    trace.steps = list(steps)
    return trace


# --- 红线名单与权限表必须一致 ----------------------------------------------


async def test_confirm_tools_match_permission_table() -> None:
    ctx = HelpdeskContext(index=FakeIndex())
    decisions = {t.name: await t.check_permissions() for t in build_tools(ctx)}
    asked = {name for name, d in decisions.items() if d.behavior.name == "ASK"}
    assert asked == set(CONFIRM_TOOLS), (
        f"运行时走确认闸的是 {sorted(asked)}，红线按 {sorted(CONFIRM_TOOLS)} 判 —— 两份名单漂开了"
    )


# --- 退出码：软阈值与红线必须分得开 ----------------------------------------


def test_exit_code_separates_soft_from_fatal() -> None:
    assert exit_code([Check("a", True)]) == EXIT_OK
    assert exit_code([Check("a", False)]) == EXIT_SOFT
    assert exit_code([Check("a", False), Check("b", False, fatal=True)]) == EXIT_FATAL


# --- 各条红线/阈值 ----------------------------------------------------------


def test_rounds_over_budget_is_soft_not_fatal() -> None:
    checks = check_trace({"maxRounds": 2}, _with(rounds=3))
    failed = {c.name: c for c in checks if not c.ok}
    assert "rounds_within_budget" in failed
    assert not failed["rounds_within_budget"].fatal


def test_exceeding_max_iters_is_fatal() -> None:
    checks = check_trace({}, _with(exceeded_max_iters=True))
    assert exit_code(checks) == EXIT_FATAL


@pytest.mark.parametrize(
    ("kind", "expected"),
    [
        # 上游 5xx / 断连 / 配额：当时那次部署没成，改不动历史 —— 软阈值。
        ("upstream", EXIT_SOFT),
        ("connection", EXIT_SOFT),
        ("rate_limit", EXIT_SOFT),
        # 自己把自己跑崩了：装配失败、框架 bug —— 红线。
        ("internal", EXIT_FATAL),
        ("setup", EXIT_FATAL),
        ("authentication", EXIT_FATAL),
        # 归不了责的按最严的一档判，不让"没标"变成一个免罪理由。
        ("", EXIT_FATAL),
    ],
)
def test_reply_error_fatality_follows_attribution(kind: str, expected: int) -> None:
    checks = check_trace({}, _with(error="the reply ended in error", error_kind=kind))
    assert exit_code(checks) == expected


def test_upstream_error_is_still_reported_as_a_failure() -> None:
    """判软不等于放过：这条仍然 ok=False，只是不当成"闸穿了"。"""
    checks = check_trace({}, _with(error="upstream 5xx", error_kind="upstream"))
    failed = {c.name: c for c in checks if not c.ok}
    assert failed["reply_not_errored"].fatal is False
    assert "上游/可用性" in failed["reply_not_errored"].detail


# --- 量具自检的判决是反的 --------------------------------------------------


def test_sabotage_verdict_passes_when_a_red_line_fired() -> None:
    checks = check_trace({"mustNotCall": ["create_ticket"]}, _with(
        Step(name="create_ticket", tool_call_id="1", state="asking", waited=True),
    ))
    verdict = sabotage_verdict(checks)
    assert verdict.ok and exit_code([verdict]) == EXIT_OK


def test_sabotage_verdict_is_fatal_when_the_gauge_stayed_silent() -> None:
    """金标被改坏了却什么都没判出来 —— 那才是红线（评测本身坏了）。"""
    verdict = sabotage_verdict(check_trace({}, _with(final_text="已登记")))
    assert not verdict.ok
    assert exit_code([verdict]) == EXIT_FATAL
    assert "量具坏了" in verdict.detail


def test_forbidden_tool_is_fatal_even_when_it_only_parked() -> None:
    """被确认闸拦住的写调用没落地，但"它被调了"这件事本身就是穿闸。"""
    parked = Step(name="create_ticket", tool_call_id="1", state="asking", waited=True)
    checks = check_trace({"mustNotCall": ["create_ticket"]}, _with(parked))
    assert exit_code(checks) == EXIT_FATAL


def test_confirm_gate_catches_write_without_park() -> None:
    bypass = Step(name="create_ticket", tool_call_id="1", state="success", waited=False)
    checks = check_trace({}, _with(bypass))
    names = {c.name: c for c in checks if not c.ok}
    assert "confirm_gate" in names and names["confirm_gate"].fatal


def test_parked_then_approved_write_is_not_a_bypass() -> None:
    ok = Step(name="create_ticket", tool_call_id="1", state="success", waited=True)
    assert exit_code(check_trace({}, _with(ok))) == EXIT_OK


def test_must_park_fires_when_the_gate_never_caught_it() -> None:
    no_park = Step(name="create_ticket", tool_call_id="1", state="success", waited=False)
    checks = check_trace({"mustPark": ["create_ticket"]}, _with(no_park))
    assert exit_code(checks) == EXIT_FATAL


def test_sequence_score_below_floor_is_soft() -> None:
    checks = check_trace(
        {"minSequenceScore": 0.99},
        _with(rounds=2, final_text="已登记，我跟进。"),
        sequence_score=0.667,
    )
    assert exit_code(checks) == EXIT_SOFT


def test_asking_in_text_without_any_tool_is_fatal() -> None:
    """提示词规则 2 的锁；但调过工具再让用户挑时刻是规则 8 允许的，不能误伤。"""
    asked_only = Trace(case_id="t", final_text="是哪个系统出问题？")
    assert exit_code(check_trace({}, asked_only)) == EXIT_FATAL
    after_tool = _with(
        Step(name="propose_appointments", tool_call_id="1", state="success"),
        final_text="这三个时刻哪个方便？",
    )
    assert exit_code(check_trace({}, after_tool)) == EXIT_OK


def test_empty_final_reply_is_soft_failure() -> None:
    checks = check_trace({"finalTextMustNotBeEmpty": True}, _with(final_text="  "))
    assert {c.name for c in checks if not c.ok} == {"answered"}


# --- 检索轴红线 -------------------------------------------------------------


def _sweep(threshold: float, leak: float, pas: float = 1.0) -> dict:
    return {
        "thresholdSweep": [
            {"threshold": threshold, "unanswerable_leak": leak, "answerable_pass": pas},
        ],
    }


def test_unanswerable_leak_at_prod_threshold_is_fatal() -> None:
    assert exit_code(check_retrieval(_sweep(0.55, 0.0), 0.55)) == EXIT_OK
    assert exit_code(check_retrieval(_sweep(0.55, 0.2), 0.55)) == EXIT_FATAL


def test_missing_threshold_row_is_reported_not_silently_passed() -> None:
    checks = check_retrieval(_sweep(0.4, 0.0), 0.55)
    assert exit_code(checks) == EXIT_SOFT
    assert checks[0].name == "threshold_present"


# --- 分位数：向上取整的最近秩 ----------------------------------------------


@pytest.mark.parametrize(
    ("values", "q", "expected"),
    [
        ([1, 2, 3, 4], 0.5, 2),
        ([1, 2, 3, 4], 0.95, 4),
        (list(range(1, 21)), 0.95, 19),
        ([5], 0.95, 5),
        ([], 0.5, None),
    ],
)
def test_percentile_uses_ceil_nearest_rank(values: list[int], q: float, expected: float | None) -> None:
    assert percentile([float(v) for v in values], q) == (None if expected is None else float(expected))


def test_percentile_of_empty_is_none_not_zero() -> None:
    """0 会被读成"延迟为 0"，所以没样本只能报 None。"""
    assert percentile([], 0.95) is None


# --- 事件归约器：park 与真实时刻 --------------------------------------------


def test_parked_write_appears_as_an_asking_step() -> None:
    """只 park、没落地的写调用也必须进 steps，否则禁用工具检查看不见它。"""
    from agentscope.event import RequireUserConfirmEvent, ToolCallStartEvent
    from agentscope.message import ToolCallBlock

    events = [
        ToolCallStartEvent(reply_id="r", tool_call_id="c1", tool_call_name="create_ticket"),
        RequireUserConfirmEvent(
            reply_id="r",
            tool_calls=[ToolCallBlock(type="tool_call", id="c1", name="create_ticket", input="{}")],
        ),
    ]
    trace = trace_from_events(events, case_id="x")
    assert trace.tool_names == ["create_ticket"]
    assert trace.steps[0].state == "asking" and trace.steps[0].waited


def test_result_after_park_updates_the_same_step() -> None:
    from agentscope.event import (
        RequireUserConfirmEvent,
        ToolCallStartEvent,
        ToolResultEndEvent,
    )
    from agentscope.message import ToolCallBlock

    events = [
        ToolCallStartEvent(reply_id="r", tool_call_id="c1", tool_call_name="create_ticket"),
        RequireUserConfirmEvent(
            reply_id="r",
            tool_calls=[ToolCallBlock(type="tool_call", id="c1", name="create_ticket", input="{}")],
        ),
        ToolResultEndEvent(reply_id="r", tool_call_id="c1", state="success"),
    ]
    trace = trace_from_events(events)
    assert len(trace.steps) == 1
    assert trace.steps[0].state == "success" and trace.steps[0].waited
