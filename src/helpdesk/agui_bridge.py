"""AG-UI 桥：CopilotKit 的一次 POST ⇄ AgentScope 的「触发 + 订阅」两条路。

形状对不上，这是唯一的成因：AG-UI 的客户端（`@ag-ui/client` HttpAgent）发一次
`POST`、`Accept: text/event-stream`，然后在**同一条响应**上读到 `RUN_FINISHED` 为止；
AgentScope 2.0.8 的 `POST /chat/` 是 fire-and-forget（`app/_router/_chat.py` 头部
就写着它不再返回 SSE），事件在另一条长连接 `GET /sessions/{id}/stream` 上。
所以桥做的事就四件：

1. threadId ↔ session_id（客户端的会话号是我们这边一个真 session，名字 `agui:<threadId>`）；
2. 把最后一条 user message 变成 `Msg`，或者把 `resume[]` 变成 `UserConfirmResultEvent`；
3. 长连接订阅该 session 的 SSE，把帧转出去；
4. 把框架只能表达成 `CUSTOM/require_user_confirm` 的待确认，翻译成 AG-UI 1.0 的
   `RUN_FINISHED.outcome = {type: "interrupt", interrupts: [...]}` —— 否则
   CopilotKit 把每一次结束都读成 success，确认卡根本没有 UI。

桥只走公开 HTTP 路由（连 `X-User-ID` 头都自己带），不 import 服务的内部对象：
它对服务端的了解不该超过一个浏览器。代价是它得回环访问自己（SSE 在 ASGI 内部
transport 下会先缓冲完整响应体，流不动）。
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
from dataclasses import dataclass, field
from typing import Any, AsyncGenerator

import httpx
from fastapi import APIRouter, HTTPException, Request
from fastapi.responses import StreamingResponse

logger = logging.getLogger(__name__)

#: 一次 run 最长等多久（模型冷启动 + 若干轮）。超了发 RUN_ERROR，不让前端吊着。
RUN_TIMEOUT = float(os.environ.get("HELPDESK_AGUI_RUN_TIMEOUT", "300"))

#: 触发被接受之后，多久没等到本轮 RUN_STARTED 就收场（别占着前端的转圈不放）。
START_TIMEOUT = float(os.environ.get("HELPDESK_AGUI_START_TIMEOUT", "90"))

#: `RUN_FINISHED` 不等于"这个 session 空了"。真机 409 换来的：框架把终点帧发出去
#: 之后，同一条 run task 还在善后 —— 写消息、写 AgentState，然后 `_auto_name_session`
#: **再调一次模型**给 session 起标题（每个 session 只有一次，所以撞上的一定是首轮）。
#: `ChatRunRegistry.spawn` 见到没结束的 task 就拒同 session 的第二次触发。
#: 对浏览器这是"上一轮刚收尾"的瞬时状态，等它即可，不该报错给前端。
TRIGGER_RETRY_DEADLINE = float(os.environ.get("HELPDESK_AGUI_TRIGGER_RETRY_DEADLINE", "30"))
TRIGGER_RETRY_INTERVAL = float(os.environ.get("HELPDESK_AGUI_TRIGGER_RETRY_INTERVAL", "0.5"))
ACTIVE_RUN_CONFLICT = "already has an active chat run"
SESSION_PREFIX = "agui:"
_CONFIRM_CUSTOM = "require_user_confirm"


def _sse(payload: dict[str, Any]) -> str:
    return f"data: {json.dumps(payload, ensure_ascii=False)}\n\n"


def _row_id(row: dict[str, Any], created_key: str) -> str:
    """POST 的响应叫 `credential_id`/`agent_id`，GET 列表里的同一条记录叫 `id`。"""
    return row.get(created_key) or row["id"]


def _row_name(row: dict[str, Any]) -> str | None:
    """AgentRecord 把 name 放在 `data` 里，CreateAgentResponse 才放在顶层。"""
    return row.get("data", {}).get("name") or row.get("name")


def confirmable_tool_call(call: dict[str, Any]) -> dict[str, Any]:
    """把事件流里那条 tool_call 变成 `POST /chat` 收得下的形状。

    真机 422：`RequireUserConfirmEvent` 出流时走 `model_dump(exclude_none=True)`，
    `suggested_rules` 里那条 `{tool_name, behavior, source}` 因此丢了必填的
    `rule_content`，前端原样递回去就过不了校验。代价是"以后都允许这个工具"
    这类建议规则过不了 AG-UI 这一道，只剩纯 allow/deny。
    """
    return {k: v for k, v in call.items() if k != "suggested_rules"}


@dataclass
class _Thread:
    """一个 AG-UI thread ↔ 一个 AgentScope session ↔ 一条常驻 SSE。"""

    session_id: str
    agent_id: str
    queue: "asyncio.Queue[dict]"
    pump: asyncio.Task
    #: interruptId -> {"reply_id":…, "tool_call":…}；重启即丢，与 InMemoryMessageBus 同寿命。
    pending: dict[str, dict] = field(default_factory=dict)


class AguiBridge:
    """把 AG-UI 的 RunAgentInput 落到 AgentScope 的 HTTP 契约上。"""

    def __init__(
        self,
        *,
        base_url: str,
        user_id: str,
        system_prompt: str,
        model: str,
        max_iters: int,
        name: str = "helpdesk",
    ) -> None:
        self._name = name
        self._system_prompt = system_prompt
        self._model = model
        self._max_iters = max_iters
        self._client = httpx.AsyncClient(
            base_url=base_url,
            headers={"X-User-ID": user_id},
            timeout=30.0,
            # 回环自调绝不能走代理：httpx 默认 trust_env=True 时代理是从
            # `urllib.request.getproxies()` 来的，macOS 下它会读系统代理（本机
            # Clash/mihomo 在 127.0.0.1:7897），而它**不认系统的那份 bypass 名单**
            # —— 于是 GET http://127.0.0.1:<本服务>/credential/ 被代理接走，
            # 回一个空正文的 502，uvicorn 那边连这条请求都没见过。
            trust_env=False,
        )
        self._agent_id: str | None = None
        self._credential_id: str | None = None
        self._threads: dict[str, _Thread] = {}
        self._lock = asyncio.Lock()

    async def _ensure_started(self) -> None:
        async with self._lock:
            if self._agent_id is None:
                await self.start()

    async def start(self) -> None:
        """credential + agent 找一遍没有就建（幂等，重启不堆重复记录）。

        两名制：POST 的响应给 `credential_id`/`agent_id`，GET 列表里的记录叫 `id`，
        而 name/type 这些字段藏在记录的 `data` 里 —— 都按 `data` 优先读。
        """
        rows = (await self._get("/credential/")).json().get("credentials", [])
        own = next(
            (
                r
                for r in rows
                if r.get("data", {}).get("type") == "ollama_credential"
                or r.get("type") == "ollama_credential"
            ),
            None,
        )
        if own is None:
            r = await self._client.post("/credential/", json={"data": {"type": "ollama_credential"}})
            r.raise_for_status()
            own = r.json()
        self._credential_id = _row_id(own, "credential_id")

        rows = (await self._get("/agent/")).json().get("agents", [])
        agent = next((a for a in rows if _row_name(a) == self._name), None)
        if agent is None:
            r = await self._client.post(
                "/agent/",
                json={
                    "name": self._name,
                    "system_prompt": self._system_prompt,
                    "react_config": {"max_iters": self._max_iters},
                    "context_config": {"trigger_ratio": 0.8, "tool_result_limit": 1500},
                },
            )
            r.raise_for_status()
            agent = r.json()
        self._agent_id = _row_id(agent, "agent_id")
        logger.info("AG-UI 桥就绪 credential=%s agent=%s", self._credential_id, self._agent_id)

    async def aclose(self) -> None:
        for thread in self._threads.values():
            thread.pump.cancel()
        await asyncio.gather(*(t.pump for t in self._threads.values()), return_exceptions=True)
        self._threads.clear()
        await self._client.aclose()

    async def _get(self, url: str, params: dict | None = None) -> httpx.Response:
        r = await self._client.get(url, params=params)
        if r.status_code >= 400:
            # 不用 raise_for_status：它不带响应体，而这条是回环自调 ——
            # 真机那个 502 到底是谁答的，只有正文看得出来。
            raise httpx.HTTPStatusError(
                f"{r.request.method} {r.request.url} → {r.status_code}: {r.text[:200]}",
                request=r.request,
                response=r,
            )
        return r

    # --- session 生命周期 ---------------------------------------------------
    async def _thread_for(self, thread_id: str) -> _Thread:
        async with self._lock:
            existing = self._threads.get(thread_id)
            if existing is not None:
                return existing
            assert self._agent_id is not None and self._credential_id is not None
            name = f"{SESSION_PREFIX}{thread_id}"
            rows = (
                await self._get("/sessions/", params={"agent_id": self._agent_id})
            ).json().get("sessions", [])
            # 名字落在 SessionRecord.config.name（POST 顶层的 name 会被收进 config），
            # 读顶层会永远找不到，重启一次就堆一条同名 session。
            hit = next(
                (s["session"] for s in rows if s.get("session", {}).get("config", {}).get("name") == name),
                None,
            )
            if hit is None:
                r = await self._client.post(
                    "/sessions/",
                    json={
                        "agent_id": self._agent_id,
                        "name": name,
                        "chat_model_config": {
                            "type": "ollama_credential",
                            "credential_id": self._credential_id,
                            "model": self._model,
                            "parameters": {"temperature": 0.0, "thinking_enable": False},
                        },
                    },
                )
                r.raise_for_status()
                session_id = r.json()["session_id"]
            else:
                session_id = hit["id"]
            queue: asyncio.Queue[dict] = asyncio.Queue()
            pump = asyncio.create_task(self._pump(session_id, self._agent_id, queue))
            thread = _Thread(session_id, self._agent_id, queue, pump)
            self._threads[thread_id] = thread
            return thread

    async def _pump(self, session_id: str, agent_id: str, queue: "asyncio.Queue[dict]") -> None:
        """常驻订阅：桥与 session 一一对应，所以 replay（按 run 裁剪）里不会有脏帧。"""
        url = f"/sessions/{session_id}/stream"
        try:
            # read=None：这条是常驻 SSE，模型想久一点不代表流断了。用客户端那个
            # 30s 总超时的话，一轮思考超过 30s 就把它读死（真机 16:24:47），
            # 而 run() 自己有 RUN_TIMEOUT 兜底，不需要读超时来替它收场。
            timeout = httpx.Timeout(connect=10.0, read=None, write=30.0, pool=30.0)
            async with self._client.stream(
                "GET",
                url,
                params={"agent_id": agent_id},
                timeout=timeout,
            ) as resp:
                async for line in resp.aiter_lines():
                    if not line.startswith("data:"):
                        continue
                    try:
                        queue.put_nowait(json.loads(line[5:].strip()))
                    except json.JSONDecodeError:
                        continue
        except asyncio.CancelledError:
            raise
        except Exception:  # noqa: BLE001
            logger.exception("SSE 订阅断了（session=%s）", session_id)
        finally:
            await queue.put({"type": "_STREAM_CLOSED"})

    # --- 一次 run -----------------------------------------------------------
    async def run(self, input: dict[str, Any]) -> AsyncGenerator[str, None]:
        """AG-UI 的硬约定：这条响应必须以 RUN_FINISHED 或 RUN_ERROR 收尾。

        真机代价：桥自己在善后/回环请求上抛异常时（一次 loopback 被本机代理拦成
        502），生成器直接断掉，CopilotKit 只能报 "Run ended without emitting a
        terminal event"，用户看到的是"话发出去了但什么都没发生"。
        """
        thread_id = input.get("threadId", "")
        run_id = input.get("runId", "")
        try:
            async for chunk in self._run(input):
                yield chunk
        except Exception:  # noqa: BLE001 - CancelledError/GeneratorExit 是 BaseException，照常传播
            logger.exception("AG-UI run 异常收场 thread=%s", thread_id)
            yield _sse(
                {
                    "type": "RUN_ERROR",
                    "threadId": thread_id,
                    "runId": run_id,
                    "message": "桥这一侧出了异常，本轮没有跑完",
                    "code": "bridge_failed",
                },
            )

    async def _run(self, input: dict[str, Any]) -> AsyncGenerator[str, None]:
        await self._ensure_started()
        thread_id = input["threadId"]
        run_id = input["runId"]
        thread = await self._thread_for(thread_id)
        if thread.pump.done():
            # 订阅曾经断过（网络抖动、服务重启）：重新挂一条，pending 里的待确认留着。
            thread.pump = asyncio.create_task(
                self._pump(thread.session_id, thread.agent_id, thread.queue),
            )

        trigger = self._trigger_body(thread, input)
        if trigger is None:
            yield _sse(
                {
                    "type": "RUN_ERROR",
                    "threadId": thread_id,
                    "runId": run_id,
                    "message": "既没有新消息也没有待确认的 interrupt 可答",
                    "code": "empty_run",
                },
            )
            return
        r = await self._trigger(thread, trigger)
        if r.status_code >= 400:
            yield _sse(
                {
                    "type": "RUN_ERROR",
                    "threadId": thread_id,
                    "runId": run_id,
                    "message": f"POST /chat 被拒 {r.status_code}：{r.text[:300]}",
                    "code": "trigger_rejected",
                },
            )
            return

        loop = asyncio.get_running_loop()
        deadline = loop.time() + RUN_TIMEOUT
        start_deadline = loop.time() + START_TIMEOUT
        started = False
        while True:
            try:
                frame = await asyncio.wait_for(thread.queue.get(), timeout=deadline - loop.time())
            except (TimeoutError, asyncio.TimeoutError):
                yield _sse(
                    {
                        "type": "RUN_ERROR",
                        "threadId": thread_id,
                        "runId": run_id,
                        "message": f"{RUN_TIMEOUT:.0f}s 内没有等到本轮结束",
                        "code": "run_timeout",
                    },
                )
                return
            ftype = frame.get("type")
            if ftype == "_STREAM_CLOSED":
                yield _sse(
                    {
                        "type": "RUN_ERROR",
                        "threadId": thread_id,
                        "runId": run_id,
                        "message": "会话事件流已断开",
                        "code": "stream_closed",
                    },
                )
                return
            if not started:
                # AG-UI 的硬约定：本轮响应的第一帧必须是 RUN_STARTED ——
                # `@ag-ui/client` 的 verifyEvents 会对第一帧就抛
                # "First event must be 'RUN_STARTED'"（真机 16:37 的 INCOMPLETE_STREAM）。
                # 常驻订阅里上一轮的善后帧（session 改名通知等）随时会掉进来，一律丢掉。
                if ftype == "RUN_STARTED":
                    started = True
                    frame["threadId"] = thread_id
                    frame["runId"] = run_id
                    yield _sse(frame)
                    continue
                if loop.time() > start_deadline:
                    yield _sse(
                        {
                            "type": "RUN_ERROR",
                            "threadId": thread_id,
                            "runId": run_id,
                            "message": f"{START_TIMEOUT:.0f}s 内没等到本轮的 RUN_STARTED",
                            "code": "missing_run_started",
                        },
                    )
                    return
                logger.debug("丢掉本轮开始前的残留帧 type=%s", ftype)
                continue
            if ftype == "CUSTOM" and frame.get("name") == _CONFIRM_CUSTOM:
                self._park(thread, frame["value"])
                # 真机实测：park 的时候框架**不发** RUN_FINISHED，这条流就此沉默
                # （P5 探针 B 段的末帧就是这条 CUSTOM）。AG-UI 的 HTTP 响应必须有
                # 终点，所以中断语义由桥来落地，而不是等上游。
                yield _sse(
                    {
                        "type": "RUN_FINISHED",
                        "threadId": thread_id,
                        "runId": run_id,
                        "outcome": {"type": "interrupt", "interrupts": _interrupts(thread.pending)},
                    },
                )
                return
            if ftype in ("RUN_STARTED", "RUN_FINISHED", "RUN_ERROR"):
                frame["threadId"] = thread_id
                frame["runId"] = run_id
            if ftype in ("RUN_FINISHED", "RUN_ERROR"):
                yield _sse(frame)
                return
            yield _sse(frame)

    async def _trigger(self, thread: _Thread, body: dict) -> httpx.Response:
        """POST /chat；上一轮还在善后时等它，别把瞬时冲突报给前端。

        每次重试前重新清一次队列：善后期间还会往 session 流里掉帧，
        不清就会跟着本轮转发出去（`started` 只挡得住终点帧）。
        """
        loop = asyncio.get_running_loop()
        deadline = loop.time() + TRIGGER_RETRY_DEADLINE
        while True:
            while not thread.queue.empty():
                thread.queue.get_nowait()
            r = await self._client.post("/chat/", json=body)
            if r.status_code != 409 or ACTIVE_RUN_CONFLICT not in r.text:
                return r
            if loop.time() >= deadline:
                return r
            await asyncio.sleep(TRIGGER_RETRY_INTERVAL)

    def _trigger_body(self, thread: _Thread, input: dict[str, Any]) -> dict | None:
        """本轮要 POST /chat 的 body；None 表示既没新话也没得可表态。"""
        entries = input.get("resume") or []
        if entries:
            results = []
            reply_id = None
            for entry in entries:
                parked = thread.pending.pop(entry.get("interruptId", ""), None)
                if parked is None:
                    continue
                reply_id = parked["reply_id"]
                results.append(
                    {
                        "confirmed": entry.get("status") != "cancelled",
                        "tool_call": confirmable_tool_call(parked["tool_call"]),
                        "rules": None,
                    },
                )
            if not results or reply_id is None:
                return None
            return {
                "agent_id": thread.agent_id,
                "session_id": thread.session_id,
                "input": {
                    # 大小写是真机 422 换来的：`input` 是 `Msg | … | UserConfirmResultEvent`
                    # 的联合，判别字段取的是 `EventType.USER_CONFIRM_RESULT` 的值
                    # （"USER_CONFIRM_RESULT"，全大写）。小写的 "user_confirm_result"
                    # 只是它在 AG-UI 侧的 **CUSTOM 事件名**，两者不是一回事。
                    "type": "USER_CONFIRM_RESULT",
                    "reply_id": reply_id,
                    "confirm_results": results,
                },
            }
        text = next(
            (
                m.get("content")
                for m in reversed(input.get("messages") or [])
                if m.get("role") == "user" and isinstance(m.get("content"), str)
            ),
            None,
        )
        if not text:
            return None
        return {
            "agent_id": thread.agent_id,
            "session_id": thread.session_id,
            # Msg.content 只收内容块数组，给裸字符串会 422。
            "input": {
                "name": "用户",
                "role": "user",
                "content": [{"type": "text", "text": text}],
            },
        }

    @staticmethod
    def _park(thread: _Thread, value: dict[str, Any]) -> None:
        for call in value.get("tool_calls", []):
            thread.pending[f"{value['reply_id']}:{call['id']}"] = {
                "reply_id": value["reply_id"],
                "tool_call": call,
            }


def _interrupts(pending: dict[str, dict]) -> list[dict]:
    out = []
    for interrupt_id, parked in pending.items():
        call = parked["tool_call"]
        out.append(
            {
                "id": interrupt_id,
                "reason": "tool_call",
                "message": f"是否执行 {call['name']}？",
                "toolCallId": call["id"],
                "metadata": {"toolName": call["name"], "args": call.get("input")},
            },
        )
    return out


def make_agui_router(bridge: AguiBridge) -> APIRouter:
    """挂 `/ag-ui`：AG-UI 的标准 HTTP 形状（一次 POST 回一条 SSE）。"""
    router = APIRouter(tags=["ag-ui"])

    @router.post("/ag-ui")
    async def ag_ui(request: Request) -> StreamingResponse:
        try:
            input = await request.json()
        except json.JSONDecodeError as exc:
            raise HTTPException(status_code=400, detail=f"不是 JSON：{exc}") from exc
        missing = {"threadId", "runId"} - set(input)
        if missing:
            raise HTTPException(status_code=422, detail=f"RunAgentInput 缺字段：{sorted(missing)}")
        return StreamingResponse(bridge.run(input), media_type="text/event-stream")

    return router
