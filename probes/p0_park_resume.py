"""P0 探针③：park 唤醒契约 —— 待确认期间来的那句话到底怎么进去。

这条直接决定"确认语义桥"的形状，也决定 Go 侧 D2（待确认期间的消息被静默丢掉）
和 D3（子串「确认」被误判成同意）这两个复现过的缺陷在新架构里会不会回来。

跑法：.venv/bin/python probes/p0_park_resume.py
"""
from __future__ import annotations

import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from agentscope.agent import Agent, ReActConfig  # noqa: E402
from agentscope.event import (  # noqa: E402
    ConfirmResult,
    ReplyEndEvent,
    ReplyStartEvent,
    RequireUserConfirmEvent,
    ToolResultEndEvent,
    UserConfirmResultEvent,
    UserInterruptEvent,
)
from agentscope.message import TextBlock, ToolResultState, UserMsg  # noqa: E402
from agentscope.permission import PermissionBehavior, PermissionDecision  # noqa: E402
from agentscope.tool import FunctionTool, Toolkit, ToolResponse  # noqa: E402

from helpdesk.eval.scripted_model import ScriptedChatModel, Turn  # noqa: E402

RAN: list[str] = []

_ASK = PermissionDecision(behavior=PermissionBehavior.ASK, message="建单前要人确认")


def create_ticket(title: str) -> ToolResponse:
    """Create a ticket.

    Args:
        title (str): ticket title
    """
    RAN.append(f"create:{title}")
    return ToolResponse(content=[TextBlock(type="text", text=f"created {title}")])


def make_agent(turns: list[Turn]) -> tuple[Agent, ScriptedChatModel]:
    model = ScriptedChatModel(turns)
    agent = Agent(
        name="helpdesk",
        system_prompt="你是帮助台助手。",
        model=model,
        toolkit=Toolkit(tools=[FunctionTool(create_ticket, permission=_ASK)]),
        react_config=ReActConfig(max_iters=10),
    )
    return agent, model


async def run(agent: Agent, inputs: object) -> list:
    events = []
    async for evt in agent.reply_stream(inputs):  # type: ignore[arg-type]
        events.append(evt)
    return events


def summarize(tag: str, events: list) -> None:
    print(f"\n=== {tag} ===")
    for e in events:
        name = type(e).__name__
        if name == "ReplyStartEvent":
            print(f"  ReplyStart reply_id={e.reply_id[:8]}")
        elif name == "ReplyEndEvent":
            print(f"  ReplyEnd finished_reason={e.finished_reason}")
        elif name == "RequireUserConfirmEvent":
            print(f"  RequireUserConfirm reply_id={e.reply_id[:8]}")
            for tc in e.tool_calls:
                print(f"    待确认: {tc.name} id={tc.id} input={tc.input}")
        elif name == "ToolResultEndEvent":
            print(f"  ToolResultEnd state={e.state}")
        elif name == "AssistantMsg" or getattr(e, "role", None) == "assistant":
            text = e.get_text_content() if hasattr(e, "get_text_content") else str(e.content)
            print(f"  AssistantMsg({e.finished_reason if hasattr(e, 'finished_reason') else '-'}): "
                  f"{str(text)[:70]}")
        else:
            print(f"  {name}")


def asking_calls(agent: Agent) -> list:
    return [
        t
        for t in agent.state.get_awaiting_tool_calls(agent.name)
        if t.state == "asking"
    ]


async def main() -> None:
    PARK = [
        Turn(tool_calls=[("create_ticket", {"title": "下单 500"})], id_prefix="P"),
        Turn(text="已按您的确认建单"),
        Turn(text="那就不建了"),
    ]

    # 1) park：写工具被 ASK 拦下
    agent, model = make_agent(PARK)
    ev = await run(agent, UserMsg(name="u", content="帮我建单：下单 500"))
    summarize("1) 待确认：工具被 ASK 拦住", ev)
    parked = [e for e in ev if isinstance(e, RequireUserConfirmEvent)]
    ends = [e for e in ev if isinstance(e, ReplyEndEvent)]
    print(f"  RequireUserConfirm={len(parked)} ReplyEnd={len(ends)} "
          f"→ park 时**不发** ReplyEndEvent，SSE/前端不能靠它判断结束")
    print(f"  cur_iter={agent.state.cur_iter} → park 的那一轮不计入轮次预算")
    assert ends == [] and len(parked) == 1
    asking = asking_calls(agent)
    print(f"  state 里 ASKING 的调用: {[t.id for t in asking]}")

    # 2) park 期间直接塞一句普通用户消息
    try:
        await run(agent, UserMsg(name="u", content="我还想提个别的问题"))
        print("\n=== 2) park 期间发普通消息 ===\n  没报错？与源码结论不符，需重查")
    except ValueError as exc:
        print("\n=== 2) park 期间发普通消息 ===")
        print(f"  ValueError: {str(exc)[:110]}")
        print("  → 框架拒绝裸消息。Go 的 D2「消息被静默丢掉」在这里变成显式异常，"
              "但代价是必须有人把这句话翻译成事件 —— 桥跑不掉")

    # 3) 批准：用 state 里的权威 tool_call 构造事件
    agent, model = make_agent(PARK)
    await run(agent, UserMsg(name="u", content="帮我建单：下单 500"))
    tc = asking_calls(agent)[0]
    reply_id = agent.state.reply_id
    RAN.clear()
    ev = await run(
        agent,
        UserConfirmResultEvent(
            reply_id=reply_id,
            confirm_results=[ConfirmResult(confirmed=True, tool_call=tc)],
        ),
    )
    summarize("3) 批准（confirmed=True）", ev)
    print(f"  工具真的执行了吗: {RAN}")
    assert RAN == ["create:下单 500"]

    # 4) 拒绝：confirmed=False → 一条 denied 的工具结果，然后模型继续想
    agent, model = make_agent(PARK)
    await run(agent, UserMsg(name="u", content="帮我建单：下单 500"))
    tc = asking_calls(agent)[0]
    RAN.clear()
    ev = await run(
        agent,
        UserConfirmResultEvent(
            reply_id=agent.state.reply_id,
            confirm_results=[ConfirmResult(confirmed=False, tool_call=tc)],
        ),
    )
    summarize("4) 拒绝（confirmed=False）", ev)
    denied = [e for e in ev if isinstance(e, ToolResultEndEvent) and e.state == ToolResultState.DENIED]
    print(f"  denied 结果 {len(denied)} 条，工具未执行={RAN == []}，"
          f"模型还在同一 reply 里继续推理（模型调用 {model.n_calls} 次）")

    # 5) 重复/过期决策
    try:
        await run(
            agent,
            UserConfirmResultEvent(
                reply_id=agent.state.reply_id,
                confirm_results=[ConfirmResult(confirmed=True, tool_call=tc)],
            ),
        )
        print("\n=== 5) 重复点击已解决的确认 ===\n  没报错 → 幂等由框架保证？需重查")
    except ValueError as exc:
        print("\n=== 5) 重复点击已解决的确认 ===")
        print(f"  ValueError: {str(exc)[:110]}")
        print("  → agent 层**不**幂等（app 层的 resume_after_decision 才返回 False）。"
              "桥自己得先读 ASKING 再决定发不发")

    # 6) 语义不明/新诉求：用 UserInterruptEvent 拆掉 park，再开新一轮
    agent, model = make_agent(
        [
            Turn(tool_calls=[("create_ticket", {"title": "旧单"})], id_prefix="I"),
            Turn(text="好的，那先说您新提的问题"),
        ],
    )
    await run(agent, UserMsg(name="u", content="帮我建单"))
    RAN.clear()
    ev = await run(agent, UserInterruptEvent(reply_id=agent.state.reply_id))
    summarize("6a) UserInterruptEvent 拆掉 park", ev)
    print(f"  被中断时工具执行了吗: {RAN}（应为空）")
    assert RAN == []
    ev2 = await run(agent, UserMsg(name="u", content="我还想提个别的问题：打印机卡纸"))
    summarize("6b) 拆完之后普通消息可以进来", ev2)
    print(f"  reply_id 换了吗: 新一轮 reply_id={ev2[0].reply_id[:8] if isinstance(ev2[0], ReplyStartEvent) else '?'}")
    print("  → 「新诉求」= 先 interrupt 再重开一轮；「批准/拒绝」= 直接给 confirm 事件")


if __name__ == "__main__":
    asyncio.run(main())
