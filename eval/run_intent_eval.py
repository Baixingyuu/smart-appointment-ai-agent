"""
意图分类评测（需 LLM / API Key）

评测 Task Classification Agent 的意图分类准确率。
运行前请先配置 .env（MODEL_PROVIDER、LLM_API_KEY 等）。

运行：python eval/run_intent_eval.py
"""

import json
import asyncio
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from config.model_provider import create_chat_model
from agents.task_classification.task_classifier import TaskClassifier
from metrics import accuracy, macro_f1


async def main():
    dataset_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "datasets", "intent_classification.json")
    with open(dataset_path, "r", encoding="utf-8") as f:
        data = json.load(f)

    classifier = TaskClassifier(create_chat_model(temperature=0))

    y_true = [d["label"] for d in data]
    y_pred = []

    for d in data:
        pred = await classifier.classify_task(d["text"])
        y_pred.append(pred)

    acc = accuracy(y_true, y_pred)
    f1 = macro_f1(y_true, y_pred)

    print("=" * 60)
    print(f"意图分类评测（{len(data)} 条标注数据）")
    print("=" * 60)
    print(f"准确率 Accuracy：{acc * 100:.1f}%")
    print(f"宏平均 F1：     {f1 * 100:.1f}%")

    # 分类别准确率
    from collections import Counter, defaultdict
    correct = defaultdict(int)
    total = Counter()
    for t, p in zip(y_true, y_pred):
        total[t] += 1
        if t == p:
            correct[t] += 1
    print("\n分类别准确率：")
    for label in sorted(total):
        print(f"  {label}: {correct[label]}/{total[label]} = {correct[label]/total[label]*100:.1f}%")
    print("=" * 60)


if __name__ == "__main__":
    asyncio.run(main())
