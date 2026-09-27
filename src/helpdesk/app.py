"""进程入口：建索引 + 一条能听懂人话的会话。

渲染用框架自带的 `ConsoleRenderer`，但**不用** `launch_console`：它把待确认写成
y/n 提示（`console/_console.py:75`），正好绕开确认桥；桥要接的是聊天里的一句话。

会话持久化本轮不做：业务事实（TicketStore）还是进程内的，恢复 AgentState 会得到
"模型记得建过单、系统里没这张单"的错位。跨轮历史在同一进程内由 AgentState 保留，
评测的多轮用例就走这条路。`agentscope.app.create_app` 那条托管路径要 `agentscope[service]`
（apscheduler），产品形态是本地 console，不装。
"""
from __future__ import annotations

import argparse
import asyncio
import sys

from agentscope.console import ConsoleRenderer

from .knowledge import open_index
from .runtime.agent_factory import CHAT_MODEL, make_agent
from .runtime.confirm_bridge import deliver
from .ticket_store import TicketStore


def _tip(pending: object) -> str:
    if pending is None:
        return "您> "
    names = "、".join(t.name for t in pending.tool_calls)  # type: ignore[attr-defined]
    return f"（等您对「{names}」表个态）您> "


async def chat(agent: object, store: TicketStore, renderer: ConsoleRenderer) -> None:
    renderer.console.print(f"聊天已开始（模型 {CHAT_MODEL}）。输入 exit 结束。", style="dim")
    pending = None
    while True:
        try:
            line = (await asyncio.to_thread(input, _tip(pending))).strip()
        except (EOFError, KeyboardInterrupt):
            return
        if line in {"exit", "quit"}:
            return
        if not line:
            continue
        result = await deliver(agent, line, on_event=renderer.render)  # type: ignore[arg-type]
        pending = result.pending
        if pending is not None:
            renderer.console.print(
                "提示：说「确认」建单，说「不建」拒绝，说别的算新诉求（会先取消这次建单）。",
                style="dim",
            )


def _summary(store: TicketStore) -> str:
    if not store.tickets:
        return "本次会话没有落成工单。"
    lines = [f"工单 {len(store.tickets)} 张："]
    for t in store.tickets.values():
        who = "转人工待认领" if t.assignee_id is None else f"员工 {t.assignee_id}"
        missing = f"，缺 {'、'.join(t.missing_info)}" if t.missing_info else ""
        lines.append(f"  #{t.id} {t.title}｜{t.category.value}/{t.priority.value}｜{t.status.value}｜{who}{missing}")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="helpdesk", description="AgentScope 版帮助台 console")
    parser.add_argument("--keep-index", action="store_true", help="不重建向量索引（省一次全量 embedding）")
    parser.add_argument(
        "--full-tool-results",
        action="store_true",
        help="不截断工具结果（框架默认 20 行，会把 match_service 末尾的【已结历史】截到可见区之外）",
    )
    parser.add_argument("--verbosity", choices=("quiet", "default", "debug"), default="default")
    args = parser.parse_args(argv)

    async def run() -> int:
        async with await open_index() as index:
            if not args.keep_index:
                print("重建索引:", await index.build(recreate=True), flush=True)
            agent, ctx = await make_agent(index)
            renderer = ConsoleRenderer(
                verbosity=args.verbosity,
                max_tool_result_lines=None if args.full_tool_results else 20,
            )
            await chat(agent, ctx.store, renderer)
            print(_summary(ctx.store))
            return 0

    return asyncio.run(run())


if __name__ == "__main__":
    sys.exit(main())
