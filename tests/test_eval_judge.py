"""判分层的量具自检：openjudge 的 grader 有没有齿，不联网也要钉住。

`ToolCallStepSequenceMatchGrader` 是纯规则打分（不调模型），所以这一整份文件是离线的。
它护的是轨迹轴那条 `tool_sequence_score` 红线：**配置错了，乱序的轨迹也会拿满分**，
那这条断言就是假仪器。四组期望值都是先实测再写进来的（见 `docs/P7_EVALUATION.md`）。
"""
from __future__ import annotations

import pytest

from helpdesk.eval.judge import judge_meta, sequence_score
from helpdesk.eval.runtime import Step, Trace


def _msgs(*names: str) -> list[dict]:
    trace = Trace(case_id="t")
    trace.steps = [
        Step(name=n, tool_call_id=f"c{i}", state="success", round_index=i, arguments={})
        for i, n in enumerate(names)
    ]
    return trace.openai_messages("磁盘写满了")


def _refs(*names: str) -> list[list[dict]]:
    return [[{"name": n, "arguments": {}}] for n in names]


async def test_in_order_trajectory_scores_full() -> None:
    score = await sequence_score(
        _msgs("search_knowledge", "create_ticket"),
        _refs("search_knowledge", "create_ticket"),
    )
    assert score == pytest.approx(1.0)


async def test_swapped_steps_score_zero_under_recall() -> None:
    """乱序必须判 0 —— 开 jaccard 或 strict 就不是了，所以配置本身要被钉住。"""
    score = await sequence_score(
        _msgs("create_ticket", "search_knowledge"),
        _refs("search_knowledge", "create_ticket"),
    )
    assert score == pytest.approx(0.0)


async def test_dropping_a_middle_step_shifts_everything_after_it() -> None:
    """实测：grader 是**逐步对齐**（参考第 i 步 对 实际第 i 步），不是集合召回。

    所以漏掉中间一环比"少一环"更贵 —— 后面的步全被错位对掉，3 环只剩 1 环对上。
    """
    score = await sequence_score(
        _msgs("search_knowledge", "assign_ticket"),
        _refs("search_knowledge", "create_ticket", "assign_ticket"),
    )
    assert score == pytest.approx(1 / 3, abs=0.01)


async def test_substituting_one_step_loses_only_that_step() -> None:
    score = await sequence_score(
        _msgs("search_knowledge", "find_open_ticket", "assign_ticket"),
        _refs("search_knowledge", "create_ticket", "assign_ticket"),
    )
    assert score == pytest.approx(2 / 3, abs=0.01)


async def test_extra_steps_are_tolerated() -> None:
    """我们的运行时在建单后还会 match_service+assign_ticket，金标止于建单确认。

    多调不扣分是**必须**的：按集合比（jaccard）会把这段正常多做判成错。
    """
    score = await sequence_score(
        _msgs("search_knowledge", "create_ticket", "match_service", "assign_ticket"),
        _refs("search_knowledge", "create_ticket"),
    )
    assert score == pytest.approx(1.0)


async def test_nothing_called_scores_zero_not_none() -> None:
    score = await sequence_score(_msgs(), _refs("search_knowledge"))
    assert score == pytest.approx(0.0)


def test_report_always_names_the_judge_model() -> None:
    """自偏好不是中性事实：评委默认就是被评者自己，报告必须写出来。"""
    meta = judge_meta()
    assert set(meta) == {"judgeModel", "judgeBase", "judgeLanguage"}
    assert meta["judgeModel"]
    assert meta["judgeBase"].endswith("/v1")
