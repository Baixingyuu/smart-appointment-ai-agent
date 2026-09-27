"""P5 前端接缝探针：AG-UI 托管路径能不能承载这个业务 agent。

三段，成本不同（B/C 互斥，一次真机只烧一段）：
  A 不起模型 —— 装配、路由面、框架附送的工具面、AG-UI 事件映射覆盖（哪些一等、
     哪些落 CUSTOM）、桥的翻译表（拿服务端的真 schema 当判据）。完全可复现，
     是"框架接管到哪一步"的账面。
  B 起模型，只走 /chat + /sessions/{id}/stream —— 灌 credential/agent/session，
     一句话打到 create_ticket 的待确认，再用框架原生的 UserConfirmResultEvent 恢复，
     然后确认工单真的落库、三源召回真的被调用。这一段验的是 P3 手写的确认桥
     能不能整个删掉。
  C 起模型，只走 POST /ag-ui —— 前端（CopilotKit）看到的就这一个端点：一次 POST、
     同一条响应上读到终点。这一段验的是桥自己（懒建会话、park→interrupt 终点、
     resume[] 恢复、threadId/runId 回写）。

跑法：
  .venv/bin/python probes/p5_agui_service.py --offline   # 只跑 A（不起模型）
  .venv/bin/python probes/p5_agui_service.py             # A + B
  .venv/bin/python probes/p5_agui_service.py agui        # A + C
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import sys
import tempfile
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
logging.getLogger("pymilvus").setLevel(logging.CRITICAL)

# 独立向量库：探针会真建单、真写历史集合，不能碰 data/milvus_lite.db。
os.environ.setdefault("HELPDESK_VECTOR_DB", "./data/p5_service.db")
os.environ.setdefault("HELPDESK_SERVICE_DB", "./data/p5_service_state.db")

import httpx  # noqa: E402
import uvicorn  # noqa: E402
from agentscope.app._router._schema import ChatRequest  # noqa: E402
from agentscope.app.message_bus import InMemoryMessageBus  # noqa: E402
from agentscope.app.middleware import AGUIProtocolMiddleware  # noqa: E402
from agentscope.app.storage import AsyncSQLAlchemyStorage  # noqa: E402
from agentscope.app.workspace_manager import LocalWorkspaceManager  # noqa: E402
from agentscope.event import (  # noqa: E402
    ExceedMaxItersEvent,
    ModelCallStartEvent,
    ReplyStartEvent,
    RequireUserConfirmEvent,
    ToolResultEndEvent,
    UserConfirmResultEvent,
)
from agentscope.message import Msg, ToolCallBlock, ToolResultState  # noqa: E402
from agentscope.workspace import LocalWorkspace  # noqa: E402
from pydantic import ValidationError  # noqa: E402

from helpdesk.agui_bridge import AguiBridge, _interrupts, _Thread, confirmable_tool_call  # noqa: E402
from helpdesk.catalog import ROSTER  # noqa: E402
from helpdesk.knowledge import open_index  # noqa: E402
from helpdesk.runtime.agent_factory import (  # noqa: E402
    CHAT_MODEL,
    MAX_ITERS,
    SYSTEM_PROMPT,
)
from helpdesk.service import USER_ID, make_service_app  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402

PORT = 8013
BASE = f"http://127.0.0.1:{PORT}"
UTTERANCE = "下单接口一直返回 401，说令牌过期了，全部用户都受影响，从今天上午十点开始"
#: 真机实测托管路径第一轮会先 ask_user（它把故障当成了变更来问窗口），所以有第二轮。
FOLLOWUP = (
    "这不是要走变更，是线上现在就在报。服务就是下单接口 order-api，"
    "没有变更窗口这回事，全部用户都进不来。"
)
# 一次真机链路：模型加载 + 若干轮，给足余量但不无限等。
DEADLINE = 480.0


def _paths(app) -> set[str]:
    # fastapi 0.141 的 app.routes 里是惰性的 _IncludedRouter，没有 .path；
    # 路由面只能从 openapi 生成结果读。
    return set(app.openapi()["paths"])


async def phase_a(index) -> tuple[list[str], dict]:
    fails: list[str] = []
    obs: dict[str, object] = {}

    app = make_service_app(index)
    paths = _paths(app)
    needed = {"/chat/", "/sessions/", "/agent/", "/credential/", "/sessions/{session_id}/stream", "/ag-ui"}
    missing = needed - paths
    if missing:
        fails.append(f"A1 缺路由：{sorted(missing)}")
    else:
        print(f"  ✓ A1 装配成功，路由齐（{len(paths)} 条路由）")
    obs["routes"] = len(paths)

    # A2 框架附送的工具面：托管路径每一轮都会挂 workspace builtin + 计划/调度/团队工具。
    with tempfile.TemporaryDirectory() as workdir:
        workspace = LocalWorkspace(workdir=workdir)
        builtin = await workspace.list_tools()
    ours = build_tools(HelpdeskContext(index=index, store=TicketStore()))
    names = sorted(t.name for t in builtin)
    obs["workspace_tools"] = names
    print(
        f"  · A2 托管路径附送 workspace 工具 {len(names)} 个：{names}\n"
        f"       我们的业务工具 {len(ours)} 个：{sorted(t.name for t in ours)}",
    )
    dangerous = [n for n in names if not n.startswith("ListMcp") and not n.startswith("ReadMcp")]
    print(
        f"  · A2 附送但看不见：ToolSurfaceMiddleware 把 {len(dangerous)} 个文件/命令类工具"
        f"（{dangerous}）挡在每次模型调用的 schema 之外 —— 挂进 Toolkit ≠ 递进上下文。",
    )

    # A3 AG-UI 映射覆盖：哪些事件有一等公民，哪些只能靠 CUSTOM 名字约定。
    mw = AGUIProtocolMiddleware(app=lambda *a, **k: None)
    probe_events = [
        ReplyStartEvent(session_id="s", reply_id="r", name="helpdesk"),
        ModelCallStartEvent(reply_id="r", model_name=CHAT_MODEL),
        ToolResultEndEvent(reply_id="r", tool_call_id="c", state=ToolResultState.SUCCESS),
        RequireUserConfirmEvent(
            reply_id="r",
            tool_calls=[ToolCallBlock(type="tool_call", id="c", name="create_ticket", input="{}")],
        ),
        ExceedMaxItersEvent(reply_id="r", name="helpdesk"),
    ]
    mapping: dict[str, str] = {}
    for evt in probe_events:
        converted = mw._to_agui_event(evt)  # noqa: SLF001
        agui_type = getattr(converted, "type", "?")
        custom_name = getattr(converted, "name", None)
        if str(agui_type).upper().endswith("CUSTOM") and custom_name:
            label = f"CUSTOM/{custom_name}"
        else:
            label = str(agui_type)
        mapping[type(evt).__name__] = label
    obs["mapping"] = mapping
    for event_name, label in mapping.items():
        flag = "CUSTOM（前端要靠约定名）" if label.startswith("CUSTOM") else "一等公民"
        print(f"  · A3 {event_name:26s} → {label} [{flag}]")
    if not mapping["RequireUserConfirmEvent"].startswith("CUSTOM"):
        fails.append("A3 RequireUserConfirmEvent 竟然有原生映射，需要更新这条结论")

    # A5 AG-UI 桥的翻译表（不起模型）：park ↔ interrupt 是这一层唯一真正的自研语义。
    bridge = AguiBridge(
        base_url="http://unused",
        user_id=USER_ID,
        system_prompt="p",
        model="m",
        max_iters=8,
    )
    thread = _Thread("s", "a", asyncio.Queue(), asyncio.create_task(asyncio.sleep(0)))
    bridge._park(  # noqa: SLF001
        thread,
        {
            "reply_id": "r1",
            "tool_calls": [
                {"type": "tool_call", "id": "c1", "name": "create_ticket", "input": '{"title":"x"}'},
            ],
        },
    )
    interrupts = _interrupts(thread.pending)
    if len(interrupts) != 1 or interrupts[0]["toolCallId"] != "c1":
        fails.append(f"A5 park 没翻成一条带 toolCallId 的 interrupt：{interrupts}")
    resolved = bridge._trigger_body(  # noqa: SLF001
        thread,
        {"resume": [{"interruptId": interrupts[0]["id"], "status": "resolved"}]},
    )
    # 判据用服务端的真 schema（ChatRequest），不再手写常量：正是手写把小写的
    # "user_confirm_result" 验成了"对"，真机 POST /chat 才 422。
    req = ChatRequest.model_validate(resolved)
    if not (
        isinstance(req.input, UserConfirmResultEvent)
        and req.input.reply_id == "r1"
        and req.input.confirm_results[0].confirmed is True
        and req.input.confirm_results[0].tool_call.id == "c1"
    ):
        fails.append(f"A5 resolved 没翻成 UserConfirmResultEvent：{resolved}")
    bridge._park(thread, {"reply_id": "r2", "tool_calls": [{"type": "tool_call", "id": "c2", "name": "create_ticket", "input": "{}"}]})  # noqa: SLF001
    cancelled = bridge._trigger_body(  # noqa: SLF001
        thread,
        {"resume": [{"interruptId": "r2:c2", "status": "cancelled"}]},
    )
    if not cancelled or ChatRequest.model_validate(cancelled).input.confirm_results[0].confirmed is not False:
        fails.append("A5 cancelled 表态没翻成 confirmed=False")
    if thread.pending:
        fails.append(f"A5 答完还留着待确认：{list(thread.pending)}")
    plain = bridge._trigger_body(  # noqa: SLF001
        thread,
        {"messages": [{"role": "assistant", "content": "上一轮"}, {"role": "user", "content": "打印机坏了"}]},
    )
    plain_msg = ChatRequest.model_validate(plain).input
    if not (
        isinstance(plain_msg, Msg)
        and [b.text for b in plain_msg.get_content_blocks()] == ["打印机坏了"]
    ):
        fails.append(f"A5 用户话没翻成内容块数组：{plain}")
    if bridge._trigger_body(thread, {"messages": []}) is not None:  # noqa: SLF001
        fails.append("A5 空 run 居然给了触发体")
    # 出流形状原样回递必须仍然失败（真机 B4 的 422），修好形状的必须过校验。
    echoed = {"type": "tool_call", "id": "c9", "name": "create_ticket", "input": "{}",
              "suggested_rules": [{"tool_name": "create_ticket", "behavior": "allow", "source": "suggested"}]}
    try:
        UserConfirmResultEvent(
            reply_id="r9",
            confirm_results=[{"confirmed": True, "tool_call": echoed, "rules": None}],
        )
        fails.append("A5 原样回递居然过了 /chat 校验：exclude_none 那条结论要改")
    except ValidationError:
        pass
    UserConfirmResultEvent(  # noqa: S106
        reply_id="r9",
        confirm_results=[{"confirmed": True, "tool_call": confirmable_tool_call(echoed), "rules": None}],
    )
    print("  ✓ A5 桥的翻译表：park→interrupt / resolved→confirmed / 话→内容块")

    # 存储/总线/工作区三件套必须能在没有 Redis 的情况下起来。
    with tempfile.TemporaryDirectory() as tmp:
        try:
            storage = AsyncSQLAlchemyStorage(f"sqlite+aiosqlite:///{tmp}/t.db", create_tables=True)
            async with storage:
                await InMemoryMessageBus().__aenter__()
                LocalWorkspaceManager(f"{tmp}/ws")
            print("  ✓ A4 sqlite + InMemoryMessageBus + LocalWorkspaceManager 无需 Redis")
        except Exception as exc:  # noqa: BLE001
            fails.append(f"A4 无 Redis 装配失败：{type(exc).__name__}: {exc}")
    return fails, obs


async def _wait_for(events: list[dict], predicate, timeout: float, what: str):
    loop = asyncio.get_running_loop()
    end = loop.time() + timeout
    while loop.time() < end:
        for evt in events:
            if predicate(evt):
                return evt
        await asyncio.sleep(0.5)
    raise AssertionError(f"超时等不到：{what}")


def _custom(events: list[dict], name: str) -> dict | None:
    for evt in events:
        if evt.get("type") == "CUSTOM" and evt.get("name") == name:
            return evt
    return None


def _tool_names(events: list[dict]) -> list[str]:
    return [e.get("toolCallName") or e.get("tool_call_name") or "?" for e in events if e.get("type") == "TOOL_CALL_START"]


async def phase_b(index) -> tuple[list[str], dict]:
    fails: list[str] = []
    obs: dict[str, object] = {"events": 0, "event_types": [], "tool_calls": [], "tickets": []}
    app = make_service_app(index, self_base=BASE)
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=PORT, log_level="error"))
    task = asyncio.create_task(server.serve())
    for _ in range(100):
        if server.started:
            break
        await asyncio.sleep(0.2)
    else:
        raise AssertionError("uvicorn 没起来")

    headers = {"X-User-ID": USER_ID}
    events: list[dict] = []
    pump: asyncio.Task | None = None
    try:
        # trust_env=False：探针自己也是回环调用，代理环境一掺和就变成"探针莫名 502"，
        # 那是量具坏了，不是被测对象坏了（桥那边同一个坑，见 agui_bridge）。
        async with httpx.AsyncClient(
            base_url=BASE,
            headers=headers,
            timeout=60.0,
            trust_env=False,
        ) as client:
            r = await client.post("/credential/", json={"data": {"type": "ollama_credential"}})
            r.raise_for_status()
            credential_id = r.json()["credential_id"]
            r = await client.post(
                "/agent/",
                json={
                    "name": "helpdesk",
                    "system_prompt": SYSTEM_PROMPT,
                    "react_config": {"max_iters": MAX_ITERS},
                    "context_config": {"trigger_ratio": 0.8, "tool_result_limit": 1500},
                },
            )
            r.raise_for_status()
            agent_id = r.json()["agent_id"]
            r = await client.post(
                "/sessions/",
                json={
                    "agent_id": agent_id,
                    "name": "p5-probe",
                    "chat_model_config": {
                        "type": "ollama_credential",
                        "credential_id": credential_id,
                        "model": CHAT_MODEL,
                        "parameters": {"temperature": 0.0, "thinking_enable": False},
                    },
                },
            )
            r.raise_for_status()
            session_id = r.json()["session_id"]
            print(f"  ✓ B1 seed 完成 credential={credential_id[:8]} agent={agent_id[:8]} session={session_id[:8]}")

            async def consume() -> None:
                async with client.stream(
                    "GET",
                    f"/sessions/{session_id}/stream",
                    params={"agent_id": agent_id},
                    timeout=None,
                ) as resp:
                    async for line in resp.aiter_lines():
                        if not line.startswith("data:"):
                            continue
                        payload = line[5:].strip()
                        if not payload:
                            continue
                        try:
                            events.append(json.loads(payload))
                        except json.JSONDecodeError:
                            continue

            pump = asyncio.create_task(consume())
            await asyncio.sleep(1.0)

            async def send(text: str, label: str) -> int:
                """发一句话（或一次表态），返回发出前 events 的水位。"""
                after = len(events)
                r = await client.post(
                    "/chat/",
                    json={
                        "agent_id": agent_id,
                        "session_id": session_id,
                        # Msg.content 只收内容块数组，给裸字符串会 422。
                        "input": {
                            "name": "用户",
                            "role": "user",
                            "content": [{"type": "text", "text": text}],
                        },
                    },
                )
                if r.status_code >= 400:
                    raise AssertionError(f"{label} POST /chat 被拒 {r.status_code}：{r.text[:300]}")
                print(f"  ✓ {label} 已触发（{r.json()['status']}）")
                return after

            def parked_in(span: list[dict]) -> dict | None:
                return next(
                    (
                        e
                        for e in span
                        if e.get("type") == "CUSTOM" and e.get("name") == "require_user_confirm"
                    ),
                    None,
                )

            async def wait_turn(after: int, what: str) -> list[dict]:
                loop = asyncio.get_running_loop()
                end = loop.time() + DEADLINE * 0.4
                while loop.time() < end:
                    span = events[after:]
                    # park 的那一轮框架不发 RUN_FINISHED（真机末帧就是那条 CUSTOM），
                    # 所以"这一轮结束了"有两种终态：结束帧，或者待确认帧。
                    if parked_in(span) or any(
                        e.get("type") in ("RUN_FINISHED", "RUN_ERROR") for e in span
                    ):
                        return span
                    await asyncio.sleep(0.5)
                raise AssertionError(f"超时等不到：{what}")

            # 第一轮只是陈述现象；真机实测它会先 ask_user，所以第二轮才谈得上待确认。
            first = await send(UTTERANCE, "B2 第一轮（陈述）")
            span = await wait_turn(first, "第一轮结束（RUN_FINISHED/RUN_ERROR）")
            parked = parked_in(span)
            err = next((e for e in span if e.get("type") == "RUN_ERROR"), None)
            if err is not None:
                fails.append(
                    f"B2 框架直接判死：RUN_ERROR code={err.get('code')} message={err.get('message')!r}",
                )
                raise AssertionError("第一轮就 RUN_ERROR")
            if parked is None:
                asked = "".join(
                    e.get("delta", "") for e in span if e.get("type") == "TEXT_MESSAGE_CONTENT"
                )
                print(f"  · B2 它先追问了：{asked.strip()!r}")
                second = await send(FOLLOWUP, "B2.5 第二轮（回答追问）")
                span = await wait_turn(second, "第二轮结束")
                parked = parked_in(span)
                if parked is None:
                    fails.append("B3 两轮之后仍然没到 require_user_confirm")
                    raise AssertionError("两轮没到待确认")

            value = parked["value"]
            print(
                "  ✓ B3 框架原生 park 到了 AG-UI：CUSTOM/require_user_confirm，"
                f"待确认工具={[t['name'] for t in value['tool_calls']]}",
            )

            # 直接构造并序列化真事件，别再手写判别字段：B4 上一次的 422 就是
            # 手写 "user_confirm_result"（小写）撞上全大写的 EventType 值。
            resume_body = UserConfirmResultEvent(  # noqa: S106
                reply_id=value["reply_id"],
                confirm_results=[
                    {"confirmed": True, "tool_call": confirmable_tool_call(tc), "rules": None}
                    for tc in value["tool_calls"]
                ],
            ).model_dump(mode="json")

            r = await client.post(
                "/chat/",
                json={
                    "agent_id": agent_id,
                    "session_id": session_id,
                    "input": resume_body,
                },
            )
            if r.status_code >= 400:
                fails.append(f"B4 恢复被拒 {r.status_code}：{r.text[:400]}")
                raise AssertionError("B4 恢复失败")
            print("  ✓ B4 用 UserConfirmResultEvent 恢复（没有走任何自研桥）")

            await _wait_for(
                events,
                lambda e: e.get("type") == "TOOL_CALL_START"
                and (e.get("toolCallName") or e.get("tool_call_name")) == "assign_ticket",
                DEADLINE * 0.45,
                "assign_ticket 的 TOOL_CALL_START",
            )

            contexts = app.state.helpdesk_contexts
            ctx = contexts.get(session_id)
            store = ctx.store if ctx else None
            obs["tickets"] = list(store.tickets.values()) if store else []
            if store is None or not store.tickets:
                fails.append("B5 这个 session 的工单簿是空的（contexts 没接到 session_id）")
            else:
                ticket = next(iter(store.tickets.values()))
                log = next(iter(store.assignments), None)
                print(
                    f"  ✓ B5 落库 #{ticket.id} 指派={ticket.assignee_id} "
                    f"in_recall={getattr(log, 'in_recall', None)} 引用={getattr(log, 'cited_tickets', None)}",
                )
                if ticket.assignee_id not in {e.id for e in ROSTER}:
                    fails.append(f"B5 指派对象不在名册里：{ticket.assignee_id}")

            called = _tool_names(events)
            for required in ("match_service", "assign_ticket"):
                if required not in called:
                    fails.append(f"B6 工具 {required} 没被调用")
            print(f"  · B6 真机工具调用序列：{called}")
            print(f"  · B7 AG-UI 事件 {len(events)} 条：{sorted({e.get('type', '?') for e in events})}")
    except AssertionError as exc:
        fails.append(f"B 段中断：{exc}")
        print(f"  · 已观测到的工具序列：{_tool_names(events)}")
        for evt in events[-4:]:
            print(f"  · 末尾事件 {evt.get('type')}: {json.dumps(evt, ensure_ascii=False)[:300]}")
    finally:
        if pump is not None:
            pump.cancel()
            await asyncio.gather(pump, return_exceptions=True)
        # 原始流落盘：下一轮诊断（为什么没到 create_ticket）不该再花一次真机钱。
        dump = Path("data/p5_agui_events.jsonl")
        dump.parent.mkdir(parents=True, exist_ok=True)
        dump.write_text("".join(json.dumps(e, ensure_ascii=False) + "\n" for e in events), encoding="utf-8")
        server.should_exit = True
        await task
        obs["tool_calls"] = _tool_names(events)
        obs["event_types"] = sorted({e.get("type", "?") for e in events})
        obs["events"] = len(events)
    return fails, obs


async def phase_c(index) -> tuple[list[str], dict]:
    """只走 POST /ag-ui —— CopilotKit 眼里有且只有这一个端点。

    B 段证的是"框架原生 HITL 契约能不能承载建单"；C 段证的是桥那一发：一次
    POST、同一条响应上读到终点。三件事必须同时成立：park 翻成
    `RUN_FINISHED.outcome={type:"interrupt"}`、表态走 `resume[]`、工单落库。
    """
    fails: list[str] = []
    obs: dict[str, object] = {"events": 0, "event_types": [], "tool_calls": [], "tickets": []}
    app = make_service_app(index, self_base=BASE)
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=PORT, log_level="error"))
    asyncio.create_task(server.serve())
    for _ in range(100):
        if server.started:
            break
        await asyncio.sleep(0.2)
    else:
        raise AssertionError("uvicorn 没起来")

    # 每次跑都用新 thread：桥按 `agui:<threadId>` 找 session，复用会把上一轮的
    # AgentState 带进来（它记得自己建过单，于是这一轮就不 park 了）。
    thread = f"p5-c-{uuid.uuid4().hex[:6]}"
    frames: list[dict] = []
    try:
        # 不手写 credential/agent/session：桥自己懒建，那也正是被测的一段。
        async with httpx.AsyncClient(
            base_url=BASE,
            headers={"X-User-ID": USER_ID},
            timeout=60.0,
            trust_env=False,
        ) as client:
            # C0 冷启动两次（不花模型钱）：真机连撞三个 bug 的都是"库里已经有东西"
            # 的第二次启动 —— GET 列表里的记录叫 id、name 藏在 data/config 里。
            bridge = app.state.agui_bridge

            async def census() -> tuple[int, int]:
                c = len((await client.get("/credential/")).json().get("credentials", []))
                a = len((await client.get("/agent/")).json().get("agents", []))
                return c, a

            await bridge._ensure_started()  # noqa: SLF001
            ids = (bridge._credential_id, bridge._agent_id)  # noqa: SLF001
            before = await census()
            await bridge.start()  # noqa: SLF001
            after = await census()
            if ids != (bridge._credential_id, bridge._agent_id):
                fails.append(f"C0 第二次启动换了 id：{ids} → {bridge._credential_id, bridge._agent_id}")
            if after != before:
                fails.append(f"C0 重启把 credential/agent 建重了：{before} → {after}")
            await bridge._thread_for("p5-c0")  # noqa: SLF001
            await bridge._thread_for("p5-c0")  # noqa: SLF001
            rows = (await client.get("/sessions/", params={"agent_id": bridge._agent_id})).json()  # noqa: SLF001
            named = [
                s["session"]
                for s in rows.get("sessions", [])
                if s["session"].get("config", {}).get("name") == "agui:p5-c0"
            ]
            if len(named) != 1:
                fails.append(f"C0 同名 session 建了 {len(named)} 条（find-or-create 没找到已有的）")
            else:
                print(f"  ✓ C0 重复启动幂等（{before[0]} credential / {before[1]} agent 不变，session 复用 {named[0]['id'][:8]}）")

            async def agui_run(messages: list[dict], resume: list[dict] | None = None) -> list[dict]:
                body = {
                    "threadId": thread,
                    "runId": f"run-{len(frames)}",
                    "messages": messages,
                    "state": {},
                    "tools": [],
                    "context": [],
                    "forwardedProps": {},
                }
                if resume:
                    body["resume"] = resume
                got: list[dict] = []
                async with client.stream("POST", "/ag-ui", json=body, timeout=DEADLINE) as resp:
                    if resp.status_code >= 400:
                        await resp.aread()
                        raise AssertionError(f"POST /ag-ui 被拒 {resp.status_code}：{resp.text[:300]}")
                    async for line in resp.aiter_lines():
                        if not line.startswith("data:"):
                            continue
                        payload = line[5:].strip()
                        if not payload:
                            continue
                        frame = json.loads(payload)
                        got.append(frame)
                        if frame.get("type") in ("RUN_FINISHED", "RUN_ERROR"):
                            break  # 桥的语义：终点之后这条响应就该关掉
                frames.extend(got)
                return got

            span = await agui_run([{"id": "m1", "role": "user", "content": UTTERANCE}])
            started = next((f for f in span if f.get("type") == "RUN_STARTED"), None)
            if started is None or started.get("threadId") != thread:
                fails.append(f"C1 RUN_STARTED 没把 threadId 换回我们的：{started}")
            err = next((f for f in span if f.get("type") == "RUN_ERROR"), None)
            if err is not None:
                fails.append(f"C1 桥报错：{err.get('code')} {err.get('message')!r}")
                raise AssertionError("C1 失败")
            term = span[-1]
            outcome = term.get("outcome") or {}
            if not outcome.get("interrupts"):
                # 真机实测第一轮常被它当成"变更"来 ask_user，答完第二轮才建单。
                print("  · C1 第一轮没停在待确认（大概是追问了），补第二轮")
                span = await agui_run(
                    [
                        {"id": "m1", "role": "user", "content": UTTERANCE},
                        {"id": "m2", "role": "user", "content": FOLLOWUP},
                    ],
                )
                term = span[-1]
                outcome = term.get("outcome") or {}
            interrupts = outcome.get("interrupts") or []
            if term.get("type") != "RUN_FINISHED" or outcome.get("type") != "interrupt" or not interrupts:
                fails.append(f"C1 park 没翻成 interrupt 终点：{term}")
                raise AssertionError("C1 失败")
            card = interrupts[0]
            if not card.get("id") or not card.get("message") or card.get("reason") != "tool_call":
                fails.append(f"C1 interrupt 缺 id/message/reason：{card}")
            else:
                print(
                    f"  ✓ C1 park → interrupt（{len(interrupts)} 条 "
                    f"{card['metadata']['toolName']}，本轮 {len(span)} 帧）",
                )

            span = await agui_run(
                [
                    {"id": "m1", "role": "user", "content": UTTERANCE},
                    {"id": "m2", "role": "user", "content": FOLLOWUP},
                ],
                resume=[{"interruptId": card["id"], "status": "resolved"}],
            )
            err = next((f for f in span if f.get("type") == "RUN_ERROR"), None)
            if err is not None:
                fails.append(f"C2 表态后桥报错：{err.get('code')} {err.get('message')!r}")
            else:
                print(f"  ✓ C2 resume[] → 同一发 POST 里跑到终点（{len(span)} 帧）")

            called = _tool_names(frames)
            obs["tool_calls"] = called
            for required in ("create_ticket", "assign_ticket"):
                if required not in called:
                    fails.append(f"C3 工具 {required} 没被调用：{called}")
            tickets = [t for ctx in app.state.helpdesk_contexts.values() for t in ctx.store.tickets.values()]
            obs["tickets"] = tickets
            if not tickets:
                fails.append("C3 工单簿是空的：桥这条路没落成单")
            else:
                t = tickets[0]
                print(
                    f"  ✓ C3 落库 #{t.id} {t.category.value}/{t.priority.value} "
                    f"指派={t.assignee_id} 缺={t.missing_info or '无'}",
                )
            print(f"  · C 段工具序列：{called}")
    except AssertionError as exc:
        print(f"  · C 段中断：{exc}")
    finally:
        dump = Path("data/p5_agui_c_events.jsonl")
        dump.parent.mkdir(parents=True, exist_ok=True)
        dump.write_text("".join(json.dumps(f, ensure_ascii=False) + "\n" for f in frames), encoding="utf-8")
        server.should_exit = True
        obs["events"] = len(frames)
        obs["event_types"] = sorted({f.get("type", "?") for f in frames})
    return fails, obs


async def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    offline = "--offline" in argv
    agui = "agui" in argv
    print(
        "P5 前端接缝探针（AG-UI 托管路径）"
        + ("｜只跑 A 段" if offline else "｜C 段：只走 POST /ag-ui" if agui else "｜B 段：框架原生 HITL"),
    )
    obs_live: dict[str, object] = {"events": 0, "event_types": []}
    async with await open_index() as index:
        print("重建索引:", await index.build(recreate=True), flush=True)
        fails_a, obs_a = await phase_a(index)
        for line in fails_a:
            print(f"  ✗ {line}")
        fails = list(fails_a)
        if not offline:
            print("\n真机段（qwen3:8b）")
            fails_live, obs_live = await (phase_c(index) if agui else phase_b(index))
            for line in fails_live:
                print(f"  ✗ {line}")
            fails += fails_live
    print(
        f"\n账面：路由 {obs_a.get('routes')} 条｜workspace 附送工具 {len(obs_a.get('workspace_tools') or [])} 个"
        f"｜AG-UI 事件 {obs_live['events']} 条 / {len(obs_live['event_types'])} 种",
    )
    print(f"退出码 {0 if not fails else 1}")
    return 0 if not fails else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
