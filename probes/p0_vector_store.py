"""P0 探针①：bge-m3 + MilvusLiteStore 能否在 macOS arm64 上跑通。

要回答的不是"能不能装"，而是三件会影响架构的事：
1. 一次索引多少 embedding 调用、多少墙钟时间（决定评测能跑几轮）；
2. COSINE 分数落在什么区间（score_threshold 必须有分布依据，不能拍）；
3. Milvus Lite 的本地 .db 在异步上下文里是否可用、进程重启后能否复用。

用法：.venv/bin/python probes/p0_vector_store.py
"""
from __future__ import annotations

import asyncio
import json
import statistics
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

import logging  # noqa: E402

# pymilvus 即使异常被捕获也会往 stderr 打整段 traceback，会盖掉探针自己的输出
logging.getLogger("pymilvus").setLevel(logging.CRITICAL)

from helpdesk.knowledge import DB_PATH, cache_enabled, open_index  # noqa: E402

DATASET = Path(__file__).resolve().parents[1] / "eval/datasets/assignment_v2.json"


def load_cases(limit: int = 45) -> list[dict]:
    data = json.loads(DATASET.read_text(encoding="utf-8"))
    return data["cases"][:limit]


def histogram(values: list[float], buckets: int = 10) -> list[tuple[str, int]]:
    if not values:
        return []
    lo, hi = min(values), max(values)
    if hi - lo < 1e-9:
        return [(f"{lo:.4f}", len(values))]
    width = (hi - lo) / buckets
    counts = [0] * buckets
    for v in values:
        counts[min(int((v - lo) / width), buckets - 1)] += 1
    return [(f"{lo + i * width:.3f}~{lo + (i + 1) * width:.3f}", c) for i, c in enumerate(counts)]


def show(title: str, values: list[float]) -> None:
    print(f"\n--- {title} ---")
    for band, c in histogram(values):
        print(f"  {band}  {'#' * c} {c}")
    if values:
        print(
            f"  n={len(values)} min={min(values):.4f} "
            f"中位={statistics.median(values):.4f} max={max(values):.4f}",
        )


async def main() -> None:
    cases = load_cases()
    queries = [f"{c['ticket']['title']} {c['ticket']['description']}" for c in cases]

    t0 = time.perf_counter()
    async with await open_index() as index:
        built = await index.build(recreate=True)
        t_index = time.perf_counter() - t0
        n_docs = sum(built.values())
        print(f"[索引] {built} → {n_docs} 次 embedding 调用，耗时 {t_index:.1f}s"
              f"（{t_index / n_docs:.2f}s/条）")

        rows = []
        t1 = time.perf_counter()
        for case, q in zip(cases, queries):
            hits = await index.search_services(q, top_k=3)
            got = int(hits[0].chunk.metadata["service_id"]) if hits else 0
            rows.append(
                {
                    "id": case["id"],
                    "gold": case["expected"]["serviceTop1"],
                    "got": got,
                    "top1_score": hits[0].score if hits else None,
                    "runner_up": [
                        (int(h.chunk.metadata["service_id"]), h.score) for h in hits[1:]
                    ],
                },
            )
        t_cold = time.perf_counter() - t1

        hits_right = [r for r in rows if r["gold"] == r["got"]]
        hits_wrong = [r for r in rows if r["gold"] != r["got"]]
        correct_scores = [r["top1_score"] for r in hits_right]
        wrong_scores = [r["top1_score"] for r in hits_wrong]

        print(
            f"\n[服务抽取 top-1] {len(hits_right)}/{len(rows)} = "
            f"{len(hits_right) / len(rows):.1%}   (n={len(rows)}，每条 = "
            f"{100 / len(rows):.1f}pt —— 低于本仓库自订『金标 <100 条不当结论』门槛)",
        )
        cache_on = cache_enabled()
        cache_note = (
            "  ← 暖缓存低估 embedding 延迟，冷数见 HELPDESK_EMBED_CACHE=off 那一跑"
            if cache_on
            else ""
        )
        print(
            f"[墙钟] 45 条查询 {t_cold:.1f}s（{t_cold / len(rows) * 1000:.0f}ms/条）"
            f"  embedding 缓存={'on' if cache_on else 'off'}{cache_note}",
        )

        show("命中正确时的 top-1 分数分布（阈值下沿的证据）", correct_scores)
        show("命中错误时的 top-1 分数分布（想靠阈值挡住就要高于这批 max）", wrong_scores)
        if wrong_scores and correct_scores:
            print(
                f"\n[可分性] 错误 max={max(wrong_scores):.4f} vs 正确 min={min(correct_scores):.4f} "
                f"→ {'存在可分间隙' if max(wrong_scores) < min(correct_scores) else '分数区间重叠，阈值切不开，只能靠别的信息'}",
            )

        print("\n--- 误判明细（含次优候选，看是不是'第二个服务也说得通'）---")
        for r in hits_wrong:
            runner = " ".join(f"{sid}:{s:.3f}" for sid, s in r["runner_up"])
            print(f"  {r['id']}: gold={r['gold']} got={r['got']}({r['top1_score']:.3f}) | {runner}")

        # 阈值扫描：3 条金标是"什么服务都不是"(serviceTop1=0)，严格 top-1 相等永远拿不到，
        # 所以用 Go 自己的判分规则（gold=0 当 top1 < t 视为命中）扫一遍。
        def sweep(t: float) -> int:
            return sum(
                (r["top1_score"] is not None)
                and (
                    (r["gold"] == 0 and r["top1_score"] < t)
                    or (r["gold"] != 0 and t <= r["top1_score"] and r["got"] == r["gold"])
                )
                for r in rows
            )

        grid = [round(0.40 + i * 0.01, 2) for i in range(31)]
        curve = [(sweep(t), t) for t in grid]
        best = max(curve)
        print("\n--- 单一全局阈值扫描（能否替代被删掉的置信门）---")
        for score, t in curve:
            if score >= best[0] or abs(t - 0.50) < 1e-9 or abs(t - 0.60) < 1e-9:
                print(f"  t={t:.2f} → {score}/{len(rows)} = {score / len(rows):.1%}")
        print(
            f"  最优 t={best[1]:.2f} → {best[0]}/{len(rows)} = {best[0] / len(rows):.1%}"
            f"，无阈值基线 {len(hits_right)}/{len(rows)} = {len(hits_right) / len(rows):.1%}",
        )
        null_gold = [r for r in rows if r["gold"] == 0]
        print(
            f"  3 条『无服务』金标的 top-1 分数 = "
            f"{[round(r['top1_score'], 3) for r in null_gold]}；"
            f"要全挡住需 t > {max(r['top1_score'] for r in null_gold):.3f}，"
            f"而这会同时误伤 "
            f"{sum(1 for r in hits_right if r['top1_score'] <= max(x['top1_score'] for x in null_gold))} "
            f"条正确命中 → 阈值买不回被删的门",
        )

        k_scores = []
        for q in queries[:10]:
            k_scores.extend(h.score for h in await index.search_knowledge(q, top_k=3))
        show("知识库检索分数分布（另一条轴，尺度不同不能共用阈值）", k_scores)

        # 删集合后旧句柄会怎样？实测：KnowledgeBase 把 ensure_collection 记忆化了，
        # 句柄认为集合已就绪，于是 search 直接打到不存在的集合上抛 pymilvus 异常。
        await index.store.delete_collection("services")
        try:
            await index.services.search(["下单接口 500"], top_k=3)
            print("\n[空集合] 旧句柄 search 静默返回 → 重建路径安全")
        except Exception as exc:  # noqa: BLE001 — 探针就是要看它怎么坏
            print(f"\n[空集合] 旧句柄 search 抛 {type(exc).__name__}: "
                  f"{str(exc).splitlines()[-1][:80]}")
            print("  → 评测里每次重建集合必须换新 KnowledgeBase 句柄（ensure_collection 有记忆）")

    # 进程内重开：验证 .db 持久（多轮评测不必重新索引）
    t2 = time.perf_counter()
    async with await open_index() as again:
        reopen = time.perf_counter() - t2
        hits = await again.search_services("下单接口频繁 500，客户端拿不到订单号", top_k=1)
        # 删过集合 → 新句柄的 ensure_collection 会懒建一个空集合，所以这里注定无命中。
        # 要验证 ".db 文件跨进程复用" 得在未删集合的情况下重开，见 p0 之后的 index 命令。
        if hits:
            print(
                f"[重开] 新句柄复用 {Path(DB_PATH).name}（{reopen:.2f}s）→ top-1 "
                f"{hits[0].chunk.metadata['service_id']} score={hits[0].score:.4f}",
            )
        else:
            print(f"[重开] 新句柄 {reopen:.2f}s 建好、懒建空集合后无命中（集合在本轮被删过）")


if __name__ == "__main__":
    asyncio.run(main())
