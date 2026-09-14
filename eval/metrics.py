"""
评测指标计算

提供意图分类、信息提取、工程师匹配等评测的指标计算函数。
"""

from typing import List, Dict, Any
from collections import Counter


def accuracy(y_true: List[str], y_pred: List[str]) -> float:
    """准确率"""
    if not y_true:
        return 0.0
    correct = sum(1 for t, p in zip(y_true, y_pred) if t == p)
    return correct / len(y_true)


def macro_f1(y_true: List[str], y_pred: List[str]) -> float:
    """宏平均 F1（多分类）"""
    labels = set(y_true) | set(y_pred)
    f1s = []
    for label in labels:
        tp = sum(1 for t, p in zip(y_true, y_pred) if t == label and p == label)
        fp = sum(1 for t, p in zip(y_true, y_pred) if t != label and p == label)
        fn = sum(1 for t, p in zip(y_true, y_pred) if t == label and p != label)
        precision = tp / (tp + fp) if (tp + fp) else 0.0
        recall = tp / (tp + fn) if (tp + fn) else 0.0
        f1 = 2 * precision * recall / (precision + recall) if (precision + recall) else 0.0
        f1s.append(f1)
    return sum(f1s) / len(f1s) if f1s else 0.0


def field_accuracy(preds: List[Dict[str, str]], truths: List[Dict[str, str]], field: str) -> float:
    """字段级准确率"""
    if not truths:
        return 0.0
    correct = sum(1 for p, t in zip(preds, truths) if p.get(field) == t.get(field))
    return correct / len(truths)


def top_k_accuracy(matched: List[str], expected: List[str], k: int = 1) -> float:
    """Top-K 匹配准确率"""
    if not expected:
        return 0.0
    correct = sum(1 for m, e in zip(matched, expected) if m == e)
    return correct / len(expected)
