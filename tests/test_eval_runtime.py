"""运行时轴的采集层测试：落库的 `messages` 表 → 四段延迟之和必须等于总时长。

这张表是托管路径唯一的成本来源（跑过的会话都在里面），而它的块结构有坑：
`tool_call` 块 50ms 就封板了，工具真正的执行窗口在 `tool_result` 块上，
确认类工具从"调用封板"到"结果出现"之间隔的是人读卡片的时间（实测 7~74s）。
把这三件事混成一个 wall 就等于报假数，所以这里逐段钉住。
"""
from __future__ import annotations

import json
import sqlite3
from datetime import datetime, timedelta

import pytest

from helpdesk.eval.runtime import percentile, runtime_summary, traces_from_messages_db

T0 = datetime(2026, 9, 27, 12, 0, 0)


def _ts(offset_s: float) -> str:
    return (T0 + timedelta(seconds=offset_s)).isoformat()


def _write_db(path, messages: list[dict]) -> None:
    conn = sqlite3.connect(path)
    conn.execute("create table messages (session_id varchar, msg_id varchar, created_at datetime, payload json)")
    for i, msg in enumerate(messages):
        conn.execute(
            "insert into messages values (?,?,?,?)",
            (msg.get("session_id", "s1"), f"m{i}", msg["created_at"], json.dumps(msg)),
        )
    conn.commit()
    conn.close()


def _reply(*blocks: dict, usage: dict | None = None) -> dict:
    return {
        "role": "assistant",
        "name": "helpdesk",
        "created_at": _ts(0),
        "finished_at": blocks[-1].get("finished_at"),
        "content": list(blocks),
        "usage": usage or {"input_tokens": 100, "output_tokens": 5},
        "finished_reason": "completed",
        "error": None,
    }


@pytest.fixture
def hosted_db(tmp_path):
    """一条真实形状的 reply：模型 3s → create_ticket 调用（封板 0.05s）→ 人看 60s →
    执行 0.001s → 模型 2s → match_service 执行 1.6s → 收尾正文生成 0.6s。"""
    path = tmp_path / "service.db"
    _write_db(
        path,
        [
            {"role": "user", "session_id": "s1", "created_at": _ts(-5), "content": []},
            _reply(
                {
                    "type": "tool_call", "id": "c1", "name": "create_ticket", "state": "finished",
                    "input": '{"title":"磁盘写满"}', "created_at": _ts(3), "finished_at": _ts(3.05),
                },
                {
                    "type": "tool_result", "id": "c1", "name": "create_ticket", "state": "success",
                    "created_at": _ts(63.05), "finished_at": _ts(63.051),
                },
                {
                    "type": "tool_call", "id": "c2", "name": "match_service", "state": "finished",
                    "input": '{"query":"磁盘写满"}', "created_at": _ts(65.05), "finished_at": _ts(65.1),
                },
                {
                    "type": "tool_result", "id": "c2", "name": "match_service", "state": "success",
                    "created_at": _ts(65.101), "finished_at": _ts(66.7),
                },
                {
                    "type": "text", "text": "工单已建，指派给 102。",
                    "created_at": _ts(67.2), "finished_at": _ts(67.8),
                },
            ),
        ],
    )
    return path


def test_timeline_tiles_exactly_into_four_buckets(hosted_db) -> None:
    trace = traces_from_messages_db(hosted_db)[0]
    assert trace.residual_ms == pytest.approx(0.0, abs=0.2)
    assert trace.wall_ms == pytest.approx(67800.0, abs=1.0)
    assert trace.wait_ms == pytest.approx(60000.0, abs=1.0)
    assert trace.tool_ms == pytest.approx(1600.0, abs=1.0)  # 结果块自身：1ms + 1599ms
    assert trace.model_ms == pytest.approx(trace.wall_ms - trace.tool_ms - trace.wait_ms, abs=1.0)


def test_park_is_not_counted_as_tool_execution(hosted_db) -> None:
    """62 秒是人读卡片，不是工具慢 —— 混进 toolMs 就会把优化方向指错。"""
    trace = traces_from_messages_db(hosted_db)[0]
    create = next(s for s in trace.steps if s.name == "create_ticket")
    match = next(s for s in trace.steps if s.name == "match_service")
    assert create.waited and create.duration_ms == pytest.approx(60051.0, abs=1.0)
    assert not match.waited
    assert match.duration_ms == pytest.approx(1650.0, abs=1.0)
    assert trace.parked == ["create_ticket"]


def test_arguments_and_tokens_come_from_the_row(hosted_db) -> None:
    trace = traces_from_messages_db(hosted_db)[0]
    assert trace.steps[0].arguments == {"title": "磁盘写满"}
    assert (trace.input_tokens, trace.output_tokens) == (100, 5)
    assert trace.final_text == "工单已建，指派给 102。"
    assert trace.rounds == 3  # 两次工具轮 + 一次收尾正文


def test_user_rows_are_not_traced(hosted_db) -> None:
    assert len(traces_from_messages_db(hosted_db)) == 1


def test_error_reply_keeps_the_framework_attribution(tmp_path) -> None:
    """`error.type` 是红线分档的依据（上游 5xx 判软、internal 判红），必须一路带到 Trace。"""
    path = tmp_path / "err.db"
    msg = _reply({"type": "text", "text": "", "created_at": _ts(1), "finished_at": _ts(1)})
    msg["error"] = {"type": "upstream", "message": "The upstream model service returned an error."}
    msg["finished_reason"] = "error"
    untyped = _reply({"type": "text", "text": "", "created_at": _ts(1), "finished_at": _ts(1)})
    untyped["error"] = {"message": "no type given"}
    reason_only = _reply({"type": "text", "text": "", "created_at": _ts(1), "finished_at": _ts(1)})
    reason_only["finished_reason"] = "error"
    _write_db(path, [msg, untyped, reason_only])
    first, second, third = traces_from_messages_db(path)
    assert (first.error, first.error_kind) == ("The upstream model service returned an error.", "upstream")
    assert (second.error, second.error_kind) == ("no type given", "")
    assert third.error == "finished_reason=error"


def test_summary_flags_that_p95_is_underpowered(hosted_db) -> None:
    traces = traces_from_messages_db(hosted_db)
    summary = runtime_summary(traces)
    assert summary["samples"] == 1
    assert "撑不起 P95" in summary["p95Note"]
    assert summary["perToolMs"]["match_service"]["p50"] == pytest.approx(1650.0, abs=1.0)


def test_percentile_and_empty_guard() -> None:
    assert percentile([], 0.95) is None
    assert runtime_summary([])["wallMs"]["p50"] is None


def test_runtime_axis_holds_error_replies_out_of_the_distribution(tmp_path) -> None:
    """历史落库里的上游 5xx：判软（不判红线）且不进分布，否则真实流量被算得又快又省。"""
    from helpdesk.eval.gates import EXIT_SOFT
    from helpdesk.eval.run import run_runtime

    good = _reply(
        {
            "type": "tool_call", "id": "c1", "name": "ask_user", "state": "finished",
            "input": '{"slots":["issue_description"]}', "created_at": _ts(2), "finished_at": _ts(2.05),
        },
        {
            "type": "tool_result", "id": "c1", "name": "ask_user", "state": "success",
            "created_at": _ts(2.051), "finished_at": _ts(2.15),
        },
        {"type": "text", "text": "请问是哪个系统出的问题？", "created_at": _ts(2.2), "finished_at": _ts(2.7)},
    )
    bad = _reply({"type": "text", "text": "", "created_at": _ts(1), "finished_at": _ts(1.002)})
    bad["session_id"] = "s2"
    bad["error"] = {"type": "upstream", "message": "The upstream model service returned an error."}
    bad["finished_reason"] = "error"
    bad["usage"] = {"input_tokens": 0, "output_tokens": 0}
    path = tmp_path / "mixed.db"
    _write_db(path, [good, bad])

    payload, rc = run_runtime(path)
    assert rc == EXIT_SOFT, "上游 5xx 判软：重跑当前代码改不动历史数据"
    assert payload["totals"] == {
        "runs": 2, "measured": 1, "excluded": 1, "passed": 1, "softFail": 1, "fatalFail": 0,
    }
    assert payload["runtime"]["samples"] == 1
    assert payload["runtime"]["wallMs"]["p50"] == pytest.approx(2700.0, abs=1.0)
    assert payload["runtime"]["excludedReplies"][0]["errorKind"] == "upstream"
