"""AG-UI 桥发给 `POST /chat` 的 body 必须过服务端的真 schema。

真机代价：A5 曾经用桥自己写的那个字面量去验桥自己，小写的 `"user_confirm_result"`
因此在线下全绿、线上 422 —— `input` 的判别字段取的是 `EventType` 的值（全大写），
小写那条只是它在 AG-UI 侧的 CUSTOM 事件名。所以这里的判据一律换成 `ChatRequest`，
fixture 用 `data/p5_agui_events.jsonl` 里真机流出的那一帧，不手写形状。
"""
from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

import pytest  # noqa: E402
from agentscope.app._router._schema import ChatRequest  # noqa: E402
from agentscope.event import UserConfirmResultEvent  # noqa: E402
from agentscope.message import Msg, ToolCallBlock  # noqa: E402
from pydantic import ValidationError  # noqa: E402

from helpdesk.agui_bridge import (  # noqa: E402
    AguiBridge,
    _interrupts,
    _Thread,
    confirmable_tool_call,
)

#: 真机 2026-09-27 15:38 从 /sessions/{id}/stream 抓到的那一帧（require_user_confirm）。
LIVE_CUSTOM = json.loads(
    '{"id":"97c0161c0fd5443abe077456926b3f66","created_at":"2026-09-27T15:38:38.928912",'
    '"metadata":{},"type":"REQUIRE_USER_CONFIRM",'
    '"reply_id":"80fdd98e1da84b1f946149c2ccd6c7c8","tool_calls":[{"type":"tool_call",'
    '"id":"m0_0_create_ticket","name":"create_ticket",'
    '"input":"{\\"title\\":\\"下单接口返回401令牌过期\\",\\"category\\":\\"incident\\",\\"priority\\":\\"P0\\"}",'
    '"state":"asking","suggested_rules":[{"tool_name":"create_ticket","behavior":"allow",'
    '"source":"suggested"}],"created_at":"2026-09-27T15:38:38.873850"}]}'
)


def _bridge(on_ready=None) -> AguiBridge:
    return AguiBridge(
        base_url="http://unused",
        user_id="u",
        system_prompt="p",
        model="m",
        max_iters=8,
        context_config={"trigger_ratio": 0.6, "tool_result_limit": 1500},
        on_ready=on_ready,
    )


async def _idle() -> None:
    await asyncio.sleep(0)


@pytest.fixture
async def parked():
    """一条真·待确认线程：桥刚从真机帧里把它 park 下来。"""
    thread = _Thread("s", "a", asyncio.Queue(), asyncio.create_task(_idle()))
    bridge = _bridge()
    bridge._park(thread, LIVE_CUSTOM)  # noqa: SLF001
    return bridge, thread


def test_resolved_resume_passes_server_schema(parked):
    """确认表态 → ChatRequest 收得下的 UserConfirmResultEvent。"""
    bridge, thread = parked
    interrupt_id = _interrupts(thread.pending)[0]["id"]
    body = bridge._trigger_body(thread, {"resume": [{"interruptId": interrupt_id, "status": "resolved"}]})  # noqa: SLF001

    event = ChatRequest.model_validate(body).input
    assert isinstance(event, UserConfirmResultEvent)
    assert event.reply_id == LIVE_CUSTOM["reply_id"]
    assert event.confirm_results[0].confirmed is True
    assert event.confirm_results[0].tool_call.id == "m0_0_create_ticket"


def test_cancelled_resume_passes_server_schema(parked):
    bridge, thread = parked
    interrupt_id = _interrupts(thread.pending)[0]["id"]
    body = bridge._trigger_body(thread, {"resume": [{"interruptId": interrupt_id, "status": "cancelled"}]})  # noqa: SLF001

    event = ChatRequest.model_validate(body).input
    assert isinstance(event, UserConfirmResultEvent)
    assert event.confirm_results[0].confirmed is False
    assert thread.pending == {}  # 答完不欠状态


def test_user_text_passes_server_schema(parked):
    """普通一句话 → Msg，且 content 是内容块数组（裸字符串会 422）。"""
    bridge, thread = parked
    body = bridge._trigger_body(  # noqa: SLF001
        thread,
        {"messages": [{"role": "assistant", "content": "上一轮"}, {"role": "user", "content": "打印机坏了"}]},
    )
    msg = ChatRequest.model_validate(body).input
    assert isinstance(msg, Msg)
    assert [b.text for b in msg.get_content_blocks()] == ["打印机坏了"]


def test_streamed_tool_call_cannot_be_echoed_verbatim(parked):
    """钉住 `confirmable_tool_call` 存在的理由：出流帧原样回递过不了必填校验。

    `RequireUserConfirmEvent` 出流时走 `model_dump(exclude_none=True)`，`suggested_rules`
    里那条因此丢了必填的 `rule_content`。代价：「以后都允许这个工具」过不了 AG-UI。
    """
    raw = LIVE_CUSTOM["tool_calls"][0]
    with pytest.raises(ValidationError):
        ToolCallBlock.model_validate(raw)
    ToolCallBlock.model_validate(confirmable_tool_call(raw))


def test_old_lowercase_discriminator_would_be_rejected(parked):
    """回归护栏：这条就是 2026-09-27 那个 422，别再用回小写。"""
    bridge, thread = parked
    body = bridge._trigger_body(  # noqa: SLF001
        thread,
        {"resume": [{"interruptId": _interrupts(thread.pending)[0]["id"], "status": "resolved"}]},
    )
    assert body["input"]["type"] == "USER_CONFIRM_RESULT"


def _client_with(handler, **bridge_kwargs):
    import httpx

    bridge = _bridge(**bridge_kwargs)
    bridge._client = httpx.AsyncClient(  # noqa: SLF001
        transport=httpx.MockTransport(handler),
        base_url="http://unused",
    )
    return bridge


async def test_trigger_waits_out_the_previous_run_tidy_up(monkeypatch):
    """钉住真机那个 409：`RUN_FINISHED` 不等于 session 空了。

    框架发完终点帧，同一条 run task 还要写消息、写 AgentState，然后
    `_auto_name_session` 再调一次模型给 session 起标题（每 session 一次，
    所以撞上的必然是首轮）；`ChatRunRegistry.spawn` 见未结束的 task 就拒触发。
    """
    import httpx

    monkeypatch.setattr("helpdesk.agui_bridge.TRIGGER_RETRY_INTERVAL", 0.01)
    attempts: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        attempts.append(json.loads(request.content))
        if len(attempts) < 3:
            return httpx.Response(
                409,
                json={"detail": "Session 's' already has an active chat run in this process."},
            )
        return httpx.Response(200, json={"status": "started", "session_id": "s", "reply_id": "r"})

    bridge = _client_with(handler)
    thread = _Thread("s", "a", asyncio.Queue(), asyncio.create_task(_idle()))
    thread.queue.put_nowait({"type": "CUSTOM", "name": "仍在善后时掉进来的帧"})

    r = await bridge._trigger(thread, {"agent_id": "a", "session_id": "s"})  # noqa: SLF001
    assert r.status_code == 200
    assert len(attempts) == 3
    assert thread.queue.empty()  # 重试前重清队列，善后帧不能跟着本轮转发
    await bridge._client.aclose()


async def test_trigger_does_not_retry_other_conflicts(monkeypatch):
    """只等这一种 409：别把真冲突（或别的 4xx）也吞成等待。"""
    import httpx

    monkeypatch.setattr("helpdesk.agui_bridge.TRIGGER_RETRY_INTERVAL", 0.01)
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(409, json={"detail": "Session is locked by another worker."})

    bridge = _client_with(handler)
    thread = _Thread("s", "a", asyncio.Queue(), asyncio.create_task(_idle()))
    r = await bridge._trigger(thread, {"agent_id": "a", "session_id": "s"})  # noqa: SLF001
    assert r.status_code == 409
    assert len(calls) == 1
    await bridge._client.aclose()


def test_loopback_client_ignores_the_system_proxy():
    """回环自调不能被系统代理接走。

    真机：httpx 默认 `trust_env=True`，代理来自 `urllib.request.getproxies()`，
    macOS 下它读系统代理又不认那份 bypass 名单 —— 于是桥调自己的
    `GET /credential/` 被本机代理回一个空正文 502，uvicorn 侧连请求都没见过。
    """
    bridge = _bridge()
    assert bridge._client.trust_env is False  # noqa: SLF001


async def test_run_always_ends_with_a_terminal_frame(monkeypatch):
    """桥自己出异常也得给 AG-UI 一个终点。

    真机：一次回环 GET 被本机代理拦成 502，生成器裸着断掉，CopilotKit 只能报
    "Run ended without emitting a terminal event"，前端看起来像"话没发出去"。
    """
    bridge = _bridge()

    async def boom():
        raise RuntimeError("loopback 502")

    monkeypatch.setattr(bridge, "_ensure_started", boom)
    chunks = [
        c
        async for c in bridge.run(
            {"threadId": "t", "runId": "r", "messages": [{"role": "user", "content": "打印机坏了"}]},
        )
    ]
    assert len(chunks) == 1
    frame = json.loads(chunks[0].removeprefix("data: ").strip())
    assert frame["type"] == "RUN_ERROR"
    assert frame["code"] == "bridge_failed"
    assert (frame["threadId"], frame["runId"]) == ("t", "r")
    await bridge._client.aclose()


async def test_forwarded_run_begins_with_run_started():
    """本轮第一帧只能是本轮的 RUN_STARTED（真机 @ag-ui/client 抛 INCOMPLETE_STREAM）。

    常驻订阅会在两轮之间掉进上一轮的善后帧（session 改名通知等），它不能当第一帧；
    同时 threadId/runId 得换成调用方的，而不是服务端那一套。
    """
    import httpx

    queue: asyncio.Queue[dict] = asyncio.Queue()
    leftovers = [
        {"type": "CUSTOM", "name": "session_updated"},  # 上一轮善后时掉进来的
        {"type": "RUN_STARTED", "threadId": "srv-session", "runId": "srv-run"},
        {"type": "TEXT_MESSAGE_CONTENT", "delta": "好的"},
        {"type": "RUN_FINISHED", "threadId": "srv-session", "runId": "srv-run"},
    ]

    def handler(request: httpx.Request) -> httpx.Response:
        for frame in leftovers:
            queue.put_nowait(frame)
        return httpx.Response(200, json={"status": "started", "session_id": "s", "reply_id": "r"})

    bridge = _client_with(handler)

    async def noop() -> None:
        return None

    async def thread_for(thread_id: str) -> _Thread:
        return _Thread("s", "a", queue, asyncio.create_task(_idle()))

    bridge._ensure_started = noop  # type: ignore[method-assign]
    bridge._thread_for = thread_for  # type: ignore[method-assign]

    out = [
        json.loads(c.removeprefix("data: ").strip())
        async for c in bridge.run(
            {"threadId": "t", "runId": "r", "messages": [{"role": "user", "content": "打印机坏了"}]},
        )
    ]
    assert [f["type"] for f in out] == ["RUN_STARTED", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED"]
    assert (out[0]["threadId"], out[0]["runId"]) == ("t", "r")
    await bridge._client.aclose()


async def test_就绪回调拿到的是桥刚认下来的那两个号():
    """AutoDream 的 cron 必须写在**这个** agent 名下，用**这份**模型配置跑。

    桥是这套服务里唯一知道 `agent_id`/`credential_id` 的地方（那两个号是它回环
    POST 出来的），所以后台任务的注册只能从这里挂出去。`on_ready` 排在两个赋值之后，
    `bridge.agent_id` 是一个会在没 start 过时直接 assert 的口 —— 回调能读到值，
    本身就证明了顺序。
    """
    import httpx

    seen: dict = {}

    async def on_ready(b: AguiBridge) -> None:
        seen["agent"] = b.agent_id
        seen["model"] = b.chat_model_config

    def handler(request: httpx.Request) -> httpx.Response:
        path, method = request.url.path, request.method
        if (path, method) == ("/credential/", "GET"):
            return httpx.Response(200, json={"credentials": []})
        if (path, method) == ("/credential/", "POST"):
            return httpx.Response(200, json={"credential_id": "cred-1"})
        if (path, method) == ("/agent/", "GET"):
            return httpx.Response(200, json={"agents": []})
        return httpx.Response(200, json={"agent_id": "agent-1"})

    bridge = _client_with(handler, on_ready=on_ready)
    await bridge.start()
    assert seen["agent"] == "agent-1"
    # 后台整理与前台对话同一个模型、同一组参数：temp=0，关思考
    assert seen["model"] == {
        "type": "ollama_credential",
        "credential_id": "cred-1",
        "model": "m",
        "parameters": {"temperature": 0.0, "thinking_enable": False},
    }
    await bridge._client.aclose()


def test_导入业务包之后回环不再交给系统代理() -> None:
    """真机 2026-09-27 20:13：只走 console 路径的探针收到一个空正文 502 ——
    那两道 `NO_PROXY` 当时只写在 `service.py` 里，管不到 `agent_factory` / `knowledge`
    自己建的模型与嵌入客户端。判据用 httpx 自己的解析结果，不重读我们设的那个字面量
    （P5 §2 的教训）：`all://127.0.0.1` 必须**在场且为 None**，缺键也读作 None。"""
    from httpx._utils import get_environment_proxies  # 私有：这正是 Client 内部用的那个函数

    mounts = get_environment_proxies()
    for host in ("all://127.0.0.1", "all://localhost", "all://[::1]"):
        assert host in mounts and mounts[host] is None, f"{host} 没被 bypass：{mounts}"
