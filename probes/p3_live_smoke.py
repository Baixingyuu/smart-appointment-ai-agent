"""P3 实跑冒烟：把真人话术过完整条链路，打印框架到底做了什么。

和 tests/test_agent.py 的分工：那边用脚本模型固定每一轮输出，验的是**接线正确**；
这边用本地 qwen3:8b（temp=0），验的是**真模型会不会这么走**。
两者结论不能互相替代 —— 实跑里 qwen3 漏填 required 参数、把 change 类的槽位
塞进 incident 单、干脆跳过查重，都是脚本模型不会犯的。

五个场景各钉一条硬承诺：
  short    建单→定位服务→指派要在一次确认里跑完（默认）
  intake   模糊上报必须走 ask_user，且第二句补上来的信息要能落成单
  dedup    同一问题再报一次不许出现第二张单
  kb       明确要求查知识库时必须真的调 search_knowledge，且不建单（唯一带断言的场景）
  kb_vague 同一件事换个含糊说法 —— 对照组，实测模型既不检索也不建单，只反问一句

跑法：.venv/bin/python probes/p3_live_smoke.py [short|intake|dedup|kb|kb_vague]
"""
from __future__ import annotations

import asyncio
import logging
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
logging.getLogger("pymilvus").setLevel(logging.CRITICAL)

from agentscope.console import ConsoleRenderer  # noqa: E402
from agentscope.event import (  # noqa: E402
    ModelCallEndEvent,
    ToolCallStartEvent,
    ToolResultEndEvent,
)

from helpdesk.knowledge import open_index  # noqa: E402
from helpdesk.runtime.agent_factory import CHAT_MODEL, make_agent  # noqa: E402
from helpdesk.runtime.confirm_bridge import deliver  # noqa: E402

SCENARIOS: dict[str, list[str]] = {
    "short": [
        "下单接口一直在报 401，token 过期了，帮忙看下",
        "确认",
    ],
    "intake": [
        "系统好像有点问题，帮我看看",
        "报销系统，上传发票的时候报错，今天开始的",
        "确认",
    ],
    "dedup": [
        "下单接口一直在报 401，token 过期了，帮忙看下",
        "确认",
        "下单接口还在报 401，token 过期那个事没解决，再催一下",
    ],
    "kb": [
        "帮我查一下知识库：私有化部署的最低硬件配置是多少？查了再回答，不要建单",
    ],
    "kb_vague": [
        "我们想私有化部署，机器配置有什么要求？先不要建单",
    ],
}


def _result_texts(agent: object) -> list[tuple[str, str, str]]:
    """按出现顺序取出每条 tool_result 的话术：quiet 渲染器不打印工具输出，
    而 dedup 场景要看的恰恰是 find_open_ticket 到底答了什么。"""
    out: list[tuple[str, str, str]] = []
    for msg in agent.state.context:  # type: ignore[attr-defined]
        for block in msg.get_content_blocks("tool_result"):
            output = block.output
            if isinstance(output, str):
                text = output
            else:
                text = "".join(
                    b["text"] if isinstance(b, dict) else getattr(b, "text", "")
                    for b in output
                )
            out.append((block.name, str(block.state), text))
    return out


async def run(index: object, lines: list[str]) -> tuple[list[str], int]:
    renderer = ConsoleRenderer(verbosity="quiet")
    agent, ctx = await make_agent(index)  # type: ignore[arg-type]
    pending = None
    # ToolResultEndEvent 只带 tool_call_id，工具名得从 Start 事件映射；这个表必须
    # 跨轮活着 —— park 前发出的调用，它的 result 会在下一轮才到达。
    name_of: dict[str, str] = {}
    stats = {"calls": 0, "in": 0, "out": 0}
    shown = 0
    for line in lines:
        print(f"\n=== 用户：{line}")
        seen: list[str] = []
        before = stats["calls"]

        def watch(event: object) -> None:
            renderer.render(event)
            if isinstance(event, ToolCallStartEvent):
                name_of[event.tool_call_id] = event.tool_call_name
                seen.append(f"call {event.tool_call_name}")
            elif isinstance(event, ToolResultEndEvent):
                seen.append(f"result {name_of.get(event.tool_call_id, '?')}={event.state}")
            elif isinstance(event, ModelCallEndEvent):
                stats["calls"] += 1
                stats["in"] += event.input_tokens
                stats["out"] += event.output_tokens
                seen.append(
                    f"model in={event.input_tokens} out={event.output_tokens} "
                    f"reason={event.finished_reason}",
                )

        out = await deliver(agent, line, on_event=watch)  # type: ignore[arg-type]
        pending = out.pending
        print("  桥判定:", out.kind, f"| 本轮模型调用 {stats['calls'] - before} 次")
        for s in seen:
            print("   ", s)
        results = _result_texts(agent)
        for name, state, text in results[shown:]:
            print(f"    ↳ {name}({state}): {text[:70]}")
        shown = len(results)

    print("\n=== 落库")
    for t in ctx.store.tickets.values():  # type: ignore[attr-defined]
        print(
            f"  #{t.id} {t.title}｜{t.category.value}/{t.priority.value}｜"
            f"{t.status.value}｜assignee={t.assignee_id}｜missing={t.missing_info}",
        )
    for log in ctx.store.assignments:  # type: ignore[attr-defined]
        print(f"  指派 #{log.ticket_id} → {log.assignee_id or '转人工'}（{log.outcome}）conf={log.confidence}")
        print(f"    候选服务: {log.candidates}")
    print(
        f"  工单 {len(ctx.store.tickets)} 张｜模型 {stats['calls']} 次｜"  # type: ignore[attr-defined]
        f"input {stats['in']} token｜output {stats['out']} token｜"
        f"cur_iter={agent.state.cur_iter}"  # type: ignore[attr-defined]
        f"｜模型={CHAT_MODEL}",
    )
    if pending is not None:
        print("  仍挂着待确认:", [t.name for t in pending.tool_calls])
    return list(name_of.values()), len(ctx.store.tickets)  # type: ignore[attr-defined]


async def main(scenario: str) -> int:
    lines = SCENARIOS[scenario]
    print(f"场景 {scenario}：{len(lines)} 句")
    async with await open_index() as index:
        called, tickets = await run(index, lines)
    if scenario != "kb":
        return 0
    # 证据 ① 的实跑对位：Agent 不替我们收集 middleware 的工具，
    # rag.list_tools() 没并进 Toolkit 的话这条调用根本不会出现。
    ok = "search_knowledge" in called and tickets == 0
    print(
        f"\n断言 kb：调过 search_knowledge={'search_knowledge' in called}"
        f"｜没顺手建单={tickets == 0}｜调用序列={called}",
    )
    return 0 if ok else 1


if __name__ == "__main__":
    name = sys.argv[1] if len(sys.argv) > 1 else "short"
    if name not in SCENARIOS:
        raise SystemExit(f"未知场景 {name!r}，可选：{', '.join(SCENARIOS)}")
    raise SystemExit(asyncio.run(main(name)))
