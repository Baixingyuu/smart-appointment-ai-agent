#!/usr/bin/env python3
"""从真实 IT 工单语料分层抽样，生成「真实数据观测」数据集。

数据来源
--------
GitHub: NajaChandran/IT-Helpdesk-Analysis 仓库内的
`all_tickets_processed_improved_v3.csv`（47,837 条真实 IT 工单，8 个类别）。
该仓库未声明许可证，且文件 14MB，因此 **CSV 不入库**，只入库本脚本与抽样结果
（抽样结果保留原始英文文本，仅用于本地观测）。

为什么要用真实数据
------------------
自建数据集的消息是我们构造的，形态分布与真实工单不一致（此前实测：
真实 reporter 消息 59% ≤20 字符）。用真实工单文本跑一遍，观察的是
「系统在分布外输入上的行为」，而不是准确率——真实工单没有金标，
因此本数据集**不参与打分**，只供 eval-realtickets 做行为观测。

抽样策略
--------
按 Topic_group 分层各取 N 条，固定随机种子保证可复现；
过滤掉过短（<30 字符）或几乎不含字母的噪声样本。
注意：原始文本已被上游脱敏（技术名词被 icon / work experience user 等
占位符替换），语义不完整——这一点是数据本身的属性，不是抽样引入的。

用法
----
  python3 eval/gen_realtickets.py /path/to/all_tickets_processed_improved_v3.csv
  # 默认每类 5 条（共 40 条），可用 --per-group 调整
"""

import argparse
import csv
import json
import pathlib
import random
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "eval" / "datasets" / "realtickets.json"

# 入库的类别顺序固定，避免因抽样随机性导致数据集 diff 抖动。
TOPIC_ORDER = [
    "Hardware", "HR Support", "Access", "Miscellaneous",
    "Storage", "Purchase", "Internal Project", "Administrative rights",
]


def is_usable(text):
    """过滤掉不适合作为模型输入的样本。

    过短（<30 字符）无法构成有效诉求；纯符号/数字的条目通常是被脱敏
    截断的残片，喂给模型只会产生噪声结论。
    """
    text = text.strip()
    if len(text) < 30:
        return False
    letters = sum(1 for ch in text if ch.isalpha())
    return letters >= 15


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("csv_path", help="all_tickets_processed_improved_v3.csv 的路径")
    parser.add_argument("--per-group", type=int, default=5, help="每个类别抽取条数")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--out", default=str(OUT))
    args = parser.parse_args()

    csv_path = pathlib.Path(args.csv_path)
    if not csv_path.exists():
        print(f"未找到 CSV: {csv_path}", file=sys.stderr)
        print("可从 https://codeload.github.com/NajaChandran/IT-Helpdesk-Analysis/tar.gz/refs/heads/main 获取",
              file=sys.stderr)
        sys.exit(1)

    with csv_path.open(encoding="utf-8", errors="replace") as handle:
        rows = list(csv.DictReader(handle))

    buckets = {}
    for row in rows:
        topic = row.get("Topic_group", "").strip()
        text = (row.get("Document") or "").strip().replace("\n", " ")
        if topic and is_usable(text):
            buckets.setdefault(topic, []).append(text)

    rng = random.Random(args.seed)
    cases = []
    for index, topic in enumerate(TOPIC_ORDER):
        pool = sorted(buckets.get(topic, []))  # 先排序再抽样：与 CSV 行序解耦
        if not pool:
            continue
        picked = rng.sample(pool, min(args.per_group, len(pool)))
        for offset, text in enumerate(picked):
            cases.append({
                "id": f"rt-{index + 1:02d}{offset + 1:02d}",
                "topicGroup": topic,
                "text": text,
            })

    payload = {
        "version": 1,
        "description": (
            "真实 IT 工单语料抽样（来源：GitHub NajaChandran/IT-Helpdesk-Analysis 的 "
            "all_tickets_processed_improved_v3.csv，47,837 条真实工单，8 个类别）。"
            "用途是**行为观测**而非打分：真实工单没有金标，因此本数据集不参与任何通过率计算，"
            "只回答一个问题——系统在分布外（英文、真实措辞、语义被上游脱敏）的输入上会怎么表现。"
            "注意：原始文本中的技术名词已被上游脱敏（icon 等占位符），语义不完整。"
        ),
        "source": "NajaChandran/IT-Helpdesk-Analysis",
        "cases": cases,
    }
    out_path = pathlib.Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"已生成 {len(cases)} 条真实工单样本: {out_path}")
    for topic in TOPIC_ORDER:
        count = sum(1 for c in cases if c["topicGroup"] == topic)
        print(f"  {topic:<22} {count}")


if __name__ == "__main__":
    main()
