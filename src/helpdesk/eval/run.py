"""评测 CLI：`make eval-extract` / `make eval-retrieval`。

红线：报告必带 mode/model/sabotage；确定性轴内禁 LLM-as-judge。
assign 与 trajectory 轴需要真模型或脚本模型，在 P4 接。
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from datetime import datetime, timezone
from pathlib import Path

from ..catalog import KnowledgeChunk
from ..dispatch import extract_services
from ..knowledge import open_index
from ..knowledge import EMBEDDAGE_MODEL as EMBED_MODEL
from .metrics import (
    ExtractionCase,
    ExtractionReport,
    RetrievalCase,
    RetrievalReport,
    score_extraction,
    score_retrieval,
)

ROOT = Path(__file__).resolve().parents[3]
DATASETS = ROOT / "eval/datasets"
REPORTS = ROOT / "eval/reports"
EVAL_DB = os.environ.get("HELPDESK_EVAL_DB", "./data/eval_milvus.db")
EVAL_KB_COLLECTION = "eval_knowledge"
THRESHOLDS = tuple(round(0.30 + i * 0.05, 2) for i in range(11))


def _load(name: str) -> dict:
    return json.loads((DATASETS / name).read_text(encoding="utf-8"))


def _header(mode: str, **extra: object) -> dict:
    return {
        "generatedAt": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "mode": mode,
        "embedModel": EMBED_MODEL,
        "chatModel": os.environ.get("HELPDESK_CHAT_MODEL", "qwen3:8b"),
        "sabotage": [],
        **extra,
    }


def _corpus(data: dict) -> tuple[KnowledgeChunk, ...]:
    return tuple(
        KnowledgeChunk(
            id=c["id"],
            doc_id=c["docId"],
            title=c["title"],
            content=c["content"],
            keywords=tuple(c.get("keywords", ())),
        )
        for c in data["corpus"]
    )


def collapse_to_docs(ranked: list[tuple[str, float]]) -> tuple[tuple[str, float], ...]:
    """chunk 级命中折成文档级排名：同文档保留最高分、按首次出现定序。"""
    best: dict[str, float] = {}
    order: list[str] = []
    for doc_id, score in ranked:
        if doc_id not in best:
            order.append(doc_id)
            best[doc_id] = score
        else:
            best[doc_id] = max(best[doc_id], score)
    return tuple((d, best[d]) for d in order)


async def run_extract(top_k: int) -> ExtractionReport:
    cases = _load("assignment_v2.json")["cases"]
    report_cases: list[ExtractionCase] = []
    async with await open_index(db_path=EVAL_DB) as index:
        await index.build(recreate=True)
        for case in cases:
            ticket = case["ticket"]
            text = f"{ticket['title']}。{ticket['description']}"
            extraction = await extract_services(index, text, top_k=top_k)
            gold = case["expected"]["serviceTop1"] or None
            report_cases.append(
                ExtractionCase(
                    case_id=case["id"],
                    gold=gold,
                    ranked=tuple((h.service_id, h.score) for h in extraction.hits),
                ),
            )
    return score_extraction(report_cases, ks=(1, 2, 3))


async def run_retrieval(top_k: int) -> RetrievalReport:
    data = _load("retrieval.json")
    corpus = _corpus(data)
    results: list[RetrievalCase] = []
    async with await open_index(
        db_path=EVAL_DB,
        knowledge_collection=EVAL_KB_COLLECTION,
    ) as index:
        await index.build(recreate=True, corpus=corpus)
        for q in data["queries"]:
            found = await index.search_knowledge(
                q["text"],
                top_k=top_k,
                score_threshold=None,
            )
            ranked = collapse_to_docs(
                [(r.chunk.metadata["doc_id"], r.score) for r in found],
            )
            results.append(
                RetrievalCase(
                    query_id=q["id"],
                    gold_doc_ids=frozenset(q.get("relevantDocIds") or ()),
                    ranked=ranked,
                ),
            )
    return score_retrieval(results, ks=(1, 3, 5), thresholds=THRESHOLDS)


def _print_extract(report: ExtractionReport) -> None:
    d = report.as_dict()
    print(f"层 1 服务抽取  n={d['n']} 人工具名金标={d['nNamedGold']} 粒度={d['granularityPt']}pt/条")
    print(f"  top-1 命中 {d['top1Accuracy']:.1%}  Recall@1/2/3 = "
          + " / ".join(f"{v:.1%}" for v in d["recallAt"].values()))
    if d["nullGoldTop1Scores"]:
        print(f"  人工判空({len(d['nullGoldTop1Scores'])} 条)的 top-1 分数: "
              + ", ".join(f"{s:.3f}" for s in d["nullGoldTop1Scores"]))
    print(f"  错例 {len(d['misses'])} 条：")
    for m in d["misses"]:
        print(f"    {m['case_id']} gold={m['gold']} pred={m['predicted']}"
              f"(score={_f(m['predicted_score'])}) gold_score={_f(m['gold_score'])}")


def _print_retrieval(report: RetrievalReport) -> None:
    d = report.as_dict()
    print(
        f"检索（dense）  查询={d['n']} 可回答={d['nAnswerable']} "
        f"不可回答={d['nUnanswerable']}",
    )
    print(f"  Recall@1/3/5 = " + " / ".join(f"{v:.1%}" for v in d["recallAt"].values())
          + f"   MRR={d['mrr']:.3f}")
    print(f"  不可回答查询 top-1 分数 min={d['unanswerableTop1']['min']} "
          f"p50={d['unanswerableTop1']['p50']} max={d['unanswerableTop1']['max']}")
    print("  阈值扫描（pass=可回答首命中过阈比例，leak=不可回答被放过比例）：")
    print("    thresh  pass   leak")
    for row in d["thresholdSweep"]:
        print(f"    {row['threshold']:.2f}    {row['answerable_pass']:.3f}  "
              f"{row['unanswerable_leak']:.3f}")


def _f(x: float | None) -> str:
    return "—" if x is None else f"{x:.3f}"


async def _main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("axis", choices=["extract", "retrieval", "assign", "trajectory"])
    ap.add_argument("--top-k", type=int)
    ap.add_argument("--out", type=Path)
    args = ap.parse_args(argv)

    if args.axis in {"assign", "trajectory"}:
        print(f"{args.axis} 轴需要模型，P4 接（当前只做确定性两轴）", file=sys.stderr)
        return 2

    if args.axis == "extract":
        report = await run_extract(args.top_k or 3)
        payload = {**_header("offline-deterministic", axis="service-extraction", topK=args.top_k or 3),
                   "dataset": "assignment_v2.json", "metrics": report.as_dict(),
                   "cases": [{"caseId": c.case_id, "gold": c.gold,
                              "ranked": [[s, round(v, 4)] for s, v in c.ranked]} for c in report.cases]}
        _print_extract(report)
    else:
        report = await run_retrieval(args.top_k or 10)
        payload = {**_header("offline-deterministic", axis="retrieval", topK=args.top_k or 10),
                   "dataset": "retrieval.json", "metrics": report.as_dict()}
        _print_retrieval(report)

    out = args.out or REPORTS / f"{args.axis}.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"→ {out.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(_main(sys.argv[1:])))
