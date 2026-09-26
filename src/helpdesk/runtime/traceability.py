"""槽位证据可追溯：模型说"这个槽位已经填了"，必须给逐字引文，引文要能在用户原话里查到。

留自研的接缝证据：归一化 + 子串是零成本的第一级闸，Go 实测子串命中的样本一次模型调用
都不花；子串不中时再退一级用字元覆盖率判"是不是同一句话的改写"，仍然不调模型。
删掉这层 = 每次建单多付一次蕴含判定的模型调用。
"""
from __future__ import annotations

from dataclasses import dataclass

from ..domain import ExtractedSlot
from .textmatch import COVERAGE_GATE, char_coverage, is_substring

VERBATIM = "verbatim"
PARAPHRASE = "paraphrase"
UNVERIFIED = "unverified"


@dataclass(frozen=True)
class Verdict:
    slot: str
    quote: str
    status: str
    score: float

    @property
    def accepted(self) -> bool:
        return self.status is not UNVERIFIED


def user_texts(state: object) -> tuple[str, ...]:
    """用户原话，只取 role=user 的消息文本。

    state 是框架注入的 AgentState（`is_state_injected=True` ⇒ kwargs["_agent_state"]），
    这里只读它的 context，不回写。
    """
    out: list[str] = []
    for msg in getattr(state, "context", ()):  # type: ignore[attr-defined]
        if msg.role != "user":
            continue
        text = msg.get_text_content() or ""
        if text.strip():
            out.append(text)
    return tuple(out)


def check_quote(quote: str, texts: tuple[str, ...]) -> tuple[str, float]:
    if not quote.strip():
        return UNVERIFIED, 0.0
    if any(is_substring(quote, t) for t in texts):
        return VERBATIM, 1.0
    best = max((char_coverage(quote, t) for t in texts), default=0.0)
    return (PARAPHRASE, best) if best >= COVERAGE_GATE else (UNVERIFIED, best)


def verify_slots(
    slots: tuple[ExtractedSlot, ...],
    texts: tuple[str, ...],
) -> tuple[tuple[Verdict, ...], tuple[str, ...]]:
    """返回逐条判定 + 通过校验的槽位名。判不过的槽位当没填过，不拦整次调用。"""
    verdicts: list[Verdict] = []
    for s in slots:
        status, score = check_quote(s.quote, texts)
        verdicts.append(Verdict(slot=s.name, quote=s.quote, status=status, score=score))
    out = tuple(verdicts)
    filled = tuple(v.slot for v in out if v.accepted)
    return out, filled


__all__ = [
    "PARAPHRASE",
    "UNVERIFIED",
    "VERBATIM",
    "Verdict",
    "check_quote",
    "user_texts",
    "verify_slots",
]
