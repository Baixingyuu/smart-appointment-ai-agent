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

from helpdesk.app import _summary  # noqa: E402
from helpdesk.agui_bridge import AguiBridge  # noqa: E402
from helpdesk.dispatch import AssignmentDecision  # noqa: E402
from helpdesk.eval.fakes import FakeIndex, history_hit  # noqa: E402
from helpdesk.eval.scripted_model import ScriptedChatModel, Turn  # noqa: E402
from helpdesk.runtime import intake as intake_state  # noqa: E402
from helpdesk.runtime.agent_factory import (  # noqa: E402
    CHAT_MODEL,
    CONTEXT_CONFIG,
    CONTEXT_TRIGGER_RATIO,
    MAX_ITERS,
    SYSTEM_PROMPT,
    TIMEZONE,
    TOOL_RESULT_LIMIT,
    LocalClockAgent,
    make_agent,
)
from helpdesk.runtime.confirm_bridge import (  # noqa: E402
    APPROVE,
    NEW_REQUEST,
    REJECT,
    classify,
    deliver,
)
from helpdesk.domain import Category, IntakeProgress, Priority  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402


async def scripted(turns: list[Turn]) -> tuple:
    model = ScriptedChatModel(turns)
    return await make_agent(FakeIndex(), model=model), model


def _tools(ctx: HelpdeskContext) -> dict:
    """系统内入口：按名字取工具对象再 `await tool.call(...)`。

    权限闸长在 Agent 那一侧，`call` 不经它 —— 越选/离职闸这类内核规则
    不依赖模型面，走这条直调入口就能测（不经过确认闸，所以不是模型的行为）。
    """
    return {t.name: t for t in build_tools(ctx)}


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


async def test_toolkit_exposes_business_tools_plus_native_search() -> None:
    (agent, _), _ = await scripted([Turn(text="收尾")])
    schemas = await agent.toolkit.get_tool_schemas()
    names = {s["function"]["name"] for s in schemas}
    assert names == {
        "match_service",
        "find_open_ticket",
        "ask_user",
        "create_ticket",
        "assign_ticket",
        "suggest_assignment",
        # P6-b 上门预约
        "propose_appointments",
        "book_appointment",
        "reschedule_appointment",
        "close_appointment",
        "upcoming_appointments",
        # 长期记忆的读口：不落盘的上下文里它如实回"没有偏好表"，所以一直可见。
        # 写口 `run_autodream` 不在这里 —— 它只在挂了日志的上下文里注册。
        "recall_preferences",
        "search_knowledge",  # 框架白送的那个
    }


# --- 上下文压缩阈值与本地时钟（P6-a①）--------------------------------------


async def test_压缩阈值取0_6而不是框架默认0_8() -> None:
    """阈值是框架自己的 `ContextConfig`，这里只钉"我们改过并且改对了方向"：
    触发点 = `trigger_ratio * model.context_size`（`_agent.py:516`），
    0.6 比 0.8 早收敛 1/3 个窗口，而框架的上界是 le=0.9。
    """
    (agent, _), _ = await scripted([Turn(text="收尾")])
    cfg = agent.context_config
    assert cfg.trigger_ratio == CONTEXT_TRIGGER_RATIO == 0.6
    assert cfg.tool_result_limit == TOOL_RESULT_LIMIT
    assert cfg.trigger_ratio < 0.8  # 框架默认值
    assert 0.0 < cfg.trigger_ratio <= 0.9  # 框架的 le=0.9 硬界
    threshold = cfg.trigger_ratio * agent.model.context_size
    assert threshold == 0.6 * 32768
    assert threshold < 0.8 * agent.model.context_size
    # 框架只在 buffer < trigger 时允许注入运行期状态，改小阈值不能撞到这条
    assert cfg.context_buffer_ratio < cfg.trigger_ratio


async def test_两条路径的阈值是同一份不是各写一遍() -> None:
    """console 吃 ContextConfig 对象，托管吃 POST /agent/ 的 JSON：
    两边数字一旦分开，测出来的"60% 就收敛"就不知道是哪条路径的了。"""
    (agent, _), _ = await scripted([Turn(text="收尾")])
    assert CONTEXT_CONFIG == {
        "trigger_ratio": agent.context_config.trigger_ratio,
        "tool_result_limit": agent.context_config.tool_result_limit,
    }
    body = await _agent_post()
    assert body["context_config"] == CONTEXT_CONFIG
    assert isinstance(body["context_config"]["trigger_ratio"], float)


async def test_托管与console都是本地时钟的agent() -> None:
    """框架默认往上下文里注入 UTC 墙钟（`InjectionConfig().timezone == 'UTC'`），
    而上门窗口比的是本地时刻：差八小时等于把"下午三点"写成 07:00。
    console 路径直接传 config；托管路径不传 injection_config，唯一入口是
    `custom_agent_cls`（`_app.py:301` 收下 → `_lifespan.py:175` 交给 ChatService）。"""
    from agentscope.agent import Agent, InjectionConfig

    from helpdesk.service import make_service_app

    assert InjectionConfig().timezone == "UTC"
    (agent, _), _ = await scripted([Turn(text="收尾")])
    assert agent.injection_config.timezone == TIMEZONE
    assert agent.injection_config.inject_runtime_state

    # 托管路径拿不到 injection_config，只拿到类：类的默认值就是它唯一能做的修正
    hosted = LocalClockAgent(name="helpdesk", system_prompt="p", model=agent.model)
    assert hosted.injection_config.timezone == TIMEZONE
    plain = Agent(name="helpdesk", system_prompt="p", model=agent.model)
    assert plain.injection_config.timezone == "UTC"

    app = make_service_app(FakeIndex())
    assert app.state.custom_agent_cls is LocalClockAgent


def _bridge() -> AguiBridge:
    """真桥，配置照 `service.py` 那条构造 —— 阈值只有一份，不在测试里另写一遍。"""
    return AguiBridge(
        base_url="http://unused",
        user_id="u",
        system_prompt=SYSTEM_PROMPT,
        model=CHAT_MODEL,
        max_iters=MAX_ITERS,
        context_config=CONTEXT_CONFIG,
    )


async def _agent_post() -> dict:
    """真桥 `start()` 递出去的 POST /agent/ body。"""
    seen: dict = {}

    class _Response:
        status_code = 200

        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return {"credentials": [], "agents": [], "id": "a1", "credential_id": "c1"}

    class _Client:
        async def get(self, url, params=None):
            return _Response()

        async def post(self, url, json=None):
            seen[url] = json
            return _Response()

    bridge = _bridge()
    bridge._client = _Client()  # noqa: SLF001
    await bridge.start()  # noqa: SLF001
    return seen["/agent/"]


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
        Turn(text="工单 #1 已建好，建议由张伟（接口组、该服务 owner）处理，等他认领。"),
    ]


async def test_happy_path_parks_then_confirms_then_recommends() -> None:
    (agent, ctx), model = await scripted(_parked_turns())
    first = await deliver(agent, COMPLAIN)
    assert first.pending is not None
    assert [t.name for t in first.pending.tool_calls] == ["create_ticket"]
    assert ctx.store.tickets == {}  # park 期间没落库

    second = await deliver(agent, "嗯嗢先这样吧")
    assert second.pending is None
    ticket = ctx.store.get(1)
    # 推荐候选这一步不落负责人：脚本轨迹止于推荐（用户还没选定），所以落不了 assignee
    assert ticket.status.value == "pending" and ticket.assignee_id is None
    assert [p.kind.value for p in ctx.store.progress_of(1)] == ["created"]
    assert ctx.store.assignments == []
    block = ctx_block(agent, "match_service")
    assert block.metadata["services"] == [2001]
    assert 101 in block.metadata["employeeCandidates"], "推荐材料必须含候选人"
    # 一次确认覆盖整条链路：建单 + 取推荐材料都在这条 reply 里跑完
    assert model.n_calls == 5


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

    载体用 `propose_appointments` 而不是指派：它是 ALLOW 的只读件，`need` 有
    min_length=1 的约束，能复现"第一次不合规、改正后重调"的形状。
    """
    bad = {"need": "", "earliest": "2026-09-28 09:00", "latest": "2026-09-28 18:00"}
    good = dict(bad, need="现场换令牌并验证下单接口")
    turns = [
        Turn(tool_calls=[("propose_appointments", bad)], ollama_ids=True),
        Turn(tool_calls=[("propose_appointments", good)], ollama_ids=True),
        Turn(text="已处理。"),
    ]
    agent, ctx = await make_agent(FakeIndex(), TicketStore(), model=ScriptedChatModel(turns))
    await agent.reply(UserMsg(name="user", content="帮我约个人上门"))

    blocks = [b for b in agent.state.context[-1].get_content_blocks("tool_result") if b.name == "propose_appointments"]
    assert [b.state for b in blocks] == ["error", "success"]
    assert len(blocks) == 2, "第二次调用必须有自己的 tool_result，不能被静默丢掉"


# --- 工具返回通道：state 与 metadata ---------------------------------------


async def test_refused_assignment_surfaces_as_error() -> None:
    """离职闸要能在返回通道里看见，而不只是话术里一句"被拒绝"。

    载体是直接调用（`FunctionTool.call`）：权限闸在 Agent 那一侧，`call` 不经它，
    所以这条路测的是内核规则本身，不是模型行为。
    """
    ctx = HelpdeskContext(index=FakeIndex(), store=TicketStore())
    ctx.store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )

    chunk = await _tools(ctx)["assign_ticket"].call(
        ticket_id=1, assignee="109", rationale="senior 且是唯一会做的", confidence=0.7
    )
    assert chunk.state == "error"
    assert chunk.metadata["refused"] == "109 不在职，不能被指派"
    assert ctx.store.get(1).assignee_id is None


async def test_assignment_parks_for_confirmation() -> None:
    """assign_ticket 是写操作，必须走确认闸：模型调它先 park，用户确认才落。

    口径从「指派由人在系统外做（DENY）」改成「用户选定后经确认闸落（ASK）」：
    模型面重新看得见 assign_ticket，但落库前必须等人确认 —— 没人确认就落不了负责人。
    """
    store = TicketStore()
    store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    turns = [
        Turn(tool_calls=[("assign_ticket", {
            "ticket_id": 1, "assignee": "101", "rationale": "owner", "confidence": 0.9,
        })], id_prefix="r"),
        Turn(text="好的，已按您的选择指派给张伟（接口组）。"),
    ]
    agent, ctx = await make_agent(FakeIndex(), store, model=ScriptedChatModel(turns))
    out = await deliver(agent, "就派给 101 张伟吧")

    # park：没确认前不落库
    assert out.pending is not None
    assert [t.name for t in out.pending.tool_calls] == ["assign_ticket"]
    assert ctx.store.get(1).assignee_id is None
    assert ctx.store.assignments == []

    # 确认后落库
    out2 = await deliver(agent, "确认")
    assert out2.pending is None
    assert ctx.store.get(1).assignee_id == 101
    assert [a.assignee_id for a in ctx.store.assignments] == [101]


def ctx_block(agent: object, name: str):
    for msg in reversed(agent.state.context):  # type: ignore[attr-defined]
        for block in msg.get_content_blocks("tool_result"):
            if block.name == name:
                return block
    raise AssertionError(f"no tool_result for {name}")


# --- 派单 v4：三源召回住在工具里，不由模型自愿调用 ----------------------------


def result_text(block) -> str:
    out = block.output
    if isinstance(out, str):
        return out
    first = out[0]
    return first.text if hasattr(first, "text") else first["text"]


async def test_match_service_returns_all_three_sources() -> None:
    """一次调用给齐服务定位 + 候选人 + 已结历史；模型跳过任何一源的机会都没有。"""
    index = FakeIndex(history=(history_hit(7, "下单接口 500", 101, 2001, 0.62),))
    turns = [
        Turn(tool_calls=[("match_service", {"query": "下单接口报 401"})], id_prefix="m"),
        Turn(text="材料齐了。"),
    ]
    agent, ctx = await make_agent(index, TicketStore(), model=ScriptedChatModel(turns))
    await agent.reply(UserMsg(name="user", content="派个单"))

    block = ctx_block(agent, "match_service")
    text = result_text(block)
    assert "【服务字典命中】" in text and "【召回候选人】" in text and "【已结历史工单" in text
    assert "owner=101" in text and "工单 7" in text
    assert block.metadata["employeeCandidates"] == [101, 107]
    assert block.metadata["history"] == [7]


async def test_over_selection_is_annotated_not_blocked() -> None:
    """越选（选了没召回的人）只提醒 + 记账：硬拦会把召回漏检伪装成无人可派。

    模型面已经禁掉指派，这两条改测规则本身 —— 走 `FunctionTool.call` 的系统内入口，
    前提仍然是先跑一次 `match_service`（三源召回住在工具里，不由调用方自愿）。
    """
    ctx = HelpdeskContext(index=FakeIndex(), store=TicketStore())
    ctx.store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    tools = _tools(ctx)
    await tools["match_service"].call(query="下单接口报 401")
    chunk = await tools["assign_ticket"].call(
        ticket_id=1, assignee="105", rationale="前端骨干", confidence=0.6
    )

    assert chunk.metadata["in_recall"] is False
    assert "召回漏检" in chunk.content[0].text
    assert ctx.store.get(1).assignee_id == 105, "没有拦下来"
    assert ctx.store.assignments[-1].in_recall is False


async def test_assignment_writes_history_that_stays_out_of_recall() -> None:
    """派完就把单写进历史集合，但 resolved=False → 读取句柄的过滤键把它挡在外面。"""
    index = FakeIndex()
    ctx = HelpdeskContext(index=index, store=TicketStore())
    ctx.store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    tools = _tools(ctx)
    await tools["match_service"].call(query="下单接口报 401")
    await tools["assign_ticket"].call(
        ticket_id=1, assignee="101", rationale="owner", confidence=0.8
    )

    assert len(index.written) == 1
    row = index.written[0]
    assert (row["ticket_id"], row["assignee_id"], row["service_id"]) == (1, 101, 2001)
    assert row["resolved"] is False


def _one_ticket_store() -> TicketStore:
    store = TicketStore()
    store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    return store


def test_console_summary_distinguishes_never_assigned_from_escalated() -> None:
    """摘要是给人看的一屏：「助手不直接派单」之后 `assignee_id is None` 不再等于转人工，两种 None 必须分开。"""
    plain = _summary(_one_ticket_store())
    store = _one_ticket_store()
    store.assign(1, AssignmentDecision(assignee="ESCALATE_HUMAN", rationale="无人可派", confidence=0.2))
    escalated = _summary(store)
    store2 = _one_ticket_store()
    store2.assign(1, AssignmentDecision(assignee="101", rationale="owner", confidence=0.8))
    assigned = _summary(store2)

    assert "未指派（由人落）" in plain and "转人工" not in plain
    assert "转人工待认领" in escalated
    assert "员工 101" in assigned
