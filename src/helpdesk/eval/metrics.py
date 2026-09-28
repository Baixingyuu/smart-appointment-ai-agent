"""四轴指标的纯函数层 —— 框架给仪表（事件流 + 结构化输出），量表在这里自研。

安装包 2.0.8 内确认无 `agentscope.evaluate`（1.x 文档里那套 Task/Metric/Evaluator 已随
版本移除）；框架自带 pass/fail 语义的只有 `GoalPipeline`，而它的 verifier 是模型判的。
所以判分分两处：红线用代码（`gates.py`，能机器断言的一律断言），无金标可断言的质量维度
用 py-openjudge 的 grader（`judge.py`）。两边都在报告里留痕：红线看退出码，评委看
`judgeModel` —— 评委默认与被评者同模型，自偏好这件事不藏。
"""
from __future__ import annotations

from collections import Counter
from dataclasses import dataclass, field


@dataclass(frozen=True)
class ExtractionCase:
    """层 1 一条结果。gold=None 是人工判空（"这单没有匹配服务"），不是缺失。"""

    case_id: str
    gold: int | None
    ranked: tuple[tuple[int, float], ...]

    @property
    def ids(self) -> tuple[int, ...]:
        return tuple(sid for sid, _ in self.ranked)

    @property
    def top1_score(self) -> float | None:
        return self.ranked[0][1] if self.ranked else None


@dataclass(frozen=True)
class Miss:
    case_id: str
    gold: int | None
    predicted: int | None
    predicted_score: float | None
    gold_score: float | None


@dataclass(frozen=True)
class ExtractionReport:
    n: int
    n_named: int
    top1_accuracy: float
    recall_at: dict[int, float]
    null_gold_top1_scores: list[float]
    misses: list[Miss]
    cases: list[ExtractionCase] = field(default_factory=list, repr=False)

    def as_dict(self) -> dict:
        return {
            "n": self.n,
            "nNamedGold": self.n_named,
            "top1Accuracy": round(self.top1_accuracy, 4),
            "recallAt": {str(k): round(v, 4) for k, v in self.recall_at.items()},
            "nullGoldTop1Scores": [round(s, 4) for s in self.null_gold_top1_scores],
            "granularityPt": round(100 / self.n_named, 2) if self.n_named else None,
            "misses": [m.__dict__ for m in self.misses],
        }


def score_extraction(cases: list[ExtractionCase], ks: tuple[int, ...] = (1, 3)) -> ExtractionReport:
    named = [c for c in cases if c.gold is not None]
    hits = 0
    per_k = {k: 0 for k in ks}
    misses: list[Miss] = []
    for c in named:
        if c.ids and c.ids[0] == c.gold:
            hits += 1
        for k in ks:
            if c.gold in c.ids[:k]:
                per_k[k] += 1
        if not c.ids or c.ids[0] != c.gold:
            gold_score = next((s for sid, s in c.ranked if sid == c.gold), None)
            misses.append(
                Miss(
                    case_id=c.case_id,
                    gold=c.gold,
                    predicted=c.ids[0] if c.ids else None,
                    predicted_score=c.top1_score,
                    gold_score=gold_score,
                ),
            )
    n = len(named)
    return ExtractionReport(
        n=len(cases),
        n_named=n,
        top1_accuracy=hits / n if n else 0.0,
        recall_at={k: per_k[k] / n if n else 0.0 for k in ks},
        null_gold_top1_scores=[c.top1_score for c in cases if c.gold is None and c.top1_score is not None],  # noqa: E501
        misses=misses,
        cases=cases,
    )


@dataclass(frozen=True)
class RetrievalCase:
    query_id: str
    gold_doc_ids: frozenset[str]
    ranked: tuple[tuple[str, float], ...]

    @property
    def answerable(self) -> bool:
        return bool(self.gold_doc_ids)

    @property
    def top1_score(self) -> float:
        return self.ranked[0][1] if self.ranked else 0.0


@dataclass(frozen=True)
class RetrievalReport:
    n: int
    n_answerable: int
    n_unanswerable: int
    recall_at: dict[int, float]
    mrr: float
    unanswerable_top1: list[float]
    threshold_sweep: list[dict]

    def as_dict(self) -> dict:
        return {
            "n": self.n,
            "nAnswerable": self.n_answerable,
            "nUnanswerable": self.n_unanswerable,
            "recallAt": {str(k): round(v, 4) for k, v in self.recall_at.items()},
            "mrr": round(self.mrr, 4),
            "unanswerableTop1": {
                "min": round(min(self.unanswerable_top1), 4),
                "p50": _pct(self.unanswerable_top1, 0.5),
                "max": round(max(self.unanswerable_top1), 4),
            },
            "thresholdSweep": self.threshold_sweep,
        }


def _pct(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    idx = min(len(s) - 1, max(0, round(q * (len(s) - 1))))
    return round(s[idx], 4)


def score_retrieval(
    cases: list[RetrievalCase],
    ks: tuple[int, ...] = (1, 3, 5),
    thresholds: tuple[float, ...] = (),
) -> RetrievalReport:
    answerable = [c for c in cases if c.answerable]
    unanswerable = [c for c in cases if not c.answerable]
    n = len(answerable)
    per_k = {k: 0 for k in ks}
    rr = 0.0
    for c in answerable:
        ids = [d for d, _ in c.ranked]
        for k in ks:
            if set(ids[:k]) & c.gold_doc_ids:
                per_k[k] += 1
        for rank, d in enumerate(ids, start=1):
            if d in c.gold_doc_ids:
                rr += 1.0 / rank
                break
    sweep = [
        {
            "threshold": t,
            "answerable_pass": sum(1 for c in answerable if c.top1_score >= t) / n if n else 0,
            "unanswerable_leak": sum(1 for c in unanswerable if c.top1_score >= t) / len(unanswerable)
            if unanswerable
            else 0,
        }
        for t in thresholds
    ]
    return RetrievalReport(
        n=len(cases),
        n_answerable=n,
        n_unanswerable=len(unanswerable),
        recall_at={k: per_k[k] / n if n else 0.0 for k in ks},
        mrr=rr / n if n else 0.0,
        unanswerable_top1=[c.top1_score for c in unanswerable],
        threshold_sweep=sweep,
    )


def flip_rate(runs: list[list[str]]) -> dict:
    """层 2 的方差：同输入跑 N 次。

    flipRate = 每条案例的不同答案数-1 再除以 N-1 的均值 —— 0 表示从没翻过，
    1 表示每跑都换个人；unanimousShare / modalShare 给出绝对一致的比例。
    """
    if not runs:
        return {"runs": 0, "cases": 0, "flipRate": 0.0, "unanimousShare": 0.0, "modalShare": 0.0}
    if len({len(r) for r in runs}) > 1:
        raise ValueError("每次跑必须覆盖同一批输入且顺序一致")
    per_case = list(zip(*runs))
    n = len(per_case)
    denom = len(runs) - 1
    flips = 0.0 if denom == 0 else sum((len(set(a)) - 1) / denom for a in per_case) / n
    unanimous = sum(1 for a in per_case if len(set(a)) == 1) / n if n else 0.0
    modal = sum(Counter(a).most_common(1)[0][1] / len(a) for a in per_case) / n if n else 0.0
    return {
        "runs": len(runs),
        "cases": n,
        "flipRate": round(flips, 4),
        "unanimousShare": round(unanimous, 4),
        "modalShare": round(modal, 4),
    }
