"""校验器必须真的会拒 —— 逐条构造坏行，证明四类造假都挡得住。

金标是唯一能证明指派轴不是在给自己打分的仪器，所以这里的用例
全部对着 RUBRIC 的硬规矩写：留空、非法员工号、离职员工、列间矛盾、改只读列。
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "eval/annotation"))

import validate as v  # noqa: E402

CASES = json.loads((ROOT / "eval/datasets/assignment_v2.json").read_text())["cases"]


def row(case_id: str = "dp-v2-001", **overrides: str) -> dict[str, str]:
    case = next(c for c in CASES if c["id"] == case_id)
    base = {
        "case_id": case_id,
        "标题": case["ticket"]["title"],
        "描述": case["ticket"]["description"],
        "类别": case["ticket"]["category"],
        "优先级": case["ticket"]["priority"],
        "涉及服务": "2001",
        "期望指派": "101",
        "是否应转人工": "否",
        "判定依据": "下单接口 owner 且有余量",
        "可判性": "单值",
    }
    base.update(overrides)
    return base


def errs(*rows: dict[str, str]) -> list[str]:
    return [msg for errors in v.parse_sheet(list(rows)).values() for msg in errors]


def full_sheet() -> list[dict[str, str]]:
    return [row(c["id"]) for c in CASES]


def test_happy_path_passes() -> None:
    assert errs(row()) == []


def test_multi_value_is_allowed_when_declared() -> None:
    assert errs(row(期望指派="105|108", 可判性="多值皆可")) == []


def test_untouched_row_is_a_count_not_an_error() -> None:
    assert v.is_untouched(row(涉及服务="", 期望指派="", 是否应转人工="", 可判性="", 判定依据=""))
    assert not v.is_untouched(row(期望指派=""))


@pytest.mark.parametrize(
    "bad",
    [
        row(期望指派=""),
        row(期望指派="张三"),
        row(期望指派="109"),
        row(期望指派="0"),
        row(期望指派="101|ESCALATE_HUMAN"),
        row(是否应转人工=""),
        row(是否应转人工="是"),
        row(期望指派="ESCALATE_HUMAN", 是否应转人工="否"),
        row(可判性=""),
        row(可判性="大概"),
        row(期望指派="101|107", 可判性="单值"),
        row(可判性="不可判"),
        row(判定依据=""),
        row(涉及服务="9999"),
    ],
)
def test_rejects(bad: dict[str, str]) -> None:
    assert errs(bad), f"这行本该被拒：{bad}"


def test_escalate_with_judgeability_is_clean() -> None:
    assert errs(
        row(
            期望指派="ESCALATE_HUMAN",
            是否应转人工="是",
            可判性="不可判",
            判定依据="同时命中两个归口，正文无主责线索",
            涉及服务="无",
        ),
    ) == []


def test_escalate_as_sole_definite_answer_is_clean() -> None:
    """RUBRIC §2：owner 与 backup 同时无容量且无第三候选时，转人工就是唯一答案。"""
    assert errs(
        row(
            期望指派="ESCALATE_HUMAN",
            是否应转人工="是",
            可判性="单值",
            判定依据="归属人与 backup 都满了，无第三候选",
        ),
    ) == []


def test_tampering_readonly_column_is_caught() -> None:
    assert v.check_tamper(full_sheet(), CASES) == []
    rows = full_sheet()
    rows[3]["描述"] = "改短了"
    msgs = v.check_tamper(rows, CASES)
    assert any("dp-v2-004" in m and "描述" in m for m in msgs), msgs


def test_missing_or_extra_row_is_caught() -> None:
    rows = full_sheet()
    msgs = " ".join(v.check_tamper(rows + [dict(rows[0])], CASES))
    assert "重复" in msgs
    msgs = " ".join(v.check_tamper(rows[1:], CASES))
    assert "行集与数据集不符" in msgs and "dp-v2-001" in msgs


def test_freeze_output_shape() -> None:
    gold = v.freeze([row(), row(case_id="dp-v2-002", 涉及服务="")], CASES)
    assert gold["nCases"] == 2
    assert gold["goldSource"] == "human-blinded-single-annotator"
    first = gold["cases"][0]
    assert first["gold"]["assigneeIds"] == [101]
    assert first["gold"]["serviceHint"] == 2001
    assert gold["cases"][1]["gold"]["serviceHint"] is None
    assert "2.2pt" in gold["goldBoundary"]


def test_self_agreement_ignores_blanks() -> None:
    out = v.self_agreement([row()], [row(可判性="")])
    assert "self-agreement" in out
    flipped = v.self_agreement([row()], [row(期望指派="107")])
    assert "0/1" in flipped or "101" in flipped
