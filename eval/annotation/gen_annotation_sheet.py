#!/usr/bin/env python3
"""生成指派轴人工标注空白表。

目的：现有 150 条指派数据集的金标由派单规则推导（gold_source: derived），
它只能证明「实现与规格一致」，无法证明「规则本身符合人的判断」。
本脚本把规则金标完全遮蔽，产出一份空白标注表，由人工独立标注
「该派谁」，再用 compare_annotation.py 对比出规则-人工一致率。

刻意不输出以下字段（避免锚定标注者）：
  - expect.goldAssigneeId / noMatch / rationale
标注表中每个员工保留完整技能与负载数据，标注者按自己的业务判断选择。

用法：
  python3 eval/annotation/gen_annotation_sheet.py
  # 产出 eval/annotation/assignment_annotation_blank.json
"""

import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
DATASETS = [
    ROOT / "eval" / "datasets" / "assignment.json",
    ROOT / "eval" / "datasets" / "assignment_hard.json",
]
OUT = ROOT / "eval" / "annotation" / "assignment_annotation_blank.json"

# 与数据集 description 保持一致的技能编号映射（仅供标注者速查）。
SKILL_NAMES = {
    1: "网络", 2: "接口", 3: "数据库", 4: "性能", 5: "安全",
    6: "前端", 7: "客户端", 8: "部署", 9: "账号权限", 10: "计费",
    11: "数据同步", 12: "硬件",
}


def skill_names(ids):
    return "/".join(SKILL_NAMES.get(i, str(i)) for i in ids)


def main():
    sheet = {
        "version": 1,
        "description": (
            "指派轴人工金标标注表。填写规则：每条 case 在 annotate.assigneeId "
            "填你认为最合适的员工 id；若认为没有合适人选，填 annotate.noMatch = true "
            "并在 note 写原因。只依据业务判断（谁更有经验处理该问题、负载是否可接受），"
            "不要试图推算任何公式。annotationStatus: pending/annotated/unsure。"
            "标注完成后运行 compare_annotation.py 得出规则-人工一致率。"
        ),
        "skillLegend": SKILL_NAMES,
        "cases": [],
    }

    total = 0
    for path in DATASETS:
        if not path.exists():
            print(f"未找到数据集: {path}", file=sys.stderr)
            sys.exit(1)
        data = json.loads(path.read_text(encoding="utf-8"))
        for case in data["cases"]:
            total += 1
            entry = {
                "id": case["id"],
                "dataset": path.stem,
                "scenario": case["scenario"],
                "ticket": {
                    "title": case["ticket"]["title"],
                    "category": case["ticket"]["category"],
                    "priority": case["ticket"]["priority"],
                    "requiredSkills": skill_names(case["ticket"]["requiredSkillIds"]),
                },
                "employees": [
                    {
                        "id": e["id"],
                        "name": e["name"],
                        "skills": skill_names(e["skillIds"]),
                        "active": e["active"],
                        "load": f'{e["currentLoad"]}/{e["maxConcurrent"]}',
                        "recency": e["recency"],
                    }
                    for e in case["employees"]
                ],
                # 人工填写区
                "annotationStatus": "pending",
                "annotate": {
                    "assigneeId": None,   # 员工 id；认为无人合适则保持 None
                    "noMatch": False,      # true = 都不合适（进待认领池）
                    "note": "",            # 判断理由（可选，用于后续分析分歧）
                },
            }
            sheet["cases"].append(entry)

    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(sheet, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"已生成 {total} 条空白标注表: {OUT}")
    print("标注建议：先标 assignment_hard 的 30 条 + 基础集抽样 30 条，即可得出一致率结论。")


if __name__ == "__main__":
    main()
