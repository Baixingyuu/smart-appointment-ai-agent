"""派单 v4 的真机断言：三源召回确实跑在工具里，历史隔离确实由框架保证。

分两段，成本不同：
  A 纯检索（不调模型）—— 灌进 20 条已结语料之后，未结单仍然被读取句柄的
     `metadata_filter` 挡在外面；顺带断言 `load_history()` 幂等（框架的 insert 不去重），
     并把"候选 n/24 人""历史块多少字"作为成本观测量打出来。这一段是可复现的硬断言。
  B 一次真机 short —— v4 把候选人画像和历史条目塞进了同一个工具结果，
     token 代价必须实测，不能拿估算顶上去（P3 记录的基线是 4~5 次 / 7348~9875 in）。

跑法：.venv/bin/python probes/p4_dispatch_v4.py
"""
from __future__ import annotations

import asyncio
import logging
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
logging.getLogger("pymilvus").setLevel(logging.CRITICAL)

from agentscope.console import ConsoleRenderer  # noqa: E402
from agentscope.event import ModelCallEndEvent  # noqa: E402

from helpdesk.catalog import ROSTER  # noqa: E402
from helpdesk.dispatch import (  # noqa: E402
    assignment_prompt,
    recall_employees,
    recall_history,
    render_candidates,
    render_history,
    to_hit,
)
from helpdesk.history import HISTORY  # noqa: E402
from helpdesk.knowledge import open_index  # noqa: E402
from helpdesk.runtime.agent_factory import make_agent  # noqa: E402
from helpdesk.runtime.confirm_bridge import deliver  # noqa: E402

# 独立 DB 文件：探针要往历史集合里写假单，不能污染 data/milvus_lite.db。
PROBE_DB = os.environ.get("HELPDESK_PROBE_DB", "./data/probe_v4.db")
SEED_ID = 901
COMPLAIN = "下单接口一直在报 500，订单创建不了，帮忙看下"


async def phase_a(index, corpus_ids: set[int]) -> tuple[list[str], dict]:
    """不调模型的隔离断言：读取句柄的过滤键是唯一那道闸。"""
    fails: list[str] = []
    obs: dict = {}

    pre = await recall_history(index, "下单接口 500 连接池耗尽")
    if pre:
        fails.append(f"A0 目录重建把历史灌进来了：{[c.ticket_id for c in pre]}")

    await index.index_ticket(
        ticket_id=SEED_ID,
        title="下单接口 500",
        description="线上下单接口大量 500，排查是数据库连接池耗尽，慢查询长期占用连接。",
        assignee_id=101,
        service_id=2001,
        resolved=True,
        category="incident",
        priority="P1",
    )
    await index.index_ticket(
        ticket_id=SEED_ID + 1,
        title="下单接口 500（未结）",
        description="线上下单接口大量 500，连接池耗尽，尚未处理完。",
        assignee_id=102,
        service_id=2001,
        resolved=False,
        category="incident",
        priority="P1",
    )
    loaded = await index.load_history()
    obs["loaded"] = loaded
    if loaded != len(corpus_ids):
        fails.append(f"A1 语料没灌满：写入 {loaded}，应有 {len(corpus_ids)}")

    hist_query = "下单接口 500 连接池耗尽 订单创建失败"
    cases = await recall_history(index, hist_query)
    ids = [c.ticket_id for c in cases]
    obs["recallA"] = ids
    if not ids:
        fails.append("A1 有语料后仍然召不到历史 —— 第三源等于没接")
    if any(i not in corpus_ids | {SEED_ID} for i in ids):
        fails.append(f"A1 召回里混进了未结单：{ids}")
    if SEED_ID + 1 in ids:
        fails.append(f"A3 未结单漏进召回里：{ids}")

    again = await index.load_history()
    dup = [
        i
        for i in [c.ticket_id for c in await recall_history(index, hist_query)]
        if ids.count(i) > 1
    ]
    obs["reload"] = again
    if again != 0 or dup:
        fails.append(f"A2 load_history 不幂等：二次写入 {again} 条，重复召回 {sorted(set(dup))}")

    candidates = await recall_employees(index, "下单接口 401 令牌过期", (to_hit(2001, 0.7),))
    cand_ids = [c.id for c in candidates]
    obs["candidates"] = cand_ids
    obs["history_chars"] = len(render_history(cases))
    hits = (to_hit(2001, 0.7),)
    obs["prompt_chars"] = len(assignment_prompt(COMPLAIN, hits, candidates, cases))
    obs["per_candidate_chars"] = round(len(render_candidates(candidates)) / max(len(candidates), 1))
    if not {101, 107} <= set(cand_ids):
        fails.append(f"A4 归属候选被截掉：{cand_ids}")
    print(f"  候选 {len(cand_ids)}/{len(ROSTER)} 人：{[(c.id, '+'.join(c.reasons), c.score) for c in candidates]}")
    print(f"  历史召回：{ids}｜历史块 {obs['history_chars']} 字")
    print(
        f"  层 2 提示词载荷（确定量，不含模型方差）：{obs['prompt_chars']} 字"
        f"｜每名候选约 {obs['per_candidate_chars']} 字"
    )
    return fails, obs


async def phase_b(index) -> tuple[dict, object]:
    renderer = ConsoleRenderer(verbosity="quiet")
    agent, ctx = await make_agent(index)
    stats = {"calls": 0, "in": 0, "out": 0}

    def watch(event: object) -> None:
        renderer.render(event)
        if isinstance(event, ModelCallEndEvent):
            stats["calls"] += 1
            stats["in"] += event.input_tokens
            stats["out"] += event.output_tokens

    for line in (COMPLAIN, "确认"):
        print(f"\n=== 用户：{line}")
        await deliver(agent, line, on_event=watch)

    match = next(
        (b for msg in agent.state.context for b in msg.get_content_blocks("tool_result")
         if b.name == "match_service"),
        None,
    )
    text = ""
    meta: dict = {}
    if match is not None:
        out = match.output
        text = out if isinstance(out, str) else "".join(
            b["text"] if isinstance(b, dict) else b.text for b in out
        )
        meta = match.metadata or {}
    log = ctx.store.assignments[-1] if ctx.store.assignments else None
    print("\n=== match_service 结果（" + str(len(text)) + " 字）")
    print(text)
    print("\n=== 落库与记账")
    for t in ctx.store.tickets.values():
        print(f"  #{t.id} {t.title}｜{t.status.value}｜assignee={t.assignee_id}")
    if log is not None:
        print(
            f"  指派 #{log.ticket_id} → {log.assignee_id or '转人工'}｜"
            f"in_recall={log.in_recall}｜候选={log.employee_candidates}｜"
            f"引用={log.cited_tickets}｜conf={log.confidence}",
        )
    print(
        f"  模型 {stats['calls']} 次｜input {stats['in']} token｜output {stats['out']} token"
        f"｜match metadata={meta}",
    )
    return stats, (text, meta, log)


async def main() -> int:
    corpus_ids = {r.id for r in HISTORY}
    # seed_history=False：A0 要断言的是"重灌目录不会顺手带来历史"，灌不灌由 phase A 自己调。
    async with await open_index(db_path=PROBE_DB, seed_history=False) as index:
        # build() 从不清历史集合（它是跑出来的真实数据），探针自己要清一次才敢断言"播种前为空"。
        await index.store.delete_collection(index.tickets.collection)
        await index.ensure_ready()
        counts = await index.build(recreate=True)
        print(f"重建目录：{counts}")
        print("\n--- A：纯检索隔离（历史语料 + 一条未结单）")
        fails, obs = await phase_a(index, corpus_ids)
        for f in fails:
            print(f"  ✗ {f}")
        print("  A 结论：" + ("全部通过" if not fails else f"{len(fails)} 条不通过"))

        print("\n--- B：真机一次 short（含 v4 三源载荷）")
        stats, (text, meta, log) = await phase_b(index)

    ok = not fails
    recalled_hist = set(meta.get("history") or [])
    if not recalled_hist & (corpus_ids | {SEED_ID}):
        print(f"  ✗ B1 工具结果里没有一条来自已结语料：{sorted(recalled_hist)}")
        ok = False
    if "【召回候选人】" not in text:
        print("  ✗ B2 工具结果里没有候选人块")
        ok = False
    if log is None or log.in_recall is None:
        print("  ✗ B3 召回证据没接到指派日志（in_recall=None 等于没接）")
        ok = False
    else:
        print(f"  实跑 in_recall={log.in_recall}｜引用={log.cited_tickets}（观测值，不是闸）")
    n_cand = len(log.employee_candidates) if log else 0
    print(
        f"\nv4 成本：模型 {stats['calls']} 次｜input {stats['in']}｜output {stats['out']}"
        f"｜match_service 结果 {len(text)} 字｜候选 {n_cand}/{len(ROSTER)} 人"
        f"（离线同题 {len(obs['candidates'])}/{len(ROSTER)}）"
    )
    print(f"退出码 {0 if ok else 1}")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
