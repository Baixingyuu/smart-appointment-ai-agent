"""指标数学是评测的可信度地基 —— 全部纯函数，手算可对。"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.eval.metrics import (  # noqa: E402
    ExtractionCase,
    RetrievalCase,
    flip_rate,
    score_extraction,
    score_retrieval,
)
from helpdesk.eval.run import _corpus, _load, collapse_to_docs  # noqa: E402


def ex(cid, gold, ranked):
    return ExtractionCase(case_id=cid, gold=gold, ranked=tuple(ranked))


def test_extraction_top1_and_recall() -> None:
    cases = [
        ex("a", 1, [(1, 0.9), (2, 0.8)]),
        ex("b", 2, [(1, 0.9), (2, 0.7)]),
        ex("c", 3, [(1, 0.9), (2, 0.8)]),
    ]
    r = score_extraction(cases, ks=(1, 2))
    assert r.n_named == 3
    assert r.top1_accuracy == pytest.approx(1 / 3)
    assert r.recall_at[1] == pytest.approx(1 / 3)
    assert r.recall_at[2] == pytest.approx(2 / 3)
    assert [m.case_id for m in r.misses] == ["b", "c"]
    assert r.misses[1].gold_score is None


def test_extraction_keeps_human_null_verdict_separate() -> None:
    """人工判空不是缺失：它不参与 top-1 准确率，只贡献分数分布给阈值决策。"""
    cases = [ex("a", 1, [(1, 0.9)]), ex("n", None, [(7, 0.41)]), ex("m", None, [])]
    r = score_extraction(cases, ks=(1,))
    assert (r.n, r.n_named) == (3, 1)
    assert r.top1_accuracy == 1.0
    assert r.null_gold_top1_scores == [0.41]


def test_retrieval_recall_mrr_and_sweep() -> None:
    cases = [
        RetrievalCase("q1", frozenset({"d1"}), (("d1", 0.9), ("d2", 0.7))),
        RetrievalCase("q2", frozenset({"d3"}), (("d2", 0.8), ("d3", 0.6))),
        RetrievalCase("q3", frozenset({"d4"}), (("d2", 0.5),)),
        RetrievalCase("q4", frozenset(), (("d2", 0.55),)),
        RetrievalCase("q5", frozenset(), (("d9", 0.75),)),
    ]
    r = score_retrieval(cases, ks=(1, 2), thresholds=(0.5, 0.7))
    assert (r.n_answerable, r.n_unanswerable) == (3, 2)
    assert r.recall_at[1] == pytest.approx(1 / 3)
    assert r.recall_at[2] == pytest.approx(2 / 3)
    assert r.mrr == pytest.approx((1 + 0.5) / 3)
    by_t = {row["threshold"]: row for row in r.threshold_sweep}
    assert by_t[0.5]["unanswerable_leak"] == pytest.approx(1.0)
    assert by_t[0.7]["unanswerable_leak"] == pytest.approx(0.5)
    assert by_t[0.7]["answerable_pass"] == pytest.approx(2 / 3)


def test_collapse_to_docs_keeps_best_score_and_first_order() -> None:
    ranked = collapse_to_docs([("d1", 0.6), ("d2", 0.9), ("d1", 0.8), ("d3", 0.1)])
    assert ranked == (("d1", 0.8), ("d2", 0.9), ("d3", 0.1))


def test_flip_rate_needs_aligned_runs() -> None:
    with pytest.raises(ValueError):
        flip_rate([["101", "102"], ["101"]])
    assert flip_rate([]) == {
        "runs": 0, "cases": 0, "flipRate": 0.0, "unanimousShare": 0.0, "modalShare": 0.0,
    }


def test_flip_rate_counts_distinct_answers_per_case() -> None:
    runs = [["101", "105"], ["101", "108"], ["107", "105"]]
    out = flip_rate(runs)
    assert out["runs"] == 3 and out["cases"] == 2
    assert out["unanimousShare"] == 0.0
    assert out["flipRate"] == pytest.approx(0.5)
    assert out["modalShare"] == pytest.approx(0.6667)


def test_flip_rate_one_case_stays_put() -> None:
    out = flip_rate([["101", "105"], ["101", "108"], ["101", "105"]])
    assert out["unanimousShare"] == pytest.approx(0.5)
    assert out["flipRate"] == pytest.approx(0.25)
    assert out["modalShare"] == pytest.approx(0.8333)
    assert flip_rate([["101"]])["flipRate"] == 0.0


def test_eval_corpus_adapter_reads_dataset() -> None:
    corpus = _corpus(_load("retrieval.json"))
    assert len(corpus) == 32
    assert len({c.doc_id for c in corpus}) == 15
    assert all(c.searchable_text.startswith(c.title) for c in corpus)
