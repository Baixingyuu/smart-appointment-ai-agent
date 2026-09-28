"""模型可见工具面：托管路径附送的框架工具不能递进模型上下文。

P5 探针 B 段真机实测（同一句话、temp=0）：console 一步到 create_ticket 停在待确认，
托管路径连调 8 次 search_knowledge 烧完 max_iters 后 RUN_ERROR。差别不在模型，
在托管路径每轮附送的 workspace builtin + 计划/团队/调度工具。这里把"只递交业务那一组"
钉成离线可复现的账。
"""
from __future__ import annotations

import asyncio
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.ledger import Ledger  # noqa: E402
from helpdesk.memory import MemoryStore  # noqa: E402
from helpdesk.runtime.specialists import SPECIALIST_TOOLS  # noqa: E402
from helpdesk.runtime.tool_surface import (  # noqa: E402
    MODEL_INVISIBLE_TOOLS,
    MODEL_VISIBLE_TOOLS,
    RETRIEVAL_TOOL,
    ToolSurfaceMiddleware,
)
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402


def _schema(name: str) -> dict:
    return {"type": "function", "function": {"name": name, "parameters": {}}}


def _wired(tmp_path: Path) -> HelpdeskContext:
    """托管路径那份上下文：挂了按用户的日志与偏好表（`service.ctx_for` 的形状）。"""
    return HelpdeskContext(
        index=FakeIndex(),
        store=TicketStore(),
        ledger=Ledger(tmp_path / "ops.ledger.jsonl", user_id="ops", session_id="s1"),
        memory=MemoryStore(tmp_path / "ops.memory.json", user_id="ops"),
    )


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
    visible = [s for s in ours if s["function"]["name"] in MODEL_VISIBLE_TOOLS]
    incoming = [*visible, _schema("search_knowledge")]
    _, seen, mw = asyncio.run(_run(incoming))
    assert [t["function"]["name"] for t in seen["tools"]] == [t["function"]["name"] for t in incoming]
    assert mw.dropped == set()


def test_业务工具现在全都在白名单里() -> None:
    """口径改成「闭环落指派」后，没有"注册但不递交"的业务件了：白名单一份不摘。

    保留这条是为了钉住"别再把 assign_ticket 悄悄摘回 MODEL_INVISIBLE_TOOLS"——
    那会让推荐完又退回"指派由人在系统外做"，而工具面与提示词已经对不上了。
    """
    ours = [_schema(t.name) for t in build_tools(HelpdeskContext(index=FakeIndex(), store=TicketStore()))]
    _, seen, mw = asyncio.run(_run(ours))
    kept = {t["function"]["name"] for t in seen["tools"]}
    assert mw.dropped == set() == MODEL_INVISIBLE_TOOLS
    assert "assign_ticket" in kept, "assign_ticket 必须回到模型面，否则推荐完无法闭环落指派"


def test_白名单与业务工具表不脱钩(tmp_path: Path) -> None:
    """加/改业务工具时必须同时改 MODEL_VISIBLE_TOOLS，否则模型看不见它。

    两份名单必须一致：白名单里多一个没注册的名字不会多出任何事，反过来漏了，
    新工具就变成"注册着却看不见"。`MODEL_INVISIBLE_TOOLS` 现在为空，仍是显式
    登记的占位 —— 哪天要再禁一个动作就往里加名字。
    """
    unwired = {t.name for t in build_tools(HelpdeskContext(index=FakeIndex(), store=TicketStore()))}
    wired = {t.name for t in build_tools(_wired(tmp_path))}
    assert wired - unwired == {"run_autodream"}
    assert set(MODEL_VISIBLE_TOOLS) - {RETRIEVAL_TOOL} - set(SPECIALIST_TOOLS) == wired - MODEL_INVISIBLE_TOOLS
    assert MODEL_INVISIBLE_TOOLS <= unwired, "不递交的工具也必须真的注册着，否则是空头承诺"
    # 专线不是业务工具：它由 make_agent 在开关打开时另外注册，两张表各管一件事
    assert not (set(SPECIALIST_TOOLS) & unwired)


def test_调度醒来的上下文必须看得见run_autodream(tmp_path: Path) -> None:
    """调度任务醒来必须看得见这个工具，而 console/评测的上下文里它无处可写。

    白名单在两种上下文里是同一份 —— 漏了 `run_autodream` 的话表现是"AutoDream 的
    cron 来了、agent 也醒了，但模型没有这个工具可调"，一条没有任何报错的死线。
    """
    assert "run_autodream" in MODEL_VISIBLE_TOOLS
    names = {t.name for t in build_tools(_wired(tmp_path))}
    assert "run_autodream" in names


def test_上游清空工具时不复活() -> None:
    """AskOutLoudMiddleware 会传 tools=[] 逼模型把问题讲出口，这里不能被白名单填回去。"""
    _, seen, mw = asyncio.run(_run([]))
    assert seen == {}
    assert mw.dropped == set()
