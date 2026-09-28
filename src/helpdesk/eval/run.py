"""评测 CLI：`make eval-extract | eval-retrieval | eval-trajectory | eval-runtime | eval-judge`。

红线：报告必带 mode/model/sabotage/repeats，且**判分结果决定退出码**（0 通过 / 1 软阈值未达 /
2 红线挂掉）。以前 `_main` 无论测出什么都 `return 0`，于是"不可回答的问题被放过"这种
致命错误只会打印一行就过去，CI 挂不住。

两处反向：`--sabotage` 那档每条都是故意演坏的，退出码只由"量具有没看见"决定（看见=0）；
`runtime` 轴读的是历史落库，改不动的历史可用性错误判软、且不进分布（见 `gates._EXTERNAL_ERROR_KINDS`）。
这两档若按字面判红，`make eval` 就成了永久红灯，而红灯永久亮等于没有红灯。

判分分两处：`gates.py` 管有没有穿闸（纯代码，红线），`judge.py` 管答得好不好
（py-openjudge 的 grader，评委默认是被评者同一个 `qwen3:8b`，所以报告写 `judgeModel`）。

`assign` 轴仍空着：它的方向不能由代码判，要等 45 条人工盲标（`docs/P2_KERNEL.md`）。
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from contextlib import AsyncExitStack
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from ..catalog import KnowledgeChunk
from ..dispatch import extract_services
from ..intent import INTENT_LABELS, classify_intent
from ..knowledge import open_index
from ..knowledge import EMBEDDAGE_MODEL as EMBED_MODEL
from ..knowledge import KNOWLEDGE_SCORE_THRESHOLD
from .gates import (
    EXIT_FATAL,
    EXIT_SOFT,
    Check,
    as_rows,
    check_retrieval,
    check_trace,
    exit_code,
    render,
    sabotage_verdict,
)
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
#: 检索轴的红线读这一档 —— 与运行时用的是同一个阈值（`knowledge.py`），
#: 换一个档只是为了画阈值扫描，不拿来判达标。
PROD_THRESHOLD = float(os.environ.get("HELPDESK_EVAL_THRESHOLD", str(KNOWLEDGE_SCORE_THRESHOLD)))
MAX_LEAK = float(os.environ.get("HELPDESK_EVAL_MAX_LEAK", "0.0"))
#: 意图轴：incident 召回低于这个数就报软阈值（漏判故障 = 漏建单）。不设红线 ——
#: 分类错误是模型质量，不是"闸穿了"。
INCIDENT_RECALL_FLOOR = float(os.environ.get("HELPDESK_INTENT_INCIDENT_RECALL_FLOOR", "0.9"))

#: 金标集是从 Go 项目带过来的，工具名换了但案例与断言没换。留原名做对照，
#: 免得日后看报告以为少了个工具。
TOOL_ALIASES = {
    "rag_search": "search_knowledge",
    "ticket_create_confirm": "create_ticket",
    "ticket_find_open_by_topic": "find_open_ticket",
}


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


# --- 轨迹 / 运行时轴 --------------------------------------------------------


def _tool(name: str) -> str:
    return TOOL_ALIASES.get(name, name)


def _expect(case: dict) -> dict:
    """把 Go 口径的 expect 翻成 `gates.check_trace` 的形状。"""
    e = case.get("expect") or {}
    spec: dict[str, Any] = {
        "mustCall": [_tool(n) for n in (e.get("tools") or [])],
        "mustNotCall": [_tool(n) for n in (e.get("forbiddenTools") or [])],
        "maxRounds": e.get("maxRounds"),
        "minSequenceScore": 0.99 if e.get("orderedTools") else None,
    }
    if e.get("expectInterrupted"):
        spec["mustPark"] = [_tool(n) for n in (e.get("tools") or ["ticket_create_confirm"])]
    else:
        # 不收尾在确认闸上的案例，最后一轮必须对用户说话。park 的那几条不判 ——
        # 递确认卡的那一轮本来就没有正文。
        spec["finalTextMustNotBeEmpty"] = True
    return {k: v for k, v in spec.items() if v not in (None, [], False)}


def _reference_steps(case: dict) -> list[list[dict[str, Any]]]:
    """orderedTools → 每步一组的参考序列。

    grader 是**逐步对齐**的（参考第 i 步 对 实际第 i 步），钉成 `strict=False, jaccard=False`：
    实测乱序 0.0、漏中间一环把后面全错位（3 环只剩 1 环对上）、超出参考长度的多做不扣分。
    最后这一条是必须的 —— 我们建单之后还会 `match_service` 取推荐材料、`propose_appointments`
    拿时段，而 Go 金标止于确认建单；按集合比（jaccard）会把这段正常多做判成错，也会让乱序拿满分。
    """
    return [[{"name": _tool(n), "arguments": {}}] for n in (case["expect"].get("orderedTools") or [])]


#: 离线轴的"理想模型"该递什么参数。金标只钉工具名不钉参数，参数由这里给全，
#: 免得工具因为缺参回 error 把红线判歪。
def _canned_args(name: str, user_text: str) -> dict[str, Any]:
    if name == "search_knowledge":
        return {"query": user_text[:40]}
    if name == "find_open_ticket":
        return {"text": user_text[:40]}
    if name == "match_service":
        return {"query": user_text[:40]}
    if name == "create_ticket":
        return {
            "title": user_text[:18],
            "description": user_text,
            "category": "incident",
            "priority": "P1",
            "slots": [],
        }
    if name == "assign_ticket":
        # 只有 `--sabotage` 会走到这里：模型面已经禁掉指派，它就是那条被注入的违规。
        return {"ticket_id": 1, "assignee": "101", "rationale": "命中服务的 owner", "confidence": 0.7}
    if name == "ask_user":
        return {"slots": ["issue_description"]}
    return {}


def _perfect_turns(case: dict, *, sabotage: bool = False) -> list[Any]:
    """理想模型：按金标顺序把该调的调一遍，然后收尾。`sabotage=True` 时改成先调禁用工具。

    这一档不是在测模型，是在测**量具**：理想轨迹必须全绿，故意违规的轨迹必须让红线响。
    """
    from .scripted_model import Turn

    e = case.get("expect") or {}
    plan = [_tool(n) for n in (e.get("orderedTools") or e.get("tools") or [])]
    if sabotage:
        plan = [_tool(n) for n in (e.get("forbiddenTools") or [])][:1] + plan[1:]
    user_text = case["messages"][0]
    turns = [
        Turn(tool_calls=[(name, _canned_args(name, user_text))], id_prefix=f"p{i}")
        for i, name in enumerate(plan)
    ]
    # 框架还会再叫几次（撤工具后问出口、确认后续跑），给足收尾轮：正文不含问句就不触发红线。
    turns += [Turn(text="已登记，我跟进。", id_prefix="tail") for _ in range(4)]
    return turns


async def _drive(
    case: dict,
    *,
    repeat: int,
    live: bool,
    index: Any = None,
    sabotage: bool = False,
) -> tuple[Any, dict[str, Any]]:
    """跑一条案例，返回 (Trace, 事实)。工具、权限闸、事件流都是真代码。"""
    from ..eval.fakes import FakeIndex
    from ..eval.scripted_model import ScriptedChatModel
    from ..runtime.agent_factory import make_agent, make_chat_model
    from ..runtime.confirm_bridge import deliver
    from ..ticket_store import TicketStore
    from .runtime import attach_arguments, trace_from_events

    model_name = "scripted"
    if live:
        model = make_chat_model()
        model_name = os.environ.get("HELPDESK_CHAT_MODEL", "qwen3:8b")
    else:
        model = ScriptedChatModel(_perfect_turns(case, sabotage=sabotage))
    agent, ctx = await make_agent(index or FakeIndex(), TicketStore(), model=model)
    events: list[Any] = []
    for text in case["messages"]:
        await deliver(agent, text, on_event=events.append)
    trace = attach_arguments(
        trace_from_events(events, case_id=case["id"], repeat=repeat, model=model_name),
        agent.state.context,
    )
    facts = {
        "tools": trace.tool_names,
        "rounds": trace.rounds,
        "parked": trace.parked,
        "ticketCreated": bool(ctx.store.tickets),
    }
    return trace, facts


async def run_trajectory(
    *,
    live: bool,
    repeat: int,
    judge: bool,
    limit: int | None,
    sabotage: bool,
) -> tuple[dict[str, Any], int]:
    """轨迹 + 运行时两轴共用这一条：离线测量具，--live 测能力与延迟。"""
    from .judge import judge_meta, judge_trace, sequence_score
    from .runtime import runtime_summary

    data = _load("trajectory.json")
    cases = data["cases"][: limit or len(data["cases"])]
    rows: list[dict[str, Any]] = []
    traces: list[Any] = []
    detected: list[dict[str, Any]] = []
    missed: list[str] = []
    rc = 0
    async with AsyncExitStack() as stack:
        index = None
        if live:
            corpus = _corpus(_load("retrieval.json"))
            index = await stack.enter_async_context(
                await open_index(db_path=EVAL_DB, knowledge_collection=EVAL_KB_COLLECTION),
            )
            await index.build(recreate=True, corpus=corpus)

        for case in cases:
            for r in range(repeat):
                trace, facts = await _drive(case, repeat=r, live=live, index=index, sabotage=sabotage)
                traces.append(trace)
                spec = _expect(case)
                refs = _reference_steps(case)
                score = None
                if refs:
                    score = await sequence_score(
                        trace.openai_messages(case["messages"][0]),
                        refs,
                        strict=False,
                        jaccard=False,
                        metric="recall",
                    )
                checks = check_trace(spec, trace, score)
                expected_created = (case.get("expect") or {}).get("expectTicketCreated")
                if expected_created is not None:
                    if facts["ticketCreated"] != expected_created and facts["parked"]:
                        # 会话停在确认闸上 = 案例只给了两条消息，模型多问一轮就把"点头"那条花掉了。
                        # 这是夹具的形状，不是模型把单丢了 —— 报告里必须分得开。
                        detail = (
                            f"实际={facts['ticketCreated']} 期望={expected_created}"
                            f"（停在确认闸：{facts['parked']}，还差一次用户点头）"
                        )
                    else:
                        detail = f"实际={facts['ticketCreated']} 期望={expected_created}"
                    checks.append(
                        Check(
                            name="ticket_created",
                            ok=facts["ticketCreated"] == expected_created,
                            detail=detail,
                        ),
                    )
                judged: list[dict[str, Any]] = []
                if judge and live:
                    judged = await judge_trace(
                        query=case["messages"][0],
                        response=trace.final_text,
                        messages=trace.openai_messages(case["messages"][0]),
                        dims=("relevance", "correctness"),
                    )
                code = exit_code(checks)
                if sabotage:
                    verdict = sabotage_verdict(checks)
                    checks.append(verdict)
                    #: 注入的违规本来就该失败（那是设计好的失败），所以这一档的退出码
                    #: 只由 verdict 决定：响=0，没响=2。不然 `make eval` 永远绿不了。
                    code = exit_code([verdict])
                    fired = [c.name for c in checks if not c.ok and c is not verdict]
                    if verdict.ok:
                        detected.append({"caseId": case["id"], "fired": fired})
                    else:
                        missed.append(case["id"])
                rc = max(rc, code)
                rows.append(
                    {
                        "caseId": case["id"],
                        "scenario": case.get("scenario"),
                        "repeat": r,
                        "exit": code,
                        "sequenceScore": score,
                        "facts": facts,
                        "checks": as_rows(checks),
                        "judged": judged,
                        "trace": {
                            "wallMs": trace.wall_ms,
                            "modelMs": trace.model_ms,
                            "toolMs": trace.tool_ms,
                            "waitMs": trace.wait_ms,
                            "residualMs": trace.residual_ms,
                            "inputTokens": trace.input_tokens,
                            "outputTokens": trace.output_tokens,
                            "error": trace.error,
                            "errorKind": trace.error_kind,
                        },
                    },
                )

    payload = {
        "mode": "live" if live else "offline-scripted",
        "dataset": "trajectory.json",
        "version": data.get("version"),
        "repeat": repeat,
        "toolAliases": TOOL_ALIASES,
        "runtime": runtime_summary(traces),
        "rows": rows,
        "totals": {
            "runs": len(rows),
            "passed": sum(1 for r in rows if r["exit"] == 0),
            "softFail": sum(1 for r in rows if r["exit"] == EXIT_SOFT),
            "fatalFail": sum(1 for r in rows if r["exit"] == EXIT_FATAL),
        },
    }
    if live:
        payload["judge"] = judge_meta() if judge else {"judgeModel": None}
    else:
        #: 离线轴的 token 与延迟都是 ScriptedChatModel 里的常数，不是成本测量。
        payload["latencyIsCanned"] = True
    if sabotage:
        payload["sabotage"] = [d["caseId"] for d in detected]
        payload["sabotageDetail"] = detected
        payload["sabotageMisses"] = missed
        #: 每一条都被故意演坏过，所以这里的数字读作"量具有几条看见了"，不是"模型有几条合格"。
        payload["totals"]["detected"] = len(detected)
        payload["totals"]["blind"] = len(missed)
        payload["totals"]["note"] = "sabotage 模式：每条都注入了违规；passed=检测到的条数"
    return payload, rc


def run_runtime(db_path: Path) -> tuple[dict[str, Any], int]:
    """运行时轴：读托管路径已经落库的会话，不叫模型，只算真实流量的延迟与 token 分布。

    红线在这里只剩与模型无关的那几条 —— 确认闸有没有被绕过、工具有没有报错、
    轮次有没有失控。金标断言（该调什么工具）在轨迹轴，那需要重跑会话。

    以错误收尾的 reply 照判、照报，但**不进分布**：那种 reply 是 0 轮 0 token 一行 hint，
    混进 p50 就等于把真实会话稀释成"又快又省"。排除的条数与原因写在 `runtime.excluded*` 里。
    """
    from .runtime import runtime_summary, traces_from_messages_db

    traces = traces_from_messages_db(db_path)
    rows: list[dict[str, Any]] = []
    rc = 0
    for trace in traces:
        checks = check_trace({}, trace)
        code = exit_code(checks)
        rc = max(rc, code)
        rows.append(
            {
                "caseId": trace.case_id,
                "rounds": trace.rounds,
                "wallMs": trace.wall_ms,
                "modelMs": trace.model_ms,
                "toolMs": trace.tool_ms,
                "waitMs": trace.wait_ms,
                "residualMs": trace.residual_ms,
                "inputTokens": trace.input_tokens,
                "outputTokens": trace.output_tokens,
                "tools": trace.tool_names,
                "parked": trace.parked,
                "error": trace.error,
                "errorKind": trace.error_kind,
                "exit": code,
                "checks": as_rows(checks),
            },
        )
    measured = [t for t in traces if not t.error]
    excluded = [t for t in traces if t.error]
    summary = runtime_summary(measured)
    if excluded:
        summary["excludedReplies"] = [
            {"caseId": t.case_id, "errorKind": t.error_kind or "未分类", "error": t.error}
            for t in excluded
        ]
        summary["excludedNote"] = (
            f"{len(excluded)} 份以错误收尾的 reply 不进分布（0 轮 0 token，进 p50 会把真实流量算得又快又省）"
        )
    return {
        "mode": "hosted-sqlite",
        "dataset": str(db_path),
        "runtime": summary,
        "rows": rows,
        "totals": {
            "runs": len(rows),
            "measured": len(measured),
            "excluded": len(excluded),
            "passed": sum(1 for r in rows if r["exit"] == 0),
            "softFail": sum(1 for r in rows if r["exit"] == EXIT_SOFT),
            "fatalFail": sum(1 for r in rows if r["exit"] == EXIT_FATAL),
        },
    }, rc


async def run_intent(limit: int | None) -> tuple[dict[str, Any], int]:
    """意图轴：用真模型把 intent.json 的 180 条分一遍，报准确率与各类召回。

    这是"live"性质（每条一次模型调用），不进门禁。incident 的召回是本轴最要紧的
    数：判成别的意图 = 漏建单，所以它单独设一道软阈值（exit 1），不设红线 ——
    分类错误是模型质量，不是"闸穿了"。
    """
    from ..runtime.agent_factory import make_chat_model

    data = _load("intent.json")
    cases = data["cases"][: limit or len(data["cases"])]
    model = make_chat_model()
    rows: list[dict[str, Any]] = []
    per: dict[str, dict[str, int]] = {
        label: {"gold": 0, "pred": 0, "correct": 0} for label in INTENT_LABELS
    }
    for case in cases:
        answer = await classify_intent(model, case["text"])
        pred, gold = answer.intent, case["intent"]
        ok = pred == gold
        per[gold]["gold"] += 1
        per[pred]["pred"] += 1
        if ok:
            per[gold]["correct"] += 1
        rows.append(
            {
                "id": case["id"],
                "text": case["text"],
                "gold": gold,
                "pred": pred,
                "confidence": round(answer.confidence, 4),
                "ok": ok,
            },
        )
    total = len(rows)
    correct = sum(1 for r in rows if r["ok"])
    labels: dict[str, dict[str, Any]] = {}
    for label, s in per.items():
        labels[label] = {
            "precision": round(s["correct"] / s["pred"], 4) if s["pred"] else 0.0,
            "recall": round(s["correct"] / s["gold"], 4) if s["gold"] else 0.0,
            "gold": s["gold"],
            "pred": s["pred"],
            "correct": s["correct"],
        }
    metrics = {"accuracy": round(correct / total, 4) if total else 0.0, "n": total, "perLabel": labels}
    incident_recall = labels.get("incident", {}).get("recall", 0.0)
    checks = [
        Check(
            "incident_recall",
            incident_recall >= INCIDENT_RECALL_FLOOR,
            f"incident 召回 {incident_recall:.1%}，低于 {INCIDENT_RECALL_FLOOR:.0%} 会漏建单",
        ),
    ]
    return {
        "mode": "live",
        "dataset": "intent.json",
        "metrics": metrics,
        "checks": as_rows(checks),
        "rows": rows,
    }, exit_code(checks)


def _print_intent(payload: dict[str, Any]) -> None:
    m = payload["metrics"]
    print(f"意图轴 · 真机  样本={m['n']}  准确率={m['accuracy']:.1%}")
    for label in INTENT_LABELS:
        s = m["perLabel"][label]
        print(
            f"  {label:14} recall={s['recall']:.1%}  precision={s['precision']:.1%}  "
            f"(gold={s['gold']} pred={s['pred']} correct={s['correct']})",
        )
    for c in payload["checks"]:
        if not c["ok"]:
            print(render([Check(**c)]))


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


def _print_runtime(payload: dict[str, Any]) -> None:
    rt = payload["runtime"]
    t = payload["totals"]
    print(f"运行时轴 · 托管落库 {payload['dataset']}  样本={rt['samples']} 份有效 reply（共 {t['runs']} 份）")
    print("  （wall = model + tool + wait + residual，residual 看得见才说明没把没测的藏进总数）")
    for key in ("wallMs", "modelMs", "toolMs", "waitMs", "residualMs", "rounds", "inputTokens", "outputTokens"):
        b = rt[key]
        print(f"  {key:13} p50={_f(b['p50'])} p95={_f(b['p95'])} mean={_f(b['mean'])} max={_f(b['max'])} n={b['n']}")
    if rt.get("p95Note"):
        print(f"  ! {rt['p95Note']}")
    if rt.get("excludedNote"):
        print(f"  ! {rt['excludedNote']}")
        for row in rt["excludedReplies"]:
            print(f"      {row['caseId']}  type={row['errorKind']}  {row['error']}")
    print("  工具耗时 p95：" + ", ".join(f"{k}={v['p95']}ms" for k, v in rt["perToolMs"].items()))
    print(f"  通过 {t['passed']} / 软阈值 {t['softFail']} / 红线 {t['fatalFail']}")
    for row in payload["rows"]:
        if row["exit"]:
            mark = "红线" if row["exit"] == EXIT_FATAL else "软"
            print(f"  [{mark}] {row['caseId']}")
            print(render([Check(**c) for c in row["checks"] if not c["ok"]]))


def _print_trajectory(payload: dict[str, Any]) -> None:
    rt = payload["runtime"]
    tag = "真机" if payload["mode"] == "live" else "离线（脚本模型，延迟与 token 是常数）"
    print(f"轨迹轴 · {tag}  案例={len({r['caseId'] for r in payload['rows']})} 跑次={payload['totals']['runs']}")
    for key in ("wallMs", "modelMs", "toolMs", "inputTokens", "outputTokens"):
        b = rt[key]
        print(f"  {key:14} p50={_f(b['p50'])} p95={_f(b['p95'])} max={_f(b['max'])} n={b['n']}")
    if rt.get("p95Note"):
        print(f"  ! {rt['p95Note']}")
    print(f"  工具耗时 p95：" + ", ".join(f"{k}={v['p95']}ms" for k, v in rt["perToolMs"].items()))
    print(f"  通过 {payload['totals']['passed']} / 软阈值 {payload['totals']['softFail']} / 红线 {payload['totals']['fatalFail']}")
    if "sabotageDetail" in payload:
        #: 这一档每条都被演坏过，"通过"读作"量具看见了"。不印 fired 的话，全 0 退出码看着像没测。
        for d in payload["sabotageDetail"]:
            print(f"    [检到] {d['caseId']}  ← {', '.join(d['fired'])}")
        for case_id in payload["sabotageMisses"]:
            print(f"    [漏检（红线）] {case_id}  ← 把金标改坏了却没判出来")
    for row in payload["rows"]:
        if row.get("judged"):
            print(f"  [{'ok ' if not row['exit'] else '失败'}] {row['caseId']}  评委分："
                  + ", ".join(f"{j['grader']}={j.get('score')}" for j in row["judged"]))
        if row["exit"]:
            mark = "红线" if row["exit"] == EXIT_FATAL else "软"
            print(f"  [{mark}] {row['caseId']} r{row['repeat']} score={_f(row['sequenceScore'])}")
            print(render([Check(**c) for c in row["checks"] if not c["ok"]]))


def _print_system(payload: dict[str, Any]) -> None:
    """端到端系统评测：四个指标一屏看清。"""
    rt = payload["runtime"]
    t = payload["totals"]
    judged = [j for r in payload["rows"] for j in r.get("judged", [])]
    print("系统评测 · 端到端（真机）")
    print("=" * 48)
    print("① 回答质量（LLM 评委）")
    for dim in ("relevance", "correctness"):
        scores = [
            j["score"]
            for j in judged
            if j["grader"] == dim and isinstance(j.get("score"), (int, float))
        ]
        if scores:
            print(f"   {dim:12} 均值={sum(scores) / len(scores):.2f}  n={len(scores)}")
    if (payload.get("judge") or {}).get("judgeModel"):
        print(f"   评委={payload['judge']['judgeModel']}（与被评同模型，仅看趋势）")
    print("② 执行轨迹（闸门 + 步序）")
    seq = [
        r["sequenceScore"]
        for r in payload["rows"]
        if isinstance(r.get("sequenceScore"), (int, float))
    ]
    seq_mean = f"{sum(seq) / len(seq):.2f}" if seq else "—"
    print(f"   通过={t['passed']} 软阈值={t['softFail']} 红线={t['fatalFail']}  步序分均值={seq_mean}")
    print("③ P95 延迟（wall = model + tool + wait + residual）")
    for key in ("wallMs", "modelMs", "toolMs", "waitMs", "residualMs"):
        b = rt[key]
        print(f"   {key:11} p50={_f(b['p50'])}  p95={_f(b['p95'])}  mean={_f(b['mean'])}")
    if rt.get("p95Note"):
        print(f"   ! {rt['p95Note']}")
    print("④ Token 成本")
    for key in ("inputTokens", "outputTokens"):
        b = rt[key]
        print(f"   {key:11} p50={_f(b['p50'])}  p95={_f(b['p95'])}  mean={_f(b['mean'])}")
    for row in payload["rows"]:
        if row["exit"]:
            mark = "红线" if row["exit"] == EXIT_FATAL else "软"
            print(f"  [{mark}] {row['caseId']} r{row['repeat']}")
            print(render([Check(**c) for c in row["checks"] if not c["ok"]]))


def _f(x: float | None) -> str:
    return "—" if x is None else f"{x:.1f}"


async def _main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("axis", choices=["system", "extract", "retrieval", "assign", "trajectory", "runtime", "intent"])
    ap.add_argument("--top-k", type=int)
    ap.add_argument("--out", type=Path)
    ap.add_argument("--live", action="store_true", help="轨迹轴用真模型跑（默认为脚本模型）")
    ap.add_argument("--repeat", type=int, default=1, help="每条案例跑几次（P95 需要 ≥20）")
    ap.add_argument("--limit", type=int, help="只跑前 N 条案例")
    ap.add_argument("--judge", action="store_true", help="轨迹轴追加 openjudge 的 LLM 评委打分")
    ap.add_argument("--sabotage", action="store_true", help="跑量具自检：故意违规必须被判出来")
    ap.add_argument("--db", type=Path, default=ROOT / "data/service.db", help="运行时轴读的落库文件")
    args = ap.parse_args(argv)

    if args.axis == "assign":
        print("assign 轴等 45 条人工盲标（docs/P2_KERNEL.md 那道口），代码不代填金标", file=sys.stderr)
        return 2

    if args.axis == "system":
        # 端到端系统评测：真机跑 trajectory 全套，一次产出回答质量 + 执行轨迹 +
        # P95 延迟 + Token 成本四个指标。judge 固定开（回答质量就靠它）。
        payload, rc = await run_trajectory(
            live=True,
            repeat=max(1, args.repeat),
            judge=True,
            limit=args.limit,
            sabotage=False,
        )
        _print_system(payload)
        report = {**_header("live", axis="system", repeats=args.repeat, dataset="trajectory.json"), **payload}
        out = args.out or REPORTS / "system.json"
    elif args.axis == "intent":
        report, rc = await run_intent(args.limit)
        report.update(_header("live", axis="intent"))
        _print_intent(report)
        out = args.out or REPORTS / "intent.json"
    elif args.axis == "runtime":
        report, rc = run_runtime(args.db)
        report.update(_header("hosted-sqlite", axis="runtime", dataset=str(args.db)))
        _print_runtime(report)
        out = args.out or REPORTS / "runtime.json"
    elif args.axis == "trajectory":
        payload, rc = await run_trajectory(
            live=args.live,
            repeat=max(1, args.repeat),
            judge=args.judge,
            limit=args.limit,
            sabotage=args.sabotage,
        )
        _print_trajectory(payload)
        #: 三条轨迹报告分开：`make eval` 里 sabotage 跑在最后，共用一个文件名就会把
        #: 那条"理想模型全绿"的报告覆盖成注入违规的那份。
        name = (
            "trajectory_live.json"
            if args.live
            else ("trajectory_sabotage.json" if args.sabotage else "trajectory.json")
        )
        out = args.out or REPORTS / name
        report = {**_header(("live" if args.live else "offline-scripted"), axis="trajectory",
                            repeats=args.repeat, dataset="trajectory.json"), **payload}
    elif args.axis == "extract":
        report_obj = await run_extract(args.top_k or 3)
        report = {**_header("offline-deterministic", axis="service-extraction", topK=args.top_k or 3),
                  "dataset": "assignment_v2.json", "metrics": report_obj.as_dict(),
                  "cases": [{"caseId": c.case_id, "gold": c.gold,
                             "ranked": [[s, round(v, 4)] for s, v in c.ranked]} for c in report_obj.cases]}
        _print_extract(report_obj)
        rc = 0
        out = args.out or REPORTS / "extract.json"
    else:
        report_obj = await run_retrieval(args.top_k or 10)
        d = report_obj.as_dict()
        checks = check_retrieval(d, PROD_THRESHOLD, MAX_LEAK)
        print(render(checks))
        report = {**_header("offline-deterministic", axis="retrieval", topK=args.top_k or 10,
                            prodThreshold=PROD_THRESHOLD, maxLeak=MAX_LEAK),
                  "dataset": "retrieval.json", "metrics": d, "checks": as_rows(checks)}
        _print_retrieval(report_obj)
        rc = exit_code(checks)
        out = args.out or REPORTS / "retrieval.json"

    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    shown = out.relative_to(ROOT) if out.is_absolute() and ROOT in out.parents else out
    print(f"→ {shown}   exit={rc}")
    return rc


if __name__ == "__main__":
    raise SystemExit(asyncio.run(_main(sys.argv[1:])))
