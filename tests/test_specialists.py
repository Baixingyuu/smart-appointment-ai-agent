"""专业子 Agent 封装成工具（P6-a③）的接缝：只读、嵌套成本可见、业务事实只有一份。

这里不调真模型：`ScriptedChatModel` 精确控制专线内部的输出，为的正是要看清
框架在给定输出下怎么做（嵌套轮次算在谁头上、确认请求从子流里冒出来会怎样）。
带不带专线在同一句话上的成本差是 P6 探针要量的，不在这里下结论。
"""
from __future__ import annotations

import sys
from datetime import datetime
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from agentscope.message import UserMsg  # noqa: E402
from agentscope.permission import PermissionBehavior  # noqa: E402

from helpdesk.appointment import AppointmentStore  # noqa: E402
from helpdesk.catalog import employees  # noqa: E402
from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.eval.scripted_model import ScriptedChatModel, Turn  # noqa: E402
from helpdesk.runtime.agent_factory import make_agent  # noqa: E402
from helpdesk.runtime.specialists import (  # noqa: E402
    ADVISE_DISPATCH_TOOL,
    CONSULT_TOOL,
    PLAN_VISIT_TOOL,
    SPECIALIST_SPECS,
    SPECIALIST_TOOLS,
    SpecialistSpec,
    make_specialist_tools,
    specialist_pool,
)
from helpdesk.runtime.tool_surface import MODEL_INVISIBLE_TOOLS, ToolSurfaceMiddleware  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402

VISIT_TASK = "给下单接口 401 约一个上门时间"


def _ctx() -> HelpdeskContext:
    return HelpdeskContext(index=FakeIndex(), store=TicketStore(), appointments=AppointmentStore())


async def _tools(ctx: HelpdeskContext, turns: list[Turn], **kw):
    model = ScriptedChatModel(turns)
    return await make_specialist_tools(ctx, model=model, **kw), model


def _by_name(tools) -> dict:
    return {t.name: t for t in tools}


class _State:
    """主管注入给专线的 AgentState 替身：只读 context 与 session_id。"""

    def __init__(self, *texts: str) -> None:
        self.context = [UserMsg(name="user", content=t) for t in texts]
        self.session_id = "sess-1"


_PROPOSE = {
    "need": "现场排查下单接口 401",
    "earliest": "2026-10-12 09:00",
    "latest": "2026-10-12 18:00",
    "duration_minutes": 60,
}


# --- 1. 挂载与工具面 -------------------------------------------------------


async def test_专线默认不挂载开关打开才多出三个工具() -> None:
    """开关是 `HELPDESK_SPECIALISTS`：没量过收益之前不改线上行为。"""
    off, _ = await make_agent(FakeIndex(), model=ScriptedChatModel([Turn(text="收尾")]))
    on, _ = await make_agent(
        FakeIndex(), model=ScriptedChatModel([Turn(text="收尾")]), specialists=True
    )
    schemas_off = await off.toolkit.get_tool_schemas()
    schemas_on = await on.toolkit.get_tool_schemas()
    n_off = {s["function"]["name"] for s in schemas_off}
    n_on = {s["function"]["name"] for s in schemas_on}
    assert n_on - n_off == set(SPECIALIST_TOOLS)
    assert not (n_off & set(SPECIALIST_TOOLS))
    # 主管看见的专线只有一个 `task` 参数：内部怎么查、查几步都不漏给上层的 schema
    for schema in schemas_on:
        if schema["function"]["name"] in SPECIALIST_TOOLS:
            assert sorted(schema["function"]["parameters"]["properties"]) == ["task"]


async def test_专线过工具面闸_三个名字都在白名单里() -> None:
    """托管路径附送的框架工具照样被摘，专线不能被误伤。"""
    agent, _ = await make_agent(
        FakeIndex(), model=ScriptedChatModel([Turn(text="收尾")]), specialists=True
    )
    schemas = await agent.toolkit.get_tool_schemas()
    hostiles = [{"type": "function", "function": {"name": n, "parameters": {}}} for n in ("Bash", "Write")]
    seen: dict = {}

    async def next_handler(**kwargs):
        seen.update(kwargs)
        return "ok"

    mw = ToolSurfaceMiddleware()
    await mw.on_model_call(agent, {"tools": schemas + hostiles}, next_handler)
    kept = {t["function"]["name"] for t in seen["tools"]}
    assert set(SPECIALIST_TOOLS) <= kept
    # 差集里除了托管附送的框架件，还有注册着但刻意不递交的业务件（`MODEL_INVISIBLE_TOOLS`）
    assert mw.dropped == {"Bash", "Write", *MODEL_INVISIBLE_TOOLS}


async def test_专线只拿只读件_没有需要确认的写工具() -> None:
    """子流里的确认请求没人能回答，所以专线的工具面必须物理上不含 ASK 件。"""
    ctx = _ctx()
    pool = await specialist_pool(ctx)

    async def behavior(name: str) -> PermissionBehavior:
        # 闸问的是"这一次调用要不要问用户"：search_knowledge 是框架的 ToolBase，
        # 要收 (tool_input, context) 两个位置参数；FunctionTool 收任意参数。
        return (await pool[name].check_permissions({}, None)).behavior

    # 判据本身要先成立：写操作确实是 ASK，否则下面那一圈 ALLOW 断言是空的
    for write in ("create_ticket", "book_appointment", "reschedule_appointment"):
        assert await behavior(write) is PermissionBehavior.ASK
    for spec in SPECIALIST_SPECS:
        # 挑漏名字不会报错，只会让这条专线每次调用都短路，所以在这里钉住
        assert set(spec.tool_names) <= set(pool), spec.name
        for name in spec.tool_names:
            assert await behavior(name) is PermissionBehavior.ALLOW


# --- 2. 嵌套成本：外层看不见，所以必须自己算 -------------------------------


async def test_嵌套轮次与token如实记进metadata() -> None:
    ctx = _ctx()
    tools, model = await _tools(
        ctx,
        [
            Turn(tool_calls=[("propose_appointments", _PROPOSE)], input_tokens=300, output_tokens=40),
            Turn(text="建议 10-12 09:00 张伟。", input_tokens=500, output_tokens=30),
        ],
    )
    out = await _by_name(tools)[PLAN_VISIT_TOOL].call(task=VISIT_TASK, _agent_state=_State())
    assert out.metadata["model_calls"] == 2 == model.n_calls
    assert out.metadata["input_tokens"] == 800
    assert out.metadata["output_tokens"] == 70
    assert out.metadata["total_tokens"] == 870
    assert out.metadata["tool_names"] == ["propose_appointments"]
    assert out.metadata["parked"] is None
    assert out.metadata["empty"] is False
    assert out.content[0].text.startswith(f"【{PLAN_VISIT_TOOL}】")


async def test_专线交出空结论时如实说没结论并叫上级自己来() -> None:
    ctx = _ctx()
    tools, _ = await _tools(ctx, [Turn(text="   ", input_tokens=10, output_tokens=2)])
    out = await _by_name(tools)[PLAN_VISIT_TOOL].call(task=VISIT_TASK, _agent_state=_State())
    assert out.metadata["empty"] is True
    assert "没交出结论" in out.content[0].text
    assert "不要把同一个任务再丢给它一次" in out.content[0].text


async def test_装配缺工具时不硬跑() -> None:
    """专线的工具面是挑出来的，挑漏了要立刻看得见，而不是让模型空转三轮。"""
    ctx = _ctx()
    bogus = SpecialistSpec(
        name="bogus", description="d", instruction="i", tool_names=("no_such_tool",)
    )
    tools, model = await _tools(ctx, [Turn(text="不该被叫到")], specs=(bogus,))
    out = await tools[0].call(task="随便查点什么", _agent_state=_State())
    assert out.metadata["misconfigured"] == ["no_such_tool"]
    assert "装配缺工具" in out.content[0].text
    assert model.n_calls == 0


# --- 3. 共享的是业务事实与原话，不是同一个会话状态 --------------------------


async def test_专线里的工具读写同一个ctx() -> None:
    """预约已经占了一格 → 派单专线里 match_service 看到的候选人负载必须已经带上它。
    这只有在线工具与主管共用一份 HelpdeskContext 时才成立。"""
    ctx = _ctx()
    baseline = next(e for e in employees() if e.id == 101).current_load
    ctx.appointments.book(
        engineer_id=101,
        start=datetime(2026, 10, 12, 10),
        duration_minutes=60,
        need="现场排查",
    )
    tools, _ = await _tools(
        ctx,
        [
            Turn(tool_calls=[("match_service", {"query": "下单接口 401"})], input_tokens=20, output_tokens=5),
            Turn(text="建议 101。", input_tokens=20, output_tokens=5),
        ],
    )
    out = await _by_name(tools)[ADVISE_DISPATCH_TOOL].call(task="这单派给谁", _agent_state=_State())
    assert out.metadata["tool_names"] == ["match_service"]
    loads = {c.employee.id: c.employee.current_load for c in ctx.evidence.candidates}
    assert loads[101] == baseline + 1


async def test_用户原话与任务一起进专线的输入() -> None:
    said = "下单接口一直返回 401，希望周一上午有人上门看看"
    ctx = _ctx()
    tools, model = await _tools(ctx, [Turn(text="知道了。")])
    await _by_name(tools)[PLAN_VISIT_TOOL].call(task=VISIT_TASK, _agent_state=_State(said))
    blob = str(model.requests)
    assert VISIT_TASK in blob
    assert said in blob  # 引文要能在用户原话里查到，专线得看到同一份原话
    assert "上级没给到用户原话" not in blob


async def test_没有用户原话时明说而不是假装看过() -> None:
    ctx = _ctx()
    tools, model = await _tools(ctx, [Turn(text="知道了。")])
    await _by_name(tools)[CONSULT_TOOL].call(task="这个报错一般怎么处理", _agent_state=_State())
    assert "上级没给到用户原话" in str(model.requests)


# --- 4. HITL 边界没有上移：子流里的确认请求会被掐断 ------------------------


async def test_专线里冒出的确认请求被中止且什么都没落库() -> None:
    """这条是反向保险：哪天有人把写工具挑进专线，必须当场报错，而不是挂在那等确认。"""
    ctx = _ctx()
    unsafe = SpecialistSpec(
        name="unsafe_writer",
        description="d",
        instruction="i",
        tool_names=("create_ticket",),
    )
    tools, _ = await _tools(
        ctx,
        [
            Turn(
                tool_calls=[
                    (
                        "create_ticket",
                        {
                            "title": "下单接口 401",
                            "description": "下单接口报 401",
                            "category": "incident",
                            "priority": "P1",
                            "slots": [],
                        },
                    )
                ]
            )
        ],
        specs=(unsafe,),
    )
    out = await tools[0].call(task="直接建单", _agent_state=_State("下单接口报 401"))
    assert out.metadata["parked"] == "create_ticket"
    assert "需要用户确认的写操作" in out.content[0].text
    assert ctx.store.tickets == {}  # 中止是真的中止，不是只说了句狠话


def test_专线的名字与业务工具不重名() -> None:
    """重名会让 Toolkit 静默覆盖（P3 的 tool_call id 撞车同一个教训的两面）。"""
    names = {t.name for t in build_tools(_ctx())}
    assert not (set(SPECIALIST_TOOLS) & names)
    assert len(set(SPECIALIST_TOOLS)) == len(SPECIALIST_SPECS) == 3


@pytest.mark.parametrize("name", SPECIALIST_TOOLS)
async def test_每条专线都只有一个task入参(name: str) -> None:
    ctx = _ctx()
    tools = _by_name(await make_specialist_tools(ctx, model=ScriptedChatModel([Turn(text="x")])))
    tool = tools[name]
    assert tool.input_schema is not None
    assert sorted(tool.input_schema["properties"]) == ["task"]
    assert (await tool.check_permissions()).behavior is PermissionBehavior.ALLOW
