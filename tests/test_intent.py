"""意图路由：分类器结构 + 路由中间件的工具面裁剪 / 流程注入 / 连续性。

这里不测真实 LLM 分类（要起 ollama），测的是路由层本身：给定一个分类结果，
工具面和流程指令是否正确、以及"流程内消息不重分类"这条连续性规则是否守住。
"""
from __future__ import annotations

import pytest

from helpdesk.domain import Category, Priority
from helpdesk.intent import IntentAnswer
from helpdesk.runtime import intent_router as router
from helpdesk.runtime.intent_router import IntentRouterMiddleware
from helpdesk.ticket_store import TicketStore


class _State:
    def __init__(self) -> None:
        self.middle_context: dict = {}


class _Agent:
    #: classify_intent 在测试里被 monkeypatch，不关心真实 model 值。
    model = None

    def __init__(self) -> None:
        self.state = _State()


class _Msg:
    role = "user"

    def __init__(self, text: str) -> None:
        self._text = text

    def get_text_content(self) -> str:
        return self._text


def _schema(name: str) -> dict:
    return {"type": "function", "function": {"name": name}}


TOOLS = [_schema("search_knowledge"), _schema("create_ticket"), _schema("assign_ticket")]


# --- 分类器结构 -------------------------------------------------------------


def test_intent_answer_validates_enum_and_confidence() -> None:
    assert IntentAnswer(intent="incident", confidence=0.9).intent == "incident"
    with pytest.raises(Exception):
        IntentAnswer(intent="not_a_real_intent", confidence=0.5)
    with pytest.raises(Exception):
        IntentAnswer(intent="incident", confidence=1.5)


def test_extract_user_text_skips_confirm_and_returns_user_text() -> None:
    assert router._extract_user_text(_Msg("系统报错")) == "系统报错"
    assert router._extract_user_text(None) is None
    assert router._extract_user_text([_Msg("   "), _Msg("下单接口")]) == "下单接口"


# --- 工具面裁剪 -------------------------------------------------------------


async def test_knowledge_only_keeps_search_knowledge() -> None:
    agent = _Agent()
    agent.state.middle_context[router.INTENT_KEY] = {"intent": "knowledge", "confidence": 0.9}
    seen: dict = {}

    async def next_handler(**kwargs):
        seen.update(kwargs)
        return "ok"

    await IntentRouterMiddleware().on_model_call(agent, {"tools": list(TOOLS)}, next_handler)
    assert [t["function"]["name"] for t in seen["tools"]] == ["search_knowledge"]


async def test_incident_keeps_full_tool_surface() -> None:
    agent = _Agent()
    agent.state.middle_context[router.INTENT_KEY] = {"intent": "incident", "confidence": 0.8}
    seen: dict = {}

    async def next_handler(**kwargs):
        seen.update(kwargs)
        return "ok"

    await IntentRouterMiddleware().on_model_call(agent, {"tools": list(TOOLS)}, next_handler)
    assert "tools" not in seen, "incident 不裁剪，原样透传"


async def test_handoff_clears_tool_surface() -> None:
    agent = _Agent()
    agent.state.middle_context[router.INTENT_KEY] = {"intent": "handoff", "confidence": 0.7}
    seen: dict = {}

    async def next_handler(**kwargs):
        seen.update(kwargs)
        return "ok"

    await IntentRouterMiddleware().on_model_call(agent, {"tools": list(TOOLS)}, next_handler)
    assert seen["tools"] == []


# --- 连续性：流程内消息不重分类 ---------------------------------------------


def test_active_flow_detected_when_asking() -> None:
    agent = _Agent()
    agent.state.middle_context["intake"] = {"asked_slots": ["affected_system"], "ask_rounds": 1}
    assert IntentRouterMiddleware()._in_active_flow(agent) is True


def test_active_flow_detected_when_ticket_unassigned() -> None:
    store = TicketStore()
    store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    assert IntentRouterMiddleware(store=store)._in_active_flow(_Agent()) is True


def test_no_active_flow_when_idle() -> None:
    store = TicketStore()
    assert IntentRouterMiddleware(store=store)._in_active_flow(_Agent()) is False


async def test_on_reply_classifies_only_new_requests(monkeypatch) -> None:
    captured: dict = {}

    async def fake_classify(model, text):
        captured["text"] = text
        return IntentAnswer(intent="incident", confidence=0.9)

    monkeypatch.setattr(router, "classify_intent", fake_classify)

    agent = _Agent()
    mw = IntentRouterMiddleware()

    async def next_handler():
        yield "e1"

    events = [e async for e in mw.on_reply(agent, {"inputs": _Msg("系统报错")}, next_handler)]
    assert events == ["e1"]
    assert captured == {"text": "系统报错"}
    assert agent.state.middle_context[router.INTENT_KEY]["intent"] == "incident"


async def test_on_reply_skips_classify_during_active_flow(monkeypatch) -> None:
    called: list[str] = []

    async def fake_classify(model, text):
        called.append(text)
        return IntentAnswer(intent="chitchat", confidence=0.9)

    monkeypatch.setattr(router, "classify_intent", fake_classify)

    store = TicketStore()
    store.create(
        title="下单接口 401",
        description="下单接口报 401",
        category=Category.INCIDENT,
        priority=Priority.P1,
    )
    agent = _Agent()
    agent.state.middle_context[router.INTENT_KEY] = {"intent": "incident", "confidence": 0.9}

    async def next_handler():
        yield "e1"

    events = [e async for e in IntentRouterMiddleware(store=store).on_reply(
        agent, {"inputs": _Msg("选 101 吧")}, next_handler,
    )]
    assert events == ["e1"]
    assert called == [], "有未指派工单时，'选 101' 是流程内消息，不该重分类"
    assert agent.state.middle_context[router.INTENT_KEY]["intent"] == "incident"
