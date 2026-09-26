"""P3 组装层：确认桥的词表判定 + 脚本模型驱动的端到端一屏。

只有 embedding + 向量库是假的（要起 ollama 才能跑真索引）；工具、权限闸、
RAGMiddleware、事件流、AgentState 全是真代码。
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from agentscope.message import UserMsg  # noqa: E402

from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.eval.scripted_model import ScriptedChatModel, Turn  # noqa: E402
from helpdesk.runtime import intake as intake_state  # noqa: E402
from helpdesk.runtime.agent_factory import make_agent  # noqa: E402
from helpdesk.runtime.confirm_bridge import (  # noqa: E402
    APPROVE,
    NEW_REQUEST,
    REJECT,
    classify,
    deliver,
)
from helpdesk.domain import Category, IntakeProgress, Priority  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402


async def scripted(turns: list[Turn]) -> tuple:
    model = ScriptedChatModel(turns)
    return await make_agent(FakeIndex(), model=model), model


COMPLAIN = "下单接口报 401，token 过期了，帮忙看下"

# --- 桥的词表判定（Go 的 D3 在这条上复现过） --------------------------------


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("确认", APPROVE),
        ("嗯嗢先这样吧", APPROVE),
        ("别问了直接建单", APPROVE),
        ("好的", APPROVE),
        ("ok", APPROVE),
        ("先不建，等我确认下再说", REJECT),
        ("不建", REJECT),
        ("取消吧", REJECT),
        ("我还想提个别的问题", NEW_REQUEST),
        ("这个大概要多久能修好", NEW_REQUEST),
        ("", NEW_REQUEST),
    ],
)
def test_classify(text: str, expected: str) -> None:
    assert classify(text) == expected


# --- 工具接线 --------------------------------------------------------------


async def test_toolkit_exposes_five_business_tools_plus_native_search() -> None:
    (agent, _), _ = await scripted([Turn(text="收尾")])
    schemas = await agent.toolkit.get_tool_schemas()
    names = {s["function"]["name"] for s in schemas}
    assert names == {
        "match_service",
        "find_open_ticket",
        "ask_user",
        "create_ticket",
        "assign_ticket",
        "search_knowledge",  # 框架白送的那个
    }


# --- 端到端：描述→检索→查重→待确认→确认→建单→派单 ------------------------


def _parked_turns() -> list[Turn]:
    return [
        Turn(tool_calls=[("search_knowledge", {"query": "下单接口 401 令牌过期"})], id_prefix="s"),
        Turn(tool_calls=[("find_open_ticket", {"text": COMPLAIN})], id_prefix="d"),
        Turn(tool_calls=[("create_ticket", {
            "title": "下单接口 401",
            "description": "下单接口报 401，token 过期",
            "category": "incident",
            "priority": "P1",
            "slots": [{"name": "affected_system", "quote": "下单接口报 401"}],
        })], id_prefix="c"),
        Turn(tool_calls=[("match_service", {"query": "下单接口 401 token 过期"})], id_prefix="m"),
        Turn(tool_calls=[("assign_ticket", {
            "ticket_id": 1,
            "assignee": "101",
            "rationale": "核心下单接口的 owner，且有余量",
            "confidence": 0.85,
        })], id_prefix="a"),
        Turn(text="工单 #1 已建好，派给张伟（接口组）。"),
    ]


async def test_happy_path_parks_then_confirms_then_dispatches() -> None:
    (agent, ctx), model = await scripted(_parked_turns())
    first = await deliver(agent, COMPLAIN)
    assert first.pending is not None
    assert [t.name for t in first.pending.tool_calls] == ["create_ticket"]
    assert ctx.store.tickets == {}  # park 期间没落库

    second = await deliver(agent, "嗯嗢先这样吧")
    assert second.pending is None
    ticket = ctx.store.get(1)
    assert ticket.status.value == "pending" and ticket.assignee_id == 101
    assert [p.kind.value for p in ctx.store.progress_of(1)] == ["created", "assigned"]
    log = ctx.store.assignments[0]
    assert log.candidates == ((2001, pytest.approx(0.61)),)
    # 一次确认覆盖整条链路：建单 + 派单都在这条 reply 里跑完
    assert model.n_calls == 6


async def test_reject_leaves_no_ticket() -> None:
    (agent, ctx), _ = await scripted(_parked_turns()[:3] + [Turn(text="那就不建了，您先确认下 token 还在不在。")])
    await deliver(agent, COMPLAIN)
    out = await deliver(agent, "先不建，等我确认下再说")
    assert out.kind == REJECT and out.pending is None
    assert ctx.store.tickets == {} and ctx.store.assignments == []


async def test_new_request_during_park_is_not_dropped() -> None:
    turns = _parked_turns()[:3] + [
        Turn(tool_calls=[("find_open_ticket", {"text": "打印机卡纸"})], id_prefix="n"),
        Turn(text="好的，先处理打印机这个问题。"),
    ]
    (agent, ctx), model = await scripted(turns)
    await deliver(agent, COMPLAIN)
    out = await deliver(agent, "我还想提个别的问题：打印机卡纸")
    assert out.kind == NEW_REQUEST
    assert ctx.store.tickets == {}, "被取消的那次建单不能留下半成品"
    assert model.n_calls == 5, "interrupt 不叫模型，重开一轮才叫"


# --- 追问进度与安全阀 ------------------------------------------------------


async def test_ask_valve_marks_progress_and_injects_it() -> None:
    turns = [
        Turn(tool_calls=[("ask_user", {"slots": ["affected_system"]})], id_prefix="q"),
        Turn(text="是哪个系统在报错？"),
    ]
    (agent, ctx), _ = await scripted(turns)
    before = await agent._get_system_prompt()  # noqa: SLF001
    assert "<intake-progress>" not in before

    out = await deliver(agent, "系统一直报错，帮我建个单")
    assert out.pending is None
    assert intake_state.load(agent.state).asked_slots == ["affected_system"]
    after = await agent._get_system_prompt()  # noqa: SLF001
    assert "<intake-progress>" in after and "1/2" in after


async def test_second_ask_on_same_slot_closes_the_valve() -> None:
    turns = [
        Turn(tool_calls=[("ask_user", {"slots": ["affected_system"]})], id_prefix="q1"),
        Turn(tool_calls=[("ask_user", {"slots": ["affected_system"]})], id_prefix="q2"),
        Turn(text="好的，那我先按现有信息建单。"),
    ]
    (agent, ctx), _ = await scripted(turns)
    await deliver(agent, "系统一直报错")
    progress = intake_state.load(agent.state)
    assert progress.ask_rounds == 1, "重复问同一个槽位不再烧额度"
    assert ctx.store.tickets == {}


async def test_question_is_spoken_before_any_ticket_is_created() -> None:
    """ask_user 之后那次模型调用被禁工具：问题必须先说出口，不能同轮建单。

    真机上 qwen3:8b 就是会 ask_user ×2 然后直接 create_ticket，用户没被问到任何
    东西，下一句话被桥判成 new_request、park 掉的建单被取消。
    """
    turns = [
        Turn(tool_calls=[("ask_user", {"slots": ["affected_system"]})], id_prefix="q"),
        Turn(
            text="是哪个系统在报错？",
            tool_calls=[("create_ticket", {
                "title": "系统报错", "description": "系统一直报错",
                "category": "incident", "priority": "P2", "slots": [],
            })],
            id_prefix="q",
        ),
    ]
    (agent, ctx), model = await scripted(turns)
    out = await deliver(agent, "系统一直报错，帮我建个单")
    assert model.requests[1]["tools"] == [], "追问后的那次调用不能带任何工具 schema"
    assert "mode='none'" in model.requests[1]["tool_choice"]
    assert model.requests[1]["dropped_tool_calls"] == 1
    assert out.pending is None and out.kind == "plain"
    assert ctx.store.tickets == {}, "问题还没问出口，这轮不该建单"
    assert agent.state.context[-1].get_text_content() == "是哪个系统在报错？"


# --- 引文可追溯 ------------------------------------------------------------


async def test_unverified_quote_is_recorded_as_missing() -> None:
    turns = [
        Turn(tool_calls=[("create_ticket", {
            "title": "下单接口 401",
            "description": "下单接口报 401",
            "category": "incident",
            "priority": "P1",
            # 用户从没说过"数据库集群"，这条引文查不到
            "slots": [{"name": "affected_system", "quote": "数据库集群主从延迟"}],
        })], id_prefix="u"),
        Turn(text="已建单。"),
    ]
    (agent, ctx), _ = await scripted(turns)
    out = await deliver(agent, "下单接口报 401，帮忙看下")
    await deliver(agent, "确认")
    assert ctx.store.get(1).missing_info == ("affected_system",)
    assert out.kind == "plain"


def test_intake_state_round_trips_through_middle_context() -> None:
    class S:
        middle_context: dict = {}

    progress = IntakeProgress(asked_slots=["planned_window"], ask_rounds=1)
    intake_state.save(S(), progress)
    assert S.middle_context == {"intake": {"asked_slots": ["planned_window"], "ask_rounds": 1}}
    loaded = intake_state.load(S())
    assert loaded.asked_slots == ["planned_window"] and loaded.ask_rounds == 1
    assert intake_state.load(type("E", (), {"middle_context": {}})()).ask_rounds == 0


# --- Ollama 的 tool_call id 撞车（真机丢写的根因） -------------------------


async def test_same_tool_retried_in_one_reply_is_not_dropped() -> None:
    """`OllamaChatModel` 的 id 只在一次响应内唯一，同一 reply 里重试同名工具会撞车。

    撞车的后果不是报错而是静默丢弃：框架按"本条消息里已有的 tool_result id"
    判定调用已完成（`state/_state.py:398`），第二次调用连 tool_result 都不会有。
    """
    store = TicketStore()
    store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    good = {"ticket_id": 1, "assignee": "101", "rationale": "owner 且有余量", "confidence": 0.85}
    turns = [
        Turn(tool_calls=[("assign_ticket", {k: v for k, v in good.items() if k != "confidence"})], ollama_ids=True),
        Turn(tool_calls=[("assign_ticket", good)], ollama_ids=True),
        Turn(text="已处理。"),
    ]
    agent, ctx = await make_agent(FakeIndex(), store, model=ScriptedChatModel(turns))
    await agent.reply(UserMsg(name="user", content="派个单"))

    blocks = agent.state.context[-1].get_content_blocks("tool_result")
    assert [b.state for b in blocks if b.name == "assign_ticket"] == ["error", "success"]
    assert ctx.store.get(1).assignee_id == 101, "改正后的重试必须真的执行"


# --- 工具返回通道：state 与 metadata ---------------------------------------


async def test_refused_assignment_surfaces_as_error() -> None:
    """离职闸要能在事件流里看见，而不只是话术里一句"被拒绝"。"""
    store = TicketStore()
    store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    turns = [
        Turn(tool_calls=[("assign_ticket", {
            "ticket_id": 1, "assignee": "109", "rationale": "senior 且是唯一会做的", "confidence": 0.7,
        })], id_prefix="r"),
        Turn(text="这个人已离职，我换人。"),
    ]
    agent, ctx = await make_agent(FakeIndex(), store, model=ScriptedChatModel(turns))
    await agent.reply(UserMsg(name="user", content="派个单"))

    block = ctx_block(agent, "assign_ticket")
    assert block.state == "error"
    assert block.metadata["refused"] == "109 不在职，不能被指派"
    assert ctx.store.get(1).assignee_id is None


def ctx_block(agent: object, name: str):
    for msg in reversed(agent.state.context):  # type: ignore[attr-defined]
        for block in msg.get_content_blocks("tool_result"):
            if block.name == name:
                return block
    raise AssertionError(f"no tool_result for {name}")
