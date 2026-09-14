#!/usr/bin/env python3
"""独立校验指派评测集的金标。

与 gen_hard_dataset.py 刻意分开：生成与验证若共用同一份代码，
校验就只能证明「代码等于自己」，无法发现规格理解错误。

本脚本的独立性体现在：
  1. 用 fractions.Fraction 做精确有理数运算，不复用 float 路径；
  2. 自己实现过滤与排序，不 import 生成器；
  3. 额外断言场景结构（对抗性），而不仅断言分数。

用法：
  python3 eval/verify_datasets.py                      # 校验默认两份数据集
  python3 eval/verify_datasets.py 路径1 路径2 ...
"""

import json
import os
import sys
from fractions import Fraction

# 权重以精确有理数书写，避免浮点误差影响平局判定。
W_SKILL = Fraction(60, 100)
W_LOAD = Fraction(20, 100)
W_RECENCY = Fraction(20, 100)


def jaccard(a, b):
    sa, sb = set(a), set(b)
    if not sa or not sb:
        return Fraction(0)
    return Fraction(len(sa & sb), len(sa | sb))


def load_ratio(emp):
    mc = emp["maxConcurrent"]
    if mc <= 0:
        return Fraction(0)
    ratio = Fraction(emp["currentLoad"], mc)
    return max(Fraction(0), min(Fraction(1), ratio))


def total(req, emp):
    return (
        W_SKILL * jaccard(req, emp["skillIds"])
        + W_LOAD * (1 - load_ratio(emp))
        + W_RECENCY * Fraction(str(emp["recency"]))
    )


def expected(req, employees):
    viable = [e for e in employees if e["active"] and e["currentLoad"] < e["maxConcurrent"]]
    if not viable:
        return 0, True, []
    ranked = sorted(
        viable,
        key=lambda e: (-total(req, e), -load_ratio(e), -Fraction(str(e["recency"])), e["id"]),
    )
    return ranked[0]["id"], False, ranked


def verify(path):
    with open(path, encoding="utf-8") as f:
        dataset = json.load(f)

    cases = dataset["cases"]
    mismatches = []
    struct_issues = []
    weak_samples = []

    for case in cases:
        req = case["ticket"]["requiredSkillIds"]
        emps = case["employees"]
        want_id, want_no_match, ranked = expected(req, emps)
        got_id = case["expect"]["goldAssigneeId"]
        got_no_match = case["expect"]["noMatch"]

        # JSON 里用 null 表示「无指派」，Go 反序列化到 int64 会得到 0，
        # 两者语义相同，不应视为不一致。
        if got_id is None:
            got_id = 0

        if want_id != got_id or want_no_match != got_no_match:
            mismatches.append(
                f"{case['id']}: 期望 id={want_id} noMatch={want_no_match}，"
                f"存储 id={got_id} noMatch={got_no_match}"
            )
            continue

        # 结构断言：非 noMatch 时，金标必须是最优的；且必须严格优于次优，
        # 否则该样本对排序方向不敏感，属于弱样本。
        #
        # 注意区分两类弱样本：
        #   - 可用候选人不足 2 人：无法检验「排序」，但仍然有效检验「过滤」
        #     （例如满载专才被排除、工单落到可用者手上）。因此只作为提示报告，
        #     不计为失败。
        #   - 同分但分项不同：排序不确定，属于真问题，必须失败。
        if not want_no_match:
            if len(ranked) < 2:
                weak_samples.append(
                    f"{case['id']}: 可用候选人不足 2 人（仅检验过滤，不检验排序）"
                )
            else:
                top, second = total(req, ranked[0]), total(req, ranked[1])
                if top == second:
                    # 完全平局是允许的（由 id 收敛），但必须有完全相同的分项。
                    same = (
                        jaccard(req, ranked[0]["skillIds"]) == jaccard(req, ranked[1]["skillIds"])
                        and load_ratio(ranked[0]) == load_ratio(ranked[1])
                        and ranked[0]["recency"] == ranked[1]["recency"]
                    )
                    if not same:
                        struct_issues.append(f"{case['id']}: 次优与最优同分但分项不同，排序不确定")

    return len(cases), mismatches, struct_issues, weak_samples


def main():
    base = os.path.dirname(os.path.abspath(__file__))
    if len(sys.argv) > 1:
        paths = sys.argv[1:]
    else:
        paths = [
            os.path.join(base, "datasets", "assignment.json"),
            os.path.join(base, "datasets", "assignment_hard.json"),
        ]

    failed = False
    for path in paths:
        if not os.path.exists(path):
            print(f"跳过（不存在）：{path}")
            continue
        count, mismatches, struct_issues, weak_samples = verify(path)
        name = os.path.basename(path)
        print(f"\n{name}: {count} 条样本")
        if mismatches:
            failed = True
            print(f"  ✗ 金标不一致 {len(mismatches)} 条：")
            for item in mismatches[:10]:
                print(f"      {item}")
        else:
            print("  ✓ 金标与独立实现一致（精确有理数运算）")
        if struct_issues:
            failed = True
            print(f"  ✗ 结构问题 {len(struct_issues)} 条：")
            for item in struct_issues[:10]:
                print(f"      {item}")
        else:
            print("  ✓ 结构断言通过（最优严格优于次优，或为完全平局）")
        if weak_samples:
            print(f"  ⚠ 弱样本 {len(weak_samples)} 条（仅检验过滤，不检验排序）：")
            for item in weak_samples[:5]:
                print(f"      {item}")
            if len(weak_samples) > 5:
                print(f"      ... 另有 {len(weak_samples) - 5} 条")

    print()
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
