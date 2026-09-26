"""文本匹配原语：归一化 → 子串 → 字元覆盖率。

漏斗第一级，不花任何模型调用。Go 实测子串命中的样本一次模型调用都不花，
所以这层留在自研：删掉它等于每个草案多付一次模型调用（成本轴直接恶化）。
"""
from __future__ import annotations

import re
import unicodedata

_WS = re.compile(r"[\s\u3000]+")
_PUNCT = re.compile(r"[，。、！？；：,.!?;:\"'“”‘’()（）\[\]【】《》<>~`@#\$%\^&\*\-\_=\\|/{}]+")

COVERAGE_GATE = 0.70


def normalize(text: str) -> str:
    text = unicodedata.normalize("NFKC", text).lower()
    text = _PUNCT.sub("", text)
    return _WS.sub("", text)


def is_substring(quote: str, source: str) -> bool:
    q = normalize(quote)
    return bool(q) and q in normalize(source)


def char_coverage(quote: str, source: str) -> float:
    """quote 的字元有多大比例出现在 source 里（多重集合，不看顺序）。"""
    q, s = normalize(quote), normalize(source)
    if not q:
        return 0.0
    pool: dict[str, int] = {}
    for ch in s:
        pool[ch] = pool.get(ch, 0) + 1
    hit = 0
    for ch in q:
        if pool.get(ch, 0) > 0:
            pool[ch] -= 1
            hit += 1
    return hit / len(q)


def matches(quote: str, source: str, gate: float = COVERAGE_GATE) -> bool:
    """子串命中直接过；否则要求字元覆盖率过闸（容忍改序/漏字）。"""
    return is_substring(quote, source) or char_coverage(quote, source) >= gate
