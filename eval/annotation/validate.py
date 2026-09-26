"""校验人工指派盲标表并冻结为金标（`make validate`）。

挡的是四类造假：留空、非法员工号、把已离职的 109 当答案、
以及自相矛盾的列组合（转人工却给了员工号、不可判却派了人）。
再加一道防篡改：只读列必须与数据集逐字一致。

用法：
  .venv/bin/python eval/annotation/validate.py              # 校验 out/sheet.csv
  .venv/bin/python eval/annotation/validate.py --freeze     # 通过后写 assignment_gold_v2.json
  .venv/bin/python eval/annotation/validate.py --recheck out/recheck.csv   # 自一致性
"""
from __future__ import annotations

import argparse
import csv
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.catalog import ESCALATE_HUMAN, employees, services_by_id  # noqa: E402

HERE = Path(__file__).resolve().parent
DATASET = ROOT / "eval/datasets/assignment_v2.json"
DEFAULT_SHEET = HERE / "out/sheet.csv"
GOLD_PATH = HERE / "assignment_gold_v2.json"

VALID_EMP_IDS = {e.id for e in employees() if e.active}
INACTIVE_EMP_IDS = {e.id for e in employees() if not e.active}
VALID_SERVICE_IDS = set(services_by_id())
JUDGEABILITY = {"单值", "多值皆可", "不可判"}
FILL_COLS = ("期望指派", "是否应转人工", "可判性", "判定依据")


def is_untouched(row: dict[str, str]) -> bool:
    return not any((row.get(c) or "").strip() for c in FILL_COLS)


class SheetError(Exception):
    pass


def read_rows(path: Path) -> list[dict[str, str]]:
    with path.open(encoding="utf-8-sig", newline="") as f:
        return list(csv.DictReader(f))


def parse_assignees(cell: str) -> list[str]:
    parts = [p.strip() for p in cell.replace("｜", "|").split("|")]
    return [p for p in parts if p]


def parse_sheet(rows: list[dict[str, str]]) -> dict[str, list[str]]:
    """返回 {case_id: [错误, ...]}；空列表表示该行合法。"""
    problems: dict[str, list[str]] = {}
    for row in rows:
        cid = (row.get("case_id") or "").strip()
        errs: list[str] = []
        assignees = parse_assignees(row.get("期望指派", ""))
        escalate_cell = (row.get("是否应转人工") or "").strip()
        judge_cell = (row.get("可判性") or "").strip()
        rationale = (row.get("判定依据") or "").strip()
        service_cell = (row.get("涉及服务") or "").strip()

        if not cid:
            errs.append("缺 case_id")
        if not assignees:
            errs.append("「期望指派」留空")
        for a in assignees:
            if a == ESCALATE_HUMAN:
                continue
            if not a.isdigit():
                errs.append(f"「期望指派」{a!r} 既不是员工号也不是 {ESCALATE_HUMAN}")
            elif int(a) in INACTIVE_EMP_IDS:
                errs.append(f"{a} 已离职（RUBRIC §1.1），不能作为答案")
            elif int(a) not in VALID_EMP_IDS:
                errs.append(f"{a} 不在名册里")
        if ESCALATE_HUMAN in assignees and len(assignees) > 1:
            errs.append(f"{ESCALATE_HUMAN} 不能与具体员工号并列：{assignees}")

        if escalate_cell not in {"是", "否"}:
            errs.append(f"「是否应转人工」必须是 是/否，当前 {escalate_cell!r}")
        else:
            escalated = assignees == [ESCALATE_HUMAN]
            if escalated != (escalate_cell == "是"):
                errs.append(
                    f"「是否应转人工={escalate_cell}」与期望指派 {assignees} 矛盾",
                )

        if judge_cell not in JUDGEABILITY:
            errs.append(f"「可判性」必须是 {'/'.join(sorted(JUDGEABILITY))}，当前 {judge_cell!r}")
        elif judge_cell == "不可判" and assignees != [ESCALATE_HUMAN]:
            errs.append(f"标了「不可判」却给出了具体指派 {assignees}")
        elif judge_cell == "单值" and len(assignees) > 1:
            errs.append(f"「可判性=单值」却给了多个指派 {assignees}")
        elif judge_cell == "多值皆可" and len(assignees) < 2:
            errs.append(f"「可判性=多值皆可」却只给了一个指派 {assignees}")

        if not rationale:
            errs.append("「判定依据」留空 —— 金标的可复现依据就是这一句")

        if service_cell and service_cell != "无":
            if not service_cell.isdigit() or int(service_cell) not in VALID_SERVICE_IDS:
                errs.append(f"「涉及服务」{service_cell!r} 不是服务号也不是 无")

        problems.setdefault(cid, []).extend(errs)
    return problems


def check_tamper(rows: list[dict[str, str]], cases: list[dict]) -> list[str]:
    by_id = {c["id"]: c for c in cases}
    ids = [r["case_id"].strip() for r in rows]
    msgs: list[str] = []
    if len(set(ids)) != len(ids):
        msgs.append("表内 case_id 有重复")
    if set(ids) != set(by_id):
        msgs.append(
            f"行集与数据集不符：缺 {sorted(set(by_id) - set(ids))} "
            f"多 {sorted(set(ids) - set(by_id))}",
        )
    for row in rows:
        case = by_id.get(row["case_id"].strip())
        if case is None:
            continue
        ticket = case["ticket"]
        for col, want in (
            ("标题", ticket["title"]),
            ("描述", ticket["description"]),
            ("类别", ticket["category"]),
            ("优先级", ticket["priority"]),
        ):
            got = (row.get(col) or "").strip()
            if got != want.strip():
                msgs.append(f"{row['case_id']} 只读列「{col}」被改动，应为 {want!r}")
    return msgs


def self_agreement(rows: list[dict[str, str]], recheck: list[dict[str, str]]) -> str:
    first = {r["case_id"].strip(): r for r in rows}
    compare_cols = ["期望指派", "是否应转人工", "可判性", "涉及服务"]
    agree = total = 0
    detail = []
    for r in recheck:
        cid = r["case_id"].strip()
        base = first.get(cid)
        if base is None:
            detail.append(f"{cid} 不在主表中")
            continue
        for col in compare_cols:
            a, b = (base.get(col) or "").strip(), (r.get(col) or "").strip()
            if not a or not b:
                detail.append(f"{cid} 「{col}」有一侧留空，不计入")
                continue
            total += 1
            same = set(parse_assignees(a)) == set(parse_assignees(b)) if "指派" in col else a == b
            agree += same
            if not same:
                detail.append(f"{cid} 「{col}」{a!r} → {b!r}")
    if total == 0:
        return "自一致性：无可比行（两次都没填）"
    return (
        f"自一致性 self-agreement = {agree}/{total} = {agree / total:.1%}"
        f"（单人复标，不是跨人 κ）\n" + "\n".join(f"  {d}" for d in detail)
    )


def freeze(rows: list[dict[str, str]], cases: list[dict]) -> dict:
    by_id = {c["id"]: c for c in cases}
    out_cases = []
    for row in rows:
        cid = row["case_id"].strip()
        assignees = parse_assignees(row.get("期望指派", ""))
        service_cell = (row.get("涉及服务") or "").strip()
        out_cases.append(
            {
                "id": cid,
                "ticket": by_id[cid]["ticket"],
                "gold": {
                    "assigneeIds": [int(a) for a in assignees if a.isdigit()],
                    "escalate": assignees == [ESCALATE_HUMAN],
                    "judgeability": (row.get("可判性") or "").strip(),
                    "rationale": (row.get("判定依据") or "").strip(),
                    "serviceHint": int(service_cell) if service_cell.isdigit() else None,
                },
            },
        )
    return {
        "version": 1,
        "goldSource": "human-blinded-single-annotator",
        "rubric": "eval/annotation/RUBRIC.md",
        "derivedFrom": "assignment_v2.json (inputs only; no formula gold reused)",
        "nCases": len(out_cases),
        "goldBoundary": (
            "45 条 ⟹ 每条粒度 2.2pt，低于本仓库「金标集 <100 条不得当结论」门槛，"
            "指派准确率只作方向性证据；单人标注只能给 self-agreement，不是跨人 κ；"
            "表内不含任何系统预测，员工顺序随机（seed=20260925）。"
        ),
        "cases": out_cases,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--sheet", type=Path, default=DEFAULT_SHEET)
    ap.add_argument("--recheck", type=Path)
    ap.add_argument("--out", type=Path, default=GOLD_PATH)
    ap.add_argument("--freeze", action="store_true")
    args = ap.parse_args()

    if not args.sheet.exists():
        raise SheetError(f"{args.sheet} 不存在 —— 先跑 `make annotate`")
    cases = json.loads(DATASET.read_text(encoding="utf-8"))["cases"]
    rows = read_rows(args.sheet)
    untouched = [r for r in rows if is_untouched(r)]
    filled_rows = [r for r in rows if not is_untouched(r)]

    tamper = check_tamper(rows, cases)
    for line in tamper:
        print(f"[篡改] {line}")
    problems = parse_sheet(filled_rows)
    bad = {cid: errs for cid, errs in problems.items() if errs}
    for cid, errs in sorted(bad.items()):
        for e in errs:
            print(f"[{cid}] {e}")

    if args.recheck:
        if not args.recheck.exists():
            raise SheetError(f"{args.recheck} 不存在")
        print(self_agreement(rows, read_rows(args.recheck)))

    print(f"行数={len(rows)} 已开始填={len(filled_rows)} 未填={len(untouched)} 有错行={len(bad)}")
    if bad or tamper or untouched:
        return 1
    if args.freeze:
        args.out.write_text(
            json.dumps(freeze(rows, cases), ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        print(f"已冻结 → {args.out}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SheetError as e:
        print(f"错误：{e}", file=sys.stderr)
        sys.exit(2)
