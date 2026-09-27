"""模型可见工具面：托管路径附送的框架工具不能递进模型上下文。

P5 探针 B 段真机实测（同一句话、temp=0）：console 一步到 create_ticket 停在待确认，
托管路径连调 8 次 search_knowledge 烧完 max_iters 后 RUN_ERROR。差别不在模型，
在托管路径每轮附送的 workspace builtin + 计划/团队/调度工具。这里把"只递交六件套"
钉成离线可复现的账。
"""
from __future__ import annotations

import asyncio
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.runtime.tool_surface import (  # noqa: E402
    MODEL_VISIBLE_TOOLS,
    ToolSurfaceMiddleware,
)
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402


def _schema(name: str) -> dict:
    return {"type": "function", "function": {"name": name, "parameters": {}}}


async def _run(tools: list[dict], *, allowed=None):
    seen: dict = {}

    async def next_handler(**kwargs):
        seen.update(kwargs)
        return "ok"

    mw = ToolSurfaceMiddleware() if allowed is None else ToolSurfaceMiddleware(allowed)
    out = await mw.on_model_call(object(), {"tools": tools}, next_handler)
    return out, seen, mw


def test_托管附送的框架工具被摘掉() -> None:
    hosted = [_schema(n) for n in (*MODEL_VISIBLE_TOOLS, "Bash", "Write", "TaskCreate", "TeamSay")]
    out, seen, mw = asyncio.run(_run(hosted))
    assert out == "ok"
    assert [t["function"]["name"] for t in seen["tools"]] == list(MODEL_VISIBLE_TOOLS)
    assert mw.dropped == {"Bash", "Write", "TaskCreate", "TeamSay"}


def test_console路径本来就是白名单_一个不摘() -> None:
    ours = [_schema(t.name) for t in build_tools(HelpdeskContext(index=FakeIndex(), store=TicketStore()))]
    _, seen, mw = asyncio.run(_run([*ours, _schema("search_knowledge")]))
    assert len(seen["tools"]) == len(MODEL_VISIBLE_TOOLS)
    assert mw.dropped == set()


def test_白名单与业务工具表不脱钩() -> None:
    """加/改业务工具时必须同时改 MODEL_VISIBLE_TOOLS，否则模型看不见它。"""
    names = {t.name for t in build_tools(HelpdeskContext(index=FakeIndex(), store=TicketStore()))}
    assert set(MODEL_VISIBLE_TOOLS) - {"search_knowledge"} == names


def test_上游清空工具时不复活() -> None:
    """AskOutLoudMiddleware 会传 tools=[] 逼模型把问题讲出口，这里不能被白名单填回去。"""
    _, seen, mw = asyncio.run(_run([]))
    assert seen == {}
    assert mw.dropped == set()
