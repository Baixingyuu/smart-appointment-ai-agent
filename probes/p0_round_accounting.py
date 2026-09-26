"""P0 探针②：轮次记账 —— 一轮发多个 tool_call 时框架怎么数。

这条决定 max_iters 的语义，也决定 Go 侧「一次回复最多 N 次工具调用」那道闸
能不能直接写成框架配置。

跑法：.venv/bin/python probes/p0_round_accounting.py
"""
from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from agentscope.agent import Agent, ReActConfig  # noqa: E402
from agentscope.event import (  # noqa: E402
    ExceedMaxItersEvent,
    ReplyEndEvent,
    ToolResultEndEvent,
)
from agentscope.message import TextBlock, ToolResultState, UserMsg  # noqa: E402
from agentscope.permission import PermissionBehavior, PermissionDecision  # noqa: E402
from agentscope.tool import FunctionTool, Toolkit, ToolResponse  # noqa: E402

from helpdesk.eval.scripted_model import ScriptExhausted, ScriptedChatModel, Turn  # noqa: E402

EXECUTED: list[str] = []

_ALLOW = PermissionDecision(behavior=PermissionBehavior.ALLOW, message="allowed by probe")


def read_tool(topic: str) -> ToolResponse:
    """Read something.

    Args:
        topic (str): the topic to read
    """
    EXECUTED.append(f"read:{topic}")
    return ToolResponse(content=[TextBlock(type="text", text=f"ok {topic}")])


def write_tool(topic: str) -> ToolResponse:
    """Write something.

    Args:
        topic (str): the topic to write
    """
    EXECUTED.append(f"write:{topic}")
    return ToolResponse(content=[TextBlock(type="text", text=f"done {topic}")])


def make_toolkit() -> Toolkit:
    return Toolkit(
        tools=[
            FunctionTool(read_tool, is_read_only=True, permission=_ALLOW),
            FunctionTool(write_tool, is_concurrency_safe=False, permission=_ALLOW),
        ],
    )


async def collect(turns: list[Turn], max_iters: int = 50) -> dict:
    global EXECUTED
    EXECUTED = []
    model = ScriptedChatModel(turns)
    agent = Agent(
        name="helpdesk",
        system_prompt="你是帮助台助手。",
        model=model,
        toolkit=make_toolkit(),
        react_config=ReActConfig(max_iters=max_iters),
    )
    events = []
    async for evt in agent.reply_stream(UserMsg(name="u", content="开始")):
        events.append(evt)
    results = [e for e in events if isinstance(e, ToolResultEndEvent)]
    return {
        "model_calls": model.n_calls,
        "cur_iter": agent.state.cur_iter,
        "executed": list(EXECUTED),
        "n_results": len(results),
        "error_results": sum(1 for e in results if e.state == ToolResultState.ERROR),
        "exceed_max_iters": sum(1 for e in events if isinstance(e, ExceedMaxItersEvent)),
        "reply_end": sum(1 for e in events if isinstance(e, ReplyEndEvent)),
        "tail": [type(e).__name__ for e in events][-5:],
        "requests": model.requests,
    }


def show(title: str, r: dict) -> None:
    print(f"\n=== {title} ===")
    print(
        f"  模型被叫 {r['model_calls']} 次 | cur_iter={r['cur_iter']} | "
        f"工具结果 {r['n_results']} 条（error {r['error_results']}） | "
        f"ExceedMaxIters={r['exceed_max_iters']} | ReplyEnd={r['reply_end']}",
    )
    print(f"  实际执行: {r['executed']}")
    for i, req in enumerate(r["requests"], 1):
        dropped = req["dropped_tool_calls"]
        print(
            f"    call#{i} tool_choice={req['tool_choice']} "
            f"msgs={req['n_messages']}"
            + (f" 脚本被吞掉 {dropped} 个 tool_call" if dropped else ""),
        )


async def main() -> None:
    # A：一轮 3 个并发安全的读工具
    r = await collect(
        [
            Turn(tool_calls=[("read_tool", {"topic": t}) for t in "abc"], id_prefix="A"),
            Turn(text="收尾"),
        ],
    )
    show("A: 一轮 3 个 tool_call", r)
    assert r["model_calls"] == 2 and r["cur_iter"] == 2 and r["n_results"] == 3
    print("  → cur_iter 按模型轮次加、不按 tool_call 加：max_iters 控不住工具调用次数")

    # B：一轮 3 个非并发安全的写工具 → 串行保序
    r = await collect(
        [
            Turn(
                tool_calls=[("write_tool", {"topic": str(i)}) for i in range(3)],
                id_prefix="B",
            ),
            Turn(text="收尾"),
        ],
    )
    show("B: 一轮 3 个写工具（is_concurrency_safe=False）", r)
    assert r["cur_iter"] == 2
    assert r["executed"] == ["write:0", "write:1", "write:2"], "非并发安全 → 串行且保序"
    print("  → 串行批次保持模型给出的顺序，不需要自研排序")

    # C：一轮里既给文本又给 tool_call
    r = await collect(
        [
            Turn(text="我先说一句", tool_calls=[("read_tool", {"topic": "x"})], id_prefix="C"),
            Turn(text="收尾"),
        ],
    )
    show("C: 一轮同时给文本和 tool_call", r)
    assert r["executed"] == ["read:x"], "有可执行 tool_call 时文本不终止回合"
    print("  → Acting 优先于 Exit，同轮的文本不会提前收尾")

    # D：max_iters 掐在中间
    r = await collect(
        [
            Turn(tool_calls=[("read_tool", {"topic": "1"})], id_prefix="D0"),
            Turn(tool_calls=[("read_tool", {"topic": "2"})], id_prefix="D1"),
            Turn(tool_calls=[("read_tool", {"topic": "3"})], id_prefix="D2"),
            Turn(text="被 max_iters 逼出来的收尾"),
        ],
        max_iters=2,
    )
    show("D: max_iters=2，脚本还想跑 3 轮工具", r)
    print(f"  尾部事件: {r['tail']}")
    print(
        f"  cur_iter={r['cur_iter']} vs max_iters=2 → 超限后框架额外叫了 "
        f"{r['model_calls'] - 2} 次模型逼收尾，并发了 {r['exceed_max_iters']} 条 "
        f"ExceedMaxItersEvent（收尾话术可在 on_reply 里换掉）",
    )

    # E：模型叫了一个不存在的工具
    r = await collect(
        [
            Turn(tool_calls=[("no_such_tool", {"topic": "1"})], id_prefix="E0"),
            Turn(text="收尾"),
        ],
    )
    show("E: 调用未注册的工具名", r)
    print(
        f"  不抛异常，改成 {r['error_results']} 条 error 状态的工具结果回喂模型 "
        f"→ 评测归因必须读 ToolResultEndEvent.state，不能只数事件条数",
    )

    # F：脚本耗尽 = 框架多跑了一轮，这本身就是断言
    try:
        await collect([Turn(tool_calls=[("read_tool", {"topic": "1"})], id_prefix="F0")])
    except ScriptExhausted as exc:
        print(f"\n=== F: 只给 1 轮脚本但模型还要继续 ===\n  {exc}")
        print("  → 脚本长度就是轮次上界，多一轮立刻炸，评测不会静默放行")


if __name__ == "__main__":
    asyncio.run(main())
