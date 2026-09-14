"""
工单信息提取评测（需 LLM / API Key）

评测 Ticketing Agent 从自然语言中提取工单字段（标题/类型/优先级）的准确率。
运行前请先配置 .env（MODEL_PROVIDER、LLM_API_KEY 等）。

运行：python eval/run_extraction_eval.py
"""

import json
import asyncio
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from config.model_provider import create_chat_model
from agents.ticketing.input_parser import InputParser
from langchain_core.chat_history import InMemoryChatMessageHistory
from metrics import field_accuracy


async def main():
    dataset_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "datasets", "ticket_extraction.json")
    with open(dataset_path, "r", encoding="utf-8") as f:
        data = json.load(f)

    parser = InputParser(create_chat_model(temperature=0))

    preds = []
    truths = []

    for d in data:
        history = InMemoryChatMessageHistory()
        ai_content = ""
        for token in parser.parse_stream(d["text"], history):
            ai_content += token
        parsed = parser.parse_data(ai_content)

        # 归一化：LLM 可能输出 "P1 高" 等，统一取前两个字符
        priority = (parsed.get("priority") or "")[:2]
        preds.append({
            "title": parsed.get("title", ""),
            "category": parsed.get("category", ""),
            "priority": priority,
        })
        truths.append({
            "title": d["title"],
            "category": d["category"],
            "priority": d["priority"],
        })

    title_acc = field_accuracy(preds, truths, "title")
    category_acc = field_accuracy(preds, truths, "category")
    priority_acc = field_accuracy(preds, truths, "priority")

    print("=" * 60)
    print(f"工单信息提取评测（{len(data)} 条标注数据）")
    print("=" * 60)
    print(f"标题提取准确率：  {title_acc * 100:.1f}%")
    print(f"类型判定准确率：  {category_acc * 100:.1f}%")
    print(f"优先级判定准确率：{priority_acc * 100:.1f}%")
    print("=" * 60)


if __name__ == "__main__":
    asyncio.run(main())
