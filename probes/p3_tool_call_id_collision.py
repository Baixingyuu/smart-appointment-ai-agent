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

2026-09-27 换了载体：`assign_ticket` 已从模型工具面白名单摘掉且 `permission=DENY`
（见 `docs/P3_AGENT.md` 顶部那条口径裁决），模型发它也不会执行，这条 id 撞车形状就复现不出来了。
现在用 `propose_appointments`（ALLOW 的只读件，`need` 有 min_length=1）复现同样的
"第一次不合规 → 改正后重调"。

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
from helpdesk.runtime.agent_factory import make_agent  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402

TOOL = "propose_appointments"
GOOD = {"need": "现场换令牌并验证下单接口", "earliest": "2026-09-28 09:00", "latest": "2026-09-28 18:00"}
# 空 need 撞 min_length=1：对应真机上那次漏填 required 字段
MISSING_FIELD = dict(GOOD, need="")


def _first_text(block: object) -> str:
    output = getattr(block, "output", None) or ""
    first = output[0] if isinstance(output, list) and output else output
    if isinstance(first, dict):
        first = first.get("text", "")
    elif not isinstance(first, str):
        first = getattr(first, "text", first)
    return str(first)[:60]


async def main() -> int:
    turns = [
        Turn(tool_calls=[(TOOL, MISSING_FIELD)], ollama_ids=True),
        Turn(tool_calls=[(TOOL, GOOD)], ollama_ids=True),
        Turn(text="已处理。"),
    ]
    agent, ctx = await make_agent(FakeIndex(), TicketStore(), model=ScriptedChatModel(turns))
    await agent.reply(UserMsg(name="user", content="帮我约个人上门"))

    calls = [b for b in agent.state.context[-1].get_content_blocks("tool_call")]
    results = [b for b in agent.state.context[-1].get_content_blocks("tool_result")]
    retries = [b for b in results if b.name == TOOL]

    print(f"{TOOL} 调用 {len(calls)} 次，tool_result {len(retries)} 条")
    for b in calls:
        print(f"  call id={b.id!r} state={b.state}")
    for b in retries:
        print(f"  result id={b.id!r} state={b.state} out={_first_text(b)!r}")

    ok = [b.state for b in retries] == ["error", "success"]
    print(
        "\n结论: 重试"
        + ("各自留下 tool_result，第二次真的执行了。" if ok else "被静默丢弃 —— 第二次调用没有 tool_result。")
    )
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
