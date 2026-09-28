"""托管服务：把同一套工具与中间件接进框架的 FastAPI 服务，对外说 AG-UI。

与 `app.py` 的分工就是这条接缝的证据：那边是我自己组装 Agent（console 路径），
这边是框架按 session 逐轮重组 Agent（托管路径）。业务代码只提供两样东西 ——
按 session_id 分的工单簿、和原来那串中间件。
"""
from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys
from pathlib import Path
from typing import Any

import uvicorn
from agentscope.app import create_app
from agentscope.app.message_bus import InMemoryMessageBus
from agentscope.app.middleware import AGUIProtocolMiddleware
from agentscope.app.storage import AsyncSQLAlchemyStorage
from agentscope.app.workspace_manager import LocalWorkspaceManager
from agentscope.middleware import RAGMiddleware, ReplyBudgetControlMiddleware
from fastapi import FastAPI
from fastapi.middleware import Middleware
from fastapi.middleware.cors import CORSMiddleware

from .agui_bridge import AguiBridge, make_agui_router
from .appointment import AppointmentStore
from .autodream import ensure_autodream_schedule
from .knowledge import KNOWLEDGE_SCORE_THRESHOLD, HelpdeskIndex, open_index
from .ledger import Ledger, appointment_sink, ticket_sink
from .memory import MemoryStore
from .runtime.agent_factory import (
    CHAT_MODEL,
    CONTEXT_CONFIG,
    MAX_ITERS,
    SYSTEM_PROMPT,
    TIMEZONE,
    TOKEN_BUDGET,
    LocalClockAgent,
)
from .runtime.intake import AskOutLoudMiddleware, IntakeInjectionMiddleware
from .runtime.intent_router import IntentRouterMiddleware
from .runtime.tool_surface import ToolSurfaceMiddleware
from .runtime.toolcall_ids import UniqueToolCallIds
from .ticket_store import TicketStore
from .tools import HelpdeskContext, build_tools

logger = logging.getLogger(__name__)

#: 回环不能被系统代理接管这件事由 `helpdesk/__init__.py` 统一兜（桥与模型/嵌入客户端都在内）。

SERVICE_DB = os.environ.get("HELPDESK_SERVICE_DB", "./data/service.db")
WORKSPACE_DIR = os.environ.get("HELPDESK_WORKSPACE_DIR", "./data/workspaces")
#: 跨会话的两件套：原始事实（JSONL 日志）+ 折出来的结论（偏好表）。按用户一份，
#: 与会话同生的工单簿/预约簿分开 —— 那两簿跟着会话没，AutoDream 读不到。
MEMORY_DIR = os.environ.get("HELPDESK_MEMORY_DIR", "./data/memory")
#: 服务每个路由都要求 `X-User-ID`；本地单人测试就固定一个身份。
USER_ID = os.environ.get("HELPDESK_USER", "ops")
#: AG-UI 桥订阅的是服务自己的 `/sessions/{id}/stream`。必须是真 HTTP：
#: httpx 的 ASGITransport 会把响应体整块缓冲，SSE 流不动。
SELF_BASE = os.environ.get("HELPDESK_SELF_BASE", "http://127.0.0.1:8000")


def memory_paths(user_id: str) -> tuple[Path, Path]:
    root = Path(MEMORY_DIR)
    return root / f"{user_id}.ledger.jsonl", root / f"{user_id}.memory.json"


def make_service_app(index: HelpdeskIndex, *, self_base: str | None = None) -> FastAPI:
    """装配托管 app。索引共用，工单簿按会话隔离。"""
    contexts: dict[str, HelpdeskContext] = {}
    ledger_path, memory_path = memory_paths(USER_ID)
    ledger = Ledger(ledger_path, user_id=USER_ID)
    memory = MemoryStore(memory_path, user_id=USER_ID)

    def ctx_for(session_id: str) -> HelpdeskContext:
        ctx = contexts.get(session_id)
        if ctx is None:
            # 一份日志，一条会话的视角：闸门数的是"新会话满几次"，所以每一行都得
            # 带着它是哪次会话写的（`Ledger.with_session`）。
            session_ledger = ledger.with_session(session_id)
            ctx = HelpdeskContext(
                index=index,
                store=TicketStore(sink=ticket_sink(session_ledger)),
                appointments=AppointmentStore(sink=appointment_sink(session_ledger)),
                ledger=session_ledger,
                memory=memory,
            )
            contexts[session_id] = ctx
        return ctx

    async def tools(user_id: str, agent_id: str, session_id: str) -> list[Any]:
        return build_tools(ctx_for(session_id))

    async def middlewares(
        user_id: str,
        agent_id: str,
        session_id: str,
    ) -> list[Any]:
        # 托管路径会自己 `await mw.list_tools()`（`app/_service/_toolkit.py:233`），
        # 所以 `search_knowledge` 不必像 console 那样手工并进 Toolkit。
        rag = RAGMiddleware(
            [index.knowledge],
            RAGMiddleware.Parameters(
                mode="agentic",
                top_k=5,
                score_threshold=KNOWLEDGE_SCORE_THRESHOLD,
            ),
        )
        return [
            ToolSurfaceMiddleware(),
            IntentRouterMiddleware(ctx_for(session_id).store),
            rag,
            IntakeInjectionMiddleware(),
            AskOutLoudMiddleware(),
            ReplyBudgetControlMiddleware(token_budget=TOKEN_BUDGET),
            UniqueToolCallIds(),
        ]

    app = create_app(
        storage=AsyncSQLAlchemyStorage(
            f"sqlite+aiosqlite:///{SERVICE_DB}",
            create_tables=True,
        ),
        message_bus=InMemoryMessageBus(),
        workspace_manager=LocalWorkspaceManager(WORKSPACE_DIR),
        # 知识库/调度/渠道三条子系统各自一个开关。前一个关：我们的检索走自己的
        # Milvus 索引，打开框架的 knowledge 挂接会二次检索（`_chat.py:975`）。
        enable_index_worker=False,
        # 调度器是 AutoDream 的定时器：框架给了 timer 归属（多节点只有一台持有）、
        # misfire 宽限、以及"这一轮跑在哪个 session、事件流看得见"。
        # 白送的 ScheduleCreate/ScheduleList 那些工具不在工具面白名单里 —— 模型给自己
        # 加 cron 等于把"什么时候能醒"交给提示词（`autodream.ensure_autodream_schedule`）。
        enable_scheduler=True,
        enable_channel_worker=False,
        extra_agent_tools=tools,
        extra_agent_middlewares=middlewares,
        extra_middlewares=[Middleware(AGUIProtocolMiddleware)],
        # 托管路径不传 injection_config，只能靠子类把时区带回本地（见 LocalClockAgent）。
        custom_agent_cls=LocalClockAgent,
        title="helpdesk",
    )
    # 前端（assistant-ui demo 等）在浏览器直连 AG-UI 端点时跨域，这里放开。
    # 本机单人测试用 *，生产要收紧到具体来源。
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )
    # 探针/测试在同一个进程里读落库事实，不去反推模型的措辞。
    app.state.helpdesk_contexts = contexts
    app.state.helpdesk_ledger = ledger
    app.state.helpdesk_memory = memory

    async def register_autodream(bridge: AguiBridge) -> None:
        """把 AutoDream 的 cron 落到框架的调度存储（桥就绪时一次，幂等）。

        失败只记账、不往上抛：注册挂了影响的是"后台整理会不会自己来"，
        而用户的对话不该为一件它没要求过的事挂掉。下一轮启动会重试。
        """
        try:
            schedule_id = await ensure_autodream_schedule(
                storage=app.state.storage,
                scheduler=app.state.scheduler_manager,
                user_id=USER_ID,
                agent_id=bridge.agent_id,
                chat_model_config=bridge.chat_model_config,
                timezone=TIMEZONE,
            )
            logger.info(
                "AutoDream 调度：%s",
                f"已登记/刷成这一版的样子 {schedule_id}"
                if schedule_id
                else "库里那份与这一版一致，未改动",
            )
        except Exception:  # noqa: BLE001
            logger.exception("AutoDream 的 cron 没登记上（对话不受影响）")

    bridge = AguiBridge(
        base_url=self_base or SELF_BASE,
        user_id=USER_ID,
        system_prompt=SYSTEM_PROMPT,
        model=CHAT_MODEL,
        max_iters=MAX_ITERS,
        context_config=CONTEXT_CONFIG,
        on_ready=register_autodream,
    )
    app.include_router(make_agui_router(bridge))
    app.state.agui_bridge = bridge
    app.router.on_shutdown.append(bridge.aclose)
    return app


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="helpdesk-service", description="AG-UI 托管服务")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--keep-index", action="store_true", help="不重建向量索引")
    args = parser.parse_args(argv)

    async def run() -> int:
        async with await open_index() as index:
            if not args.keep_index:
                print("重建索引:", await index.build(recreate=True), flush=True)
            host = args.host.replace("0.0.0.0", "127.0.0.1")
            app = make_service_app(index, self_base=f"http://{host}:{args.port}")
            server = uvicorn.Server(
                uvicorn.Config(
                    app,
                    host=args.host,
                    port=args.port,
                    log_level="info",
                    # 桥订阅的是它自己（`GET /sessions/{id}/stream`，read 不设超时），
                    # 而 uvicorn 要等所有连接关掉才走 on_shutdown —— 那里才是取消订阅
                    # 的地方。不封顶就是 SIGTERM 永远落不下来（真机：25s 没退，
                    # 向量库的锁一直被握着，重启只能 kill -9）。
                    timeout_graceful_shutdown=5,
                ),
            )
            await server.serve()
            return 0

    return asyncio.run(run())


if __name__ == "__main__":
    sys.exit(main())
