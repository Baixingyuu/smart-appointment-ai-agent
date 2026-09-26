"""生成 45 条人工指派盲标表（`make annotate`）。

盲标的全部意义：金标必须由被测系统之外的第二套实现产出。所以这张表里
不出现任何系统预测（serviceTop1、weakness、outcome、assigneeId 一个都不写），
员工名册按随机顺序附在表尾，只暴露运营事实与工单正文。

产出：
  eval/annotation/out/sheet.csv           主标注表（UTF-8 BOM，Numbers/Excel 可直接打开）
  eval/annotation/out/roster_shuffle.csv  随机顺序员工表（顺序不含信息）
  eval/annotation/out/resume.txt          本次 seed 与填写指引
"""
from __future__ import annotations

import argparse
import csv
import json
import random
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.catalog import employees  # noqa: E402

DATASET = ROOT / "eval/datasets/assignment_v2.json"
OUT_DIR = Path(__file__).resolve().parent / "out"
SEED = 20260925

HEADERS = [
    "case_id",
    "标题",
    "描述",
    "类别",
    "优先级",
    "涉及服务",
    "期望指派",
    "是否应转人工",
    "判定依据",
    "可判性",
]
BLANK = ["", "", "", "", "", "", "", "", "", ""]


def _cases() -> list[dict]:
    data = json.loads(DATASET.read_text(encoding="utf-8"))
    return data["cases"]


def write_sheet(cases: list[dict], path: Path) -> None:
    with path.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(HEADERS)
        for case in cases:
            ticket = case["ticket"]
            row = list(BLANK)
            row[0] = case["id"]
            row[1] = ticket["title"]
            row[2] = ticket["description"]
            row[3] = ticket["category"]
            row[4] = ticket["priority"]
            w.writerow(row)


def write_roster(rng: random.Random, path: Path) -> None:
    rows = [
        [
            e.id,
            e.name,
            e.team_id,
            e.level.value,
            "在职" if e.active else "已离职",
            f"{e.current_load}/{e.max_concurrent}",
        ]
        for e in employees()
    ]
    rng.shuffle(rows)
    with path.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(["员工号", "姓名", "团队号", "级别", "在职", "当前负载/上限"])
        w.writerows(rows)


def write_resample(cases: list[dict], rng: random.Random, path: Path) -> Path:
    """自一致性复标子表：随机抽 10 条，只给正文，字段留空。"""
    sample = rng.sample(cases, 10)
    with path.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(HEADERS)
        for case in sample:
            ticket = case["ticket"]
            row = list(BLANK)
            row[0] = case["id"]
            row[1] = ticket["title"]
            row[2] = ticket["description"]
            row[3] = ticket["category"]
            row[4] = ticket["priority"]
            w.writerow(row)
    return path


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--seed", type=int, default=SEED)
    ap.add_argument(
        "--resample",
        action="store_true",
        help="额外生成 10 条自一致性复标子表（隔一轮再填，不要看第一次的答案）",
    )
    args = ap.parse_args()

    OUT_DIR.mkdir(parents=True, exist_ok=True)
    cases = _cases()
    rng = random.Random(args.seed)

    sheet = OUT_DIR / "sheet.csv"
    write_sheet(cases, sheet)
    roster = OUT_DIR / "roster_shuffle.csv"
    write_roster(rng, roster)

    paths = [sheet, roster]
    if args.resample:
        paths.append(write_resample(cases, rng, OUT_DIR / "recheck.csv"))

    (OUT_DIR / "resume.txt").write_text(
        "填写顺序：先读 RUBRIC.md 再填表。\n"
        "先标 dp-v2-003 / 017 / 023 / 045 这 4 条 —— Go 侧重标定已确认它们过期。\n"
        "「期望指派」只接受员工号（多值用 | 分隔）或 ESCALATE_HUMAN。\n"
        "「判定依据」每行必填，一句话即可；「可判性」填 单值/多值皆可/不可判。\n"
        f"seed={args.seed}（员工表顺序是随机的，不携带任何信息）\n",
        encoding="utf-8",
    )

    print(f"cases={len(cases)} sheet={sheet.relative_to(ROOT)}")
    for p in paths:
        print("  ", p.relative_to(ROOT))
    print("参考事实表见 eval/annotation/RUBRIC.md（员工画像与服务归属）")


if __name__ == "__main__":
    main()
