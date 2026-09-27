"""托管服务：把同一套工具与中间件接进框架的 FastAPI 服务，对外说 AG-UI。

与 `app.py` 的分工就是这条接缝的证据：那边是我自己组装 Agent（console 路径），
这边是框架按 session 逐轮重组 Agent（托管路径）。业务代码只提供两样东西 ——
按 session_id 分的工单簿、和原来那串中间件。
"""
from __future__ import annotations

import argparse
import asyncio
import os
import sys
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

from .agui_bridge import AguiBridge, make_agui_router
from .knowledge import KNOWLEDGE_SCORE_THRESHOLD, HelpdeskIndex, open_index
from .runtime.agent_factory import CHAT_MODEL, MAX_ITERS, SYSTEM_PROMPT, TOKEN_BUDGET
from .runtime.intake import AskOutLoudMiddleware, IntakeInjectionMiddleware
from .runtime.tool_surface import ToolSurfaceMiddleware
from .runtime.toolcall_ids import UniqueToolCallIds
from .ticket_store import TicketStore
from .tools import HelpdeskContext, build_tools

#: 回环地址不能被系统代理接管。httpx 的代理设置来自 `urllib.request.getproxies()`，
#: macOS 下它读系统配置、却**不认系统自带的那份 bypass 名单**，于是本机一旦开着
#: Clash 一类代理，这个进程里每一个 httpx 客户端 —— 调 ollama:11434 的模型客户端、
#: 调自己 `/credential/` 的桥 —— 都会收到一个空正文的 502（2026-09-27 16:20 真机）。
os.environ.setdefault("NO_PROXY", "127.0.0.1,localhost,::1")
os.environ.setdefault("no_proxy", "127.0.0.1,localhost,::1")

SERVICE_DB = os.environ.get("HELPDESK_SERVICE_DB", "./data/service.db")
WORKSPACE_DIR = os.environ.get("HELPDESK_WORKSPACE_DIR", "./data/workspaces")
#: 服务每个路由都要求 `X-User-ID`；本地单人测试就固定一个身份。
USER_ID = os.environ.get("HELPDESK_USER", "ops")
#: AG-UI 桥订阅的是服务自己的 `/sessions/{id}/stream`。必须是真 HTTP：
#: httpx 的 ASGITransport 会把响应体整块缓冲，SSE 流不动。
SELF_BASE = os.environ.get("HELPDESK_SELF_BASE", "http://127.0.0.1:8000")


def make_service_app(index: HelpdeskIndex, *, self_base: str | None = None) -> FastAPI:
    """装配托管 app。索引共用，工单簿按会话隔离。"""
    contexts: dict[str, HelpdeskContext] = {}

    def ctx_for(session_id: str) -> HelpdeskContext:
        ctx = contexts.get(session_id)
        if ctx is None:
            ctx = HelpdeskContext(index=index, store=TicketStore())
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
        # 知识库/调度/渠道三条子系统的开关：我们的检索走自己的 Milvus 索引，
        # 打开框架的 knowledge 挂接会二次检索（`_chat.py:975`）。
        enable_index_worker=False,
        enable_scheduler=False,
        enable_channel_worker=False,
        extra_agent_tools=tools,
        extra_agent_middlewares=middlewares,
        extra_middlewares=[Middleware(AGUIProtocolMiddleware)],
        title="helpdesk",
    )
    # 探针/测试在同一个进程里读落库事实，不去反推模型的措辞。
    app.state.helpdesk_contexts = contexts
    bridge = AguiBridge(
        base_url=self_base or SELF_BASE,
        user_id=USER_ID,
        system_prompt=SYSTEM_PROMPT,
        model=CHAT_MODEL,
        max_iters=MAX_ITERS,
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
