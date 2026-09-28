"""专线开/关的成本对照：同一句话，两臂各跑一次。

拆编排的收益没有先验。P6-a③ 把"查资料 / 排期 / 派单参谋"从主管上下文里搬进三条专线，
换来两笔代价：每次模型调用的工具 schema 多出三份，以及专线内部自己那几轮模型钱。
所以这里量的不是"结构更清晰"，是轮次与 token 的差 —— 与 P4 那次一样，
先带量具去判断，再决定要不要改变默认行为。

三段，成本不同：
  A 离线（不调模型）：挂载差（多出哪三个工具、schema 多少字）、工具面闸、
    以及"外层事件流看不见嵌套轮次"这条账 —— 用脚本模型钉死，B 段才知道该读哪里。
  B 真机（本地 qwen3:8b，temp=0）：三句话各跑关/开两臂，一句一轮，外层与嵌套并排列出。
    句子选的是"专线本来就该有活干"的那三类（受理 / 查知识 / 约上门）—— 如果开臂一次都没
    走到专线，那本身就是结论。每臂只有 1 个样本，temp=0 不保证可复现（P1 记过这条），
    所以差值是观测值，不是结论；要结论就重复跑。

跑法：
    .venv/bin/python probes/p6_specialists_cost.py --offline   # 只跑 A，不花钱
    .venv/bin/python probes/p6_specialists_cost.py             # A + B
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
logging.getLogger("pymilvus").setLevel(logging.CRITICAL)

from agentscope.console import ConsoleRenderer  # noqa: E402
from agentscope.event import (  # noqa: E402
    ModelCallEndEvent,
    ToolCallStartEvent,
    ToolResultEndEvent,
)

from agentscope.message import UserMsg  # noqa: E402

from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.eval.scripted_model import ScriptedChatModel, Turn  # noqa: E402
from helpdesk.knowledge import DB_PATH, open_index  # noqa: E402
from helpdesk.runtime.agent_factory import CHAT_MODEL, MAX_ITERS, make_agent  # noqa: E402
from helpdesk.runtime.confirm_bridge import deliver  # noqa: E402
from helpdesk.runtime.specialists import (  # noqa: E402
    PLAN_VISIT_TOOL,
    SPECIALIST_TOOLS,
    make_specialist_tools,
)
from helpdesk.runtime.tool_surface import MODEL_VISIBLE_TOOLS  # noqa: E402

#: 独立库：Milvus Lite 一个文件只让一个进程握着（P5 那条 SIGTERM 落不下来的锁），
#: 而本机大概率正跑着 `make serve`。从现库复制一份，谁也不抢谁。
PROBE_DB = os.environ.get("HELPDESK_PROBE_DB", "./data/p6_specialists.db")
#: 三句话各跑两臂。intake 是 P3 的 short；consult 明确要求查知识库（`consult_ticket` 该接的活）；
#: visit 是给 `plan_visit` 的最后一次机会 —— 这条路上"直接调 propose_appointments"和
#: "交给专线"是两个都走得通的选项，模型怎么选就是拆编排的全部意义。
#: 整个 B 段可以反复跑：每跑一次给每个格子多一个样本（temp=0 不保证同一次结果）。
SCENARIOS = {
    "intake": "下单接口一直在报 401，token 过期了，帮忙看下",
    "consult": "帮我查一下知识库：私有化部署的最低硬件配置是多少？查了再回答，不要建单",
    "visit": "帮我约个上门时间，下周工作日上午都行，看看谁能来",
}
SENTENCE = SCENARIOS["intake"]
_PROPOSE = {
    "need": "现场排查下单接口 401",
    "earliest": "2026-10-12 09:00",
    "latest": "2026-10-12 18:00",
    "duration_minutes": 60,
}


def nested_cost(agent: object) -> dict[str, int]:
    """把专线那几次工具调用**内部**的真实开销加回来。

    外层事件流只看得见"一次工具调用"，而一次子调用里面可能烧了好几轮模型 ——
    `ModelCallEndEvent` 是子 Agent 自己的流发出来的，不会冒到外层。
    所以 P6-a③ 在 `_run` 里把 `ModelCallEndEvent` 聚进 ToolChunk.metadata，
    框架会把它原样带进上下文里的 tool_result 块（P4 探针读过同一条路径）。
    """
    total = {"model_calls": 0, "input_tokens": 0, "output_tokens": 0, "specialist_calls": 0}
    for msg in agent.state.context:  # type: ignore[attr-defined]
        for block in msg.get_content_blocks("tool_result"):
            if block.name not in SPECIALIST_TOOLS:
                continue
            meta = block.metadata or {}
            total["specialist_calls"] += 1
            for key in ("model_calls", "input_tokens", "output_tokens"):
                total[key] += int(meta.get(key, 0))
    return total


async def phase_a() -> list[str]:
    """离线：挂载差与嵌套账。"""
    fails: list[str] = []
    model_off = ScriptedChatModel([Turn(text="收尾")])
    off, _ = await make_agent(FakeIndex(), model=model_off)
    on, ctx = await make_agent(FakeIndex(), model=ScriptedChatModel([Turn(text="收尾")]), specialists=True)
    names_off = {s["function"]["name"] for s in await off.toolkit.get_tool_schemas()}
    schemas_on = await on.toolkit.get_tool_schemas()
    names_on = {s["function"]["name"] for s in schemas_on}

    added = names_on - names_off
    if added != set(SPECIALIST_TOOLS):
        fails.append(f"A1 开关多出来的不是那三条专线：{sorted(added)}")
    payload = sum(
        len(json.dumps(s, ensure_ascii=False)) for s in schemas_on if s["function"]["name"] in added
    )
    print(f"  A1 挂载差：+{len(added)} 工具，schema 共 {payload} 字（每次模型调用都带上）")
    print(f"     {sorted(added)}")

    invisible = names_on - set(MODEL_VISIBLE_TOOLS)
    if invisible:
        fails.append(f"A2 有工具进了 Toolkit 却进不了模型（白名单漏了）：{sorted(invisible)}")
    print(f"  A2 工具面：注册 {len(names_on)} 条，白名单 {len(MODEL_VISIBLE_TOOLS)} 条，差集 {sorted(invisible)}")

    tools = await make_specialist_tools(ctx, model=ScriptedChatModel(
        [
            Turn(tool_calls=[("propose_appointments", _PROPOSE)], input_tokens=300, output_tokens=40),
            Turn(text="建议 10-12 09:00 张伟。", input_tokens=500, output_tokens=30),
        ]
    ))
    class _State:
        context = [UserMsg(name="user", content=SENTENCE)]
        session_id = "probe-a"

    plan_visit = next(t for t in tools if t.name == PLAN_VISIT_TOOL)
    meta = (await plan_visit.call(task="给下单接口 401 约一个上门时间", _agent_state=_State())).metadata
    if meta["model_calls"] != 2 or meta["total_tokens"] != 870:
        fails.append(f"A3 专线自己的账不对：{meta}")
    print(
        f"  A3 嵌套账：外层 1 次工具调用 = 内层 {meta['model_calls']} 次模型、"
        f"in {meta['input_tokens']} / out {meta['output_tokens']}（这些数字不在外层事件流里）"
    )
    return fails


async def _arm(index: object, *, specialists: bool, scenario: str, sentence: str) -> dict:
    renderer = ConsoleRenderer(verbosity="quiet")
    label = "开" if specialists else "关"
    agent, ctx = await make_agent(index, specialists=specialists)  # type: ignore[arg-type]
    stats = {"calls": 0, "in": 0, "out": 0}
    name_of: dict[str, str] = {}
    seen: list[str] = []

    def watch(event: object) -> None:
        renderer.render(event)
        if isinstance(event, ToolCallStartEvent):
            name_of[event.tool_call_id] = event.tool_call_name
        elif isinstance(event, ToolResultEndEvent):
            seen.append(f"{name_of.get(event.tool_call_id, '?')}={event.state}")
        elif isinstance(event, ModelCallEndEvent):
            stats["calls"] += 1
            stats["in"] += event.input_tokens
            stats["out"] += event.output_tokens

    print(f"\n=== B｜{scenario}｜专线{label}：{sentence}")
    out = await deliver(agent, sentence, on_event=watch)  # type: ignore[arg-type]
    nested = nested_cost(agent)
    parked = [t.name for t in out.pending.tool_calls] if out.pending else []
    print(f"  工具序列：{seen}")
    print(f"  外层：模型 {stats['calls']} 次｜in {stats['in']}｜out {stats['out']}｜cur_iter={agent.state.cur_iter}")  # type: ignore[attr-defined]
    print(
        f"  嵌套：专线 {nested['specialist_calls']} 次调用 = {nested['model_calls']} 轮模型、"
        f"in {nested['input_tokens']}｜out {nested['output_tokens']}"
    )
    print(
        f"  合计：模型 {stats['calls'] + nested['model_calls']} 次｜"
        f"token {stats['in'] + stats['out'] + nested['input_tokens'] + nested['output_tokens']}"
        f"｜park={parked}｜工单 {len(ctx.store.tickets)} 张"  # type: ignore[attr-defined]
    )
    return {
        "scenario": scenario,
        "label": label,
        "outer_calls": stats["calls"],
        "outer_tokens": stats["in"] + stats["out"],
        "nested_calls": nested["model_calls"],
        "nested_tokens": nested["input_tokens"] + nested["output_tokens"],
        "specialist_calls": nested["specialist_calls"],
        "tools": seen,
        "parked": parked,
        "iters": agent.state.cur_iter,  # type: ignore[attr-defined]
    }


def _prepare_db() -> None:
    if Path(PROBE_DB).exists():
        return
    shutil.copytree(DB_PATH, PROBE_DB)
    print(f"从 {DB_PATH} 复制探针库副本（不抢它的文件锁）：{PROBE_DB}")


def _compare(off: dict, on: dict, fails: list[str]) -> None:
    calls = (off["outer_calls"] + off["nested_calls"], on["outer_calls"] + on["nested_calls"])
    tokens = (off["outer_tokens"] + off["nested_tokens"], on["outer_tokens"] + on["nested_tokens"])
    print(
        f"\n=== 对照｜{off['scenario']}（各 1 个样本，temp=0 仍不可复现 —— 差值是观测值，不是结论）\n"
        f"  模型轮次：关 {calls[0]}｜开 {calls[1]}｜Δ {calls[1] - calls[0]:+d}\n"
        f"  token：  关 {tokens[0]}｜开 {tokens[1]}｜Δ {tokens[1] - tokens[0]:+d}\n"
        f"  其中嵌套：关 {off['nested_tokens']}｜开 {on['nested_tokens']}\n"
        f"  只看外层事件会报：关 {off['outer_tokens']}｜开 {on['outer_tokens']}"
        f"（少了 {on['nested_tokens']}，那就是专线自己烧掉的那份）"
    )
    for arm in (off, on):
        leaked = [t for t in arm["tools"] if t.split("=")[0] not in MODEL_VISIBLE_TOOLS]
        if leaked:
            print(f"  ✗ B1 {arm['label']}臂出现了白名单外的工具：{leaked}")
            fails.append(f"B1/{arm['scenario']}")
    if on["specialist_calls"] == 0:
        print("  · B2 开臂这一轮没调用任何专线（工具面挂了不等于模型会用）—— 差值里只有 schema 变宽那部分")
    if off["parked"] != on["parked"] or off["tools"] != on["tools"]:
        print(f"  · B3 两臂没走在同一条路上：关 {off['tools']}｜开 {on['tools']} —— 这一轮的差值不可比")
    else:
        print(f"  · B3 两臂停在同一处（{on['tools']}，park={on['parked']}）")


async def main(offline: bool) -> int:
    print("--- A：离线挂载与嵌套账（不调模型）")
    fails = await phase_a()
    for f in fails:
        print(f"  ✗ {f}")
    print("  A 结论：" + ("全部通过" if not fails else f"{len(fails)} 条不通过"))
    if offline:
        return 0 if not fails else 1

    _prepare_db()
    async with await open_index(db_path=PROBE_DB) as index:
        await index.ensure_ready()
        print(f"\n--- B：{len(SCENARIOS)} 句话 × 两臂（{CHAT_MODEL} temp=0 max_iters={MAX_ITERS}）")
        for scenario, sentence in SCENARIOS.items():
            off = await _arm(index, specialists=False, scenario=scenario, sentence=sentence)
            on = await _arm(index, specialists=True, scenario=scenario, sentence=sentence)
            _compare(off, on, fails)
    print(f"退出码 {0 if not fails else 1}")
    return 0 if not fails else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main("--offline" in sys.argv)))
