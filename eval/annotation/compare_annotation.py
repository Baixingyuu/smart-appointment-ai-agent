#!/usr/bin/env python3
"""对比人工标注与规则推导金标，产出「规则-人工一致率」。

这是指派轴第一个能对外讲的完整数字的来源：
  「派单器 100% 实现了规则（现有 eval 结论），规则本身 X% 符合人的判断（本脚本）。」

只统计 annotationStatus == "annotated" 的条目；unsure 的条目单独列出
（它们本身就是规则口径模糊的证据，不计入分母）。

用法：
  python3 eval/annotation/compare_annotation.py [标注表路径]
  # 默认读取 eval/annotation/assignment_annotation_blank.json（标注后的版本）
"""

import json
import pathlib
import sys
from collections import defaultdict

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
DATASETS = [
    ROOT / "eval" / "datasets" / "assignment.json",
    ROOT / "eval" / "datasets" / "assignment_hard.json",
]


def load_derived_gold():
    """读取规则推导金标：case_id -> (gold_assignee_id, no_match)。"""
    gold = {}
    for path in DATASETS:
        data = json.loads(path.read_text(encoding="utf-8"))
        for case in data["cases"]:
            expect = case["expect"]
            gold[case["id"]] = (expect.get("goldAssigneeId"), expect.get("noMatch", False))
    return gold


def main():
    sheet_path = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else \
        ROOT / "eval" / "annotation" / "assignment_annotation_blank.json"
    if not sheet_path.exists():
        print(f"未找到标注表: {sheet_path}，先运行 gen_annotation_sheet.py", file=sys.stderr)
        sys.exit(1)

    sheet = json.loads(sheet_path.read_text(encoding="utf-8"))
    derived = load_derived_gold()

    stats = defaultdict(lambda: {"total": 0, "agree": 0})
    disagreements = []
    unsure = []
    annotated = 0

    for case in sheet["cases"]:
        status = case.get("annotationStatus", "pending")
        if status == "unsure":
            unsure.append(case["id"])
            continue
        if status != "annotated":
            continue
        annotated += 1

        human_id = case["annotate"].get("assigneeId")
        human_no_match = case["annotate"].get("noMatch", False)
        rule_id, rule_no_match = derived[case["id"]]

        agree = (human_no_match and rule_no_match) or \
                (not human_no_match and not rule_no_match and human_id == rule_id)
        key = case["dataset"]
        stats[key]["total"] += 1
        stats[case["scenario"]]["total"] += 1
        if agree:
            stats[key]["agree"] += 1
            stats[case["scenario"]]["agree"] += 1
        else:
            disagreements.append({
                "id": case["id"],
                "scenario": case["scenario"],
                "human": "noMatch" if human_no_match else human_id,
                "rule": "noMatch" if rule_no_match else rule_id,
                "note": case["annotate"].get("note", ""),
            })

    if annotated == 0:
        print("标注表中还没有 annotationStatus=annotated 的条目。")
        print("标注方法：填 annotate.assigneeId（或 noMatch=true），"
              "并把 annotationStatus 改为 annotated。")
        return

    print(f"已标注 {annotated} 条，待定(unsure) {len(unsure)} 条\n")
    overall_agree = 0
    overall_total = 0
    for scope in ("assignment", "assignment_hard"):
        if scope in stats:
            s = stats[scope]
            rate = s["agree"] / s["total"] * 100
            overall_agree += s["agree"]
            overall_total += s["total"]
            print(f"{scope:<18} {s['agree']}/{s['total']}  一致率 {rate:.1f}%")
    if overall_total:
        print(f"{'合计':<18} {overall_agree}/{overall_total}  "
              f"一致率 {overall_agree / overall_total * 100:.1f}%")

    print("\n按场景分解")
    for name, s in sorted(stats.items()):
        if name in ("assignment", "assignment_hard"):
            continue
        print(f"  {name:<28} {s['agree']}/{s['total']}  {s['agree'] / s['total'] * 100:.1f}%")

    if disagreements:
        print(f"\n分歧明细（{len(disagreements)} 条）—— 这些是权重/规则需要修的证据:")
        for d in disagreements:
            print(f"  {d['id']} [{d['scenario']}] 人工: {d['human']}  规则: {d['rule']}"
                  + (f"  备注: {d['note']}" if d["note"] else ""))
    if unsure:
        print(f"\n待定条目（规则口径模糊的证据）: {', '.join(unsure)}")


if __name__ == "__main__":
    main()
