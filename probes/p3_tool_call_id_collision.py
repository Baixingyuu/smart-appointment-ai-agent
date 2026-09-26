"""P3 探针：同名工具在同一 reply 里被调两次时，tool_call id 会不会撞车。

真机（qwen3:8b + OllamaChatModel）上复现过一次**静默丢写**：assign_ticket 漏填
required 的 confidence → 框架按 `message/_block.py:163` 的迁移把这次调用置
finished 并回了校验错误；模型下一轮改正再调一次，这一次没有任何 tool_result，
工单就停在未指派。

机制（在安装包源码里逐条核过）：
1. `model/_ollama/_model.py:324` 合成 id 用 `f"{idx}_{name}"` —— 只在**一次模型响应内**唯一；
2. `state/_state.py:310-316` 的 `append_context` 把同一 reply 的所有块塞进**同一条**
   assistant 消息（msg id == reply_id），于是两轮的同名调用共享 id；
3. `state/_state.py:398-403` 与 `agent/_agent.py:3499-3507` 都用
   「本条消息里已有的 tool_result id 集合」过滤待执行调用 —— 重试的 id 已经被第一次
   失败的调用占掉，于是被判成"已完成"，直接不执行。

脚本模型默认生成的 id 跨轮唯一（`t0-0`、`t1-0`），所以离线测试测不到这条；
`Turn(ollama_ids=True)` 把 id 规则换成 Ollama 的，才把 bug 关进 pytest。

跑法：.venv/bin/python probes/p3_tool_call_id_collision.py
"""
from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from agentscope.message import UserMsg  # noqa: E402

from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.eval.scripted_model import ScriptedChatModel, Turn  # noqa: E402
from helpdesk.domain import Category, Priority  # noqa: E402
from helpdesk.runtime.agent_factory import make_agent  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402

GOOD = {"ticket_id": 1, "assignee": "101", "rationale": "owner 且有余量", "confidence": 0.85}
# 模型真机上第一次就是这么发的：漏了 required 的 confidence
MISSING_FIELD = {k: v for k, v in GOOD.items() if k != "confidence"}


def _first_text(block: object) -> str:
    output = getattr(block, "output", None) or ""
    first = output[0] if isinstance(output, list) and output else output
    if isinstance(first, dict):
        first = first.get("text", "")
    elif not isinstance(first, str):
        first = getattr(first, "text", first)
    return str(first)[:60]


async def main() -> int:
    store = TicketStore()
    ticket = store.create(
        title="下单接口 401",
        description="下单接口报 401，token 过期",
        category=Category.INCIDENT,
        priority=Priority.P1,
    ).ticket
    turns = [
        Turn(tool_calls=[("assign_ticket", MISSING_FIELD)], ollama_ids=True),
        Turn(tool_calls=[("assign_ticket", GOOD)], ollama_ids=True),
        Turn(text="已处理。"),
    ]
    agent, ctx = await make_agent(FakeIndex(), store, model=ScriptedChatModel(turns))
    await agent.reply(UserMsg(name="user", content="派个单"))

    calls = [b for b in agent.state.context[-1].get_content_blocks("tool_call")]
    results = [b for b in agent.state.context[-1].get_content_blocks("tool_result")]
    assign_results = [b for b in results if b.name == "assign_ticket"]

    print(f"工单 #{ticket.id} 的 assignee = {ctx.store.get(ticket.id).assignee_id}")
    print(f"assign_ticket 调用 {len(calls)} 次，tool_result {len(assign_results)} 条")
    for b in calls:
        print(f"  call id={b.id!r} state={b.state}")
    for b in assign_results:
        print(f"  result id={b.id!r} state={b.state} out={_first_text(b)!r}")

    ok = len(assign_results) == 2 and ctx.store.get(ticket.id).assignee_id == 101
    print(
        "\n结论: 重试"
        + ("被正常执行，指派落地。" if ok else "被静默丢弃 —— 第二次调用没有任何 tool_result。")
    )
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
