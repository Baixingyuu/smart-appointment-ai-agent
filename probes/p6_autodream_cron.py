"""AutoDream 的 cron 真机验证：把心跳临时调到每分钟，看它到底醒不醒、醒了干什么。

P6-a 的账在离线侧已经钉死了（`tests/test_autodream.py` 32 条：闸、幂等、检查点、任务锁、
DONT_ASK 下的 DENY）。这一支补的是唯一没在机器上验过的一环：**到点真的醒一次、并且把
`run_autodream` 调起来**。默认 cron 是凌晨三点（`AUTODREAM_CRON`），所以做法只有一个：
把周期换成 `* * * * *` 再走一遍真实路径 —— 换掉的只有周期。登记走 `service.register_autodream`
（桥 first-ready 那条），触发走框架的 `SchedulerManager._trigger`，执行走
`WakeupDispatcher → ChatService.run`，一处都不替。

必须跑在一次性世界里：进程内起 uvicorn，服务库/工作区/记忆目录全指进 `mkdtemp`，向量库
从现库复制一份（Milvus Lite 一个文件只让一个进程握着，而本机大概率正跑着 `make serve`）。

五条判据：
  C1 登记：桥 first-ready 之后调度存储里恰好有一条 `autodream`，且它带着 DONT_ASK 与非 stateful。
  C2 落地：到点之后 `list_sessions_by_schedule` 里出现新 session（每次触发一个新会话）。
  C3 执行：那一轮的消息里真的出现 `run_autodream` 的 tool_call 与 tool_result，正文是
     `DreamResult.text()`（"AutoDream：折叠 N 行事实…"）。"模型的活儿只是把它调一次"的证据。
  C4 闸：第二轮心跳照样醒，但**不重折** —— 计数表 `runs` 仍是 1，那一轮的正文写着为什么不跑。
     "cron 是心跳、真正的条件是那两道闸"就靠这一条从口号变成观测。
  C5 不写：无人值守的那几轮没往日志里添任何一行。权限层的 DENY 已在单测钉过，这里是全链路
     （调度 session 真的带上了 `permission_mode`）再看一次。

跑法：
    .venv/bin/python probes/p6_autodream_cron.py          # 等两次分钟边界，约 2–3 分钟
    .venv/bin/python probes/p6_autodream_cron.py --once   # 只验第一次触发就收
"""
from __future__ import annotations

import asyncio
import logging
import os
import shutil
import socket
import sys
import tempfile
import time
from datetime import datetime, timedelta
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(_ROOT / "src"))
logging.getLogger("pymilvus").setLevel(logging.CRITICAL)

# ---------------------------------------------------------------------------
# 环境变量必须在 import helpdesk 之前落位：service / autodream / knowledge 三个模块
# 都在模块顶层读一次 os.environ（`AUTODREAM_CRON` 尤甚），晚设等于没设。
# ---------------------------------------------------------------------------
_WORLD = Path(tempfile.mkdtemp(prefix="helpdesk_p6_cron_"))
_SRC_VECTOR = _ROOT / "data" / "milvus_lite.db"
os.environ["HELPDESK_VECTOR_DB"] = str(_WORLD / "vector.db")
os.environ["HELPDESK_SERVICE_DB"] = str(_WORLD / "service.db")
os.environ["HELPDESK_WORKSPACE_DIR"] = str(_WORLD / "workspaces")
os.environ["HELPDESK_MEMORY_DIR"] = str(_WORLD / "memory")
#: 唯一改产的默认值：心跳从"每天一次"改成"每分钟一次"，否则这条证据要等到凌晨三点。
os.environ["HELPDESK_AUTODREAM_CRON"] = "* * * * *"

import uvicorn  # noqa: E402

from helpdesk.autodream import AUTODREAM_CRON, AUTODREAM_NAME  # noqa: E402
from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.knowledge import open_index  # noqa: E402
from helpdesk.ledger import Ledger  # noqa: E402
from helpdesk.memory import MemoryStore  # noqa: E402
from helpdesk.service import USER_ID, make_service_app, memory_paths  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402

#: 分钟边界 + 一轮模型 + 调度器 reconcile（框架那边最长 60s）都算在内。
DEADLINE = float(os.environ.get("HELPDESK_PROBE_CRON_DEADLINE", "300"))
SEED_SESSIONS = 6
#: 调度记录叫 `autodream`，模型要调的工具叫 `run_autodream` —— 两个不是一个东西，
#: 拿前者去读后者的 tool_result 会永远读到空（真机 2026-09-27 20:38 就这么空转过一次）。
#: 工具名就是 `tools.py` 里那个函数名，A 段先离线钉住，再花分钟边界的钱。
AUTODREAM_TOOL = "run_autodream"

#: 六次会话的原始事实。分布是**故意做歪**的：并列时 `_plurality` 判"没有偏好"，
#: 一份全平局的种子只能验管道，验不出偏好表长什么样。
#: 上午 5／下午 1、周五 3、60 分钟 4、101 上门 4 次，各有各的严格多数。
_BASE = datetime(2026, 9, 7, 9, 0)  # 周一
_DAY_OFFSET = (0, 1, 1, 2, 1, 3)  # 相对 _BASE 的日偏移 → 周四/周五×3/周六/周日
_HOUR = (9, 9, 14, 9, 10, 9)
_DURATION = (60, 60, 120, 60, 60, 120)
_ENGINEER = (101, 102, 101, 101, 102, 101)
_MISSING = ([], ["planned_window"], [], ["planned_window"], [], ["planned_window"])
#: 结局行：一次成了、一次上门没解决、一次取消，其余三次还没到点（None）。
#: 完成率只算真上过门的两次 → 1/2；取消率的分母是约成的六次 → 1/6。
_OUTCOME = (
    ("appointment.visited", True),
    None,
    None,
    ("appointment.visited", False),
    None,
    ("appointment.cancelled", None),
)


def seed_ledger() -> dict[str, int]:
    """照两个 sink 的形状写种子日志（`ledger.py`）。

    `appointment.booked` 必须带 start + duration_minutes —— 缺任一 `fold` 会当场响，
    而这里要的就是"能被真折叠读过去"的行，不是一份看起来像的假数据。
    """
    ledger_path, _ = memory_paths(USER_ID)
    ledger = Ledger(ledger_path, user_id=USER_ID)
    counted = {"rows": 0, "tickets": 0, "appointments": 0}
    for i in range(SEED_SESSIONS):
        view = ledger.with_session(f"seed-{i}")
        start = _BASE + timedelta(days=3 + _DAY_OFFSET[i], hours=_HOUR[i] - _BASE.hour)
        wrote = start - timedelta(days=2)
        ticket_id = f"T-2026-{i:04d}"
        view.append(
            "ticket.created",
            ticket_id,
            {
                "category": "incident" if i % 2 else "request",
                "priority": "high" if i % 3 == 0 else "medium",
                "status": "assigned",
                "assignee_id": _ENGINEER[i],
                "missing": list(_MISSING[i]),
            },
            at=wrote,
        )
        detail = {
            "engineer_id": _ENGINEER[i],
            "start": start.isoformat(timespec="minutes"),
            "duration_minutes": _DURATION[i],
            "service_id": 2001,
            "ticket_id": ticket_id,
            "resolved": None,
        }
        view.append("appointment.booked", f"A-2026-{i:04d}", detail, at=wrote)
        counted["rows"] += 2
        counted["tickets"] += 1
        counted["appointments"] += 1
        outcome = _OUTCOME[i]
        if outcome is not None:
            kind, resolved = outcome
            view.append(kind, f"A-2026-{i:04d}", {**detail, "resolved": resolved}, at=start)
            counted["rows"] += 1
            counted["appointments"] += 1
    return counted


def row_counts() -> dict[str, int]:
    ledger_path, _ = memory_paths(USER_ID)
    ledger = Ledger(ledger_path, user_id=USER_ID)
    out = {"rows": 0, "tickets": 0, "appointments": 0}
    for row in ledger.rows():
        out["rows"] += 1
        if row.kind.startswith("ticket."):
            out["tickets"] += 1
        elif row.kind.startswith("appointment."):
            out["appointments"] += 1
    return out


# ---- 读落库的那一轮 --------------------------------------------------------
def _output_text(block: object) -> str:
    out = getattr(block, "output", None)
    if isinstance(out, str):
        return out
    bits: list[str] = []
    for part in out or ():
        if isinstance(part, dict):
            bits.append(str(part.get("text", "")))
        else:
            bits.append(str(getattr(part, "text", part)))
    return "\n".join(bits)


async def read_turn(storage, session_id: str) -> dict:
    """一次触发那个 session 里模型实际调了哪些工具、工具回了什么、这一轮值多少 token。"""
    msgs, _ = await storage.list_messages(USER_ID, session_id, limit=50)
    calls = [b.name for m in msgs for b in m.get_content_blocks("tool_call")]
    inputs = [
        f"{b.name}({str(b.input)[:60]})" for m in msgs for b in m.get_content_blocks("tool_call")
    ]
    results = {b.name: _output_text(b) for m in msgs for b in m.get_content_blocks("tool_result")}
    usage = next((m.usage for m in msgs if getattr(m, "usage", None) is not None), None)
    return {
        "session_id": session_id,
        "calls": calls,
        "inputs": inputs,
        "results": results,
        "msgs": len(msgs),
        "usage": usage,
    }


async def _wait_for(what: str, fn, *, timeout: float) -> object:
    """轮询到条件成立为止。cron 的粒度是分钟，所以这份等待就是这东西的真实节奏。"""
    deadline = time.monotonic() + timeout
    while True:
        got = await fn()
        if got:
            return got
        if time.monotonic() >= deadline:
            raise AssertionError(f"超时 {timeout:.0f}s 等不到：{what}")
        await asyncio.sleep(2.0)


async def _scheduled_sessions(storage, schedule_id: str, need: int):
    got = await storage.list_sessions_by_schedule(USER_ID, schedule_id)
    return got if len(got) >= need else None


async def _turn_settled(storage, session_id: str, *, running_last: bool) -> bool:
    """等那一轮"看得见结果"。只有最后一轮允许停在"调了但结果还没落库"，
    否则前面几轮会为了一个可能永远不来的 tool_result 白等一分钟。"""
    turn = await read_turn(storage, session_id)
    if AUTODREAM_TOOL in turn["results"]:
        return True
    return running_last and bool(turn["calls"])


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


async def _join(task: asyncio.Task) -> None:
    try:
        await task
    except Exception:  # noqa: BLE001
        pass


async def phase_a() -> tuple[list[str], dict[str, int]]:
    print(f"--- A：一次性世界（{AUTODREAM_NAME} 的心跳 = {AUTODREAM_CRON}）")
    print(f"  世界目录：{_WORLD}")
    fails: list[str] = []
    seeded = seed_ledger()
    ledger_path, memory_path = memory_paths(USER_ID)
    if not ledger_path.exists():
        fails.append("A1 种子日志没落盘")
    if memory_path.exists():
        fails.append("A2 计数表不该在折叠之前就存在")
    if seeded["rows"] != row_counts()["rows"]:
        fails.append(f"A3 回放读到的行数与写入不符：{row_counts()} vs {seeded}")
    # 量具先于结论：B 段是按工具名去读 tool_result 的，名字读错就是白等两分钟。
    wired = HelpdeskContext(
        index=FakeIndex(),
        ledger=Ledger(ledger_path, user_id=USER_ID),
        memory=MemoryStore(memory_path, user_id=USER_ID),
    )
    names = {t.name for t in build_tools(wired)}
    if AUTODREAM_TOOL not in names:
        fails.append(f"A4 挂了落盘事实源的上下文里没有 {AUTODREAM_TOOL}：{sorted(names)[:5]}…")
    print(f"  A 种子：{seeded['rows']} 行（工单 {seeded['tickets']}／预约 {seeded['appointments']}）"
          f"来自 {SEED_SESSIONS} 次会话｜检查点 0、计数表不存在 —— 闸门要看的就是这份新日志")
    print(f"  A4 工具名：挂上日志与计数表之后注册 {len(names)} 条，{AUTODREAM_TOOL} 在列")
    return fails, seeded


async def phase_b(app, seeded: dict[str, int], once: bool, t0: float) -> list[str]:
    fails: list[str] = []
    storage = app.state.storage
    bridge = app.state.agui_bridge

    # 桥第一次就绪才登记 cron —— 这就是"没人打开过对话的服务上没有这条调度"的那个设计，
    # 所以这一步替的是"第一个人打开前端"，没有替调度器本身。
    await bridge.start()
    records = await storage.list_schedules(USER_ID)
    mine = [r for r in records if r.data.name == AUTODREAM_NAME]
    if len(mine) != 1:
        fails.append(f"C1 桥就绪后调度存储里的 {AUTODREAM_NAME} 不是恰好一条：{len(mine)}")
        return fails
    record = mine[0]
    data = record.data
    print(f"\n--- B：真触发（服务在 {bridge.chat_model_config['model']} temp=0，等分钟边界）")
    print(f"  C1 登记：调度共 {len(records)} 条｜{AUTODREAM_NAME} 1 条"
          f"｜cron={data.cron_expression}｜tz={data.timezone}"
          f"｜permission={getattr(data.permission_mode, 'value', data.permission_mode)}"
          f"｜stateful={data.stateful}｜model={data.chat_model_config.model}")
    if data.chat_model_config.model != bridge.chat_model_config["model"]:
        fails.append("C1b 调度记录的模型与前台对话不是一个")

    _, memory_path = memory_paths(USER_ID)
    mem = MemoryStore(memory_path, user_id=USER_ID)
    before = mem.load()
    print(f"  触发前：seq={before.seq} runs={before.runs} last_run_at={before.last_run_at}")

    need = 1 if once else 2
    fired = await _wait_for(
        f"{need} 次触发产生的 session",
        lambda: _scheduled_sessions(storage, record.id, need),
        timeout=DEADLINE,
    )
    print(f"  C2 落地：调度创建了 {len(fired)} 个 session（存储给的是新的在前，按时间倒回来读）"
          f"｜第一个距启动 {time.monotonic() - t0:.0f}s")

    order = list(reversed(fired))
    turns = []
    for i, s in enumerate(order):
        is_last = i == len(order) - 1
        await _wait_for(
            f"第 {i + 1} 次触发的 session 里 {AUTODREAM_TOOL} 回了话",
            lambda s=s, is_last=is_last: _turn_settled(storage, s.id, running_last=is_last),
            timeout=DEADLINE,
        )
        turn = await read_turn(storage, s.id)
        turns.append(turn)
        used = turn["usage"]
        cost = f"in {used.input_tokens}｜out {used.output_tokens}" if used else "用量未落库"
        print(f"  第 {i + 1} 轮（session {turn['session_id'][:12]}…）：{turn['msgs']} 条消息｜"
              f"{cost}")
        print(f"     调用：{turn['inputs']}")

    # 最后一轮是按"看见调用"放行的（否则可能永远等不到），所以给它 60s 把
    # tool_result 落下来再判 —— 落不下来如实说，不硬失败。
    if len(turns) > 1:
        try:
            await _wait_for(
                "最后一轮的 tool_result",
                lambda: _turn_settled(storage, order[-1].id, running_last=False),
                timeout=60,
            )
        except AssertionError:
            pass
        else:
            turns[-1] = await read_turn(storage, order[-1].id)

    after = mem.load()
    print(f"  触发后：seq={after.seq} runs={after.runs} last_run_at={after.last_run_at}")

    first = turns[0]
    if AUTODREAM_TOOL not in first["calls"]:
        fails.append(f"C3 第一次触发那一轮没调 {AUTODREAM_TOOL}：{first['calls']}")
    text = first["results"].get(AUTODREAM_TOOL, "")
    if not text.startswith("AutoDream："):
        fails.append(f"C3b 工具正文不是 DreamResult.text()：{text[:80]!r}")
    else:
        print("  C3 执行：那一轮真的折了一次，工具回给模型的正文：")
        for line in text.splitlines():
            print(f"     | {line}")

    if after.runs != 1:
        fails.append(f"C4 折叠次数应为 1（第二轮只该被闸住）：runs={after.runs}")
    if after.seq != seeded["rows"]:
        fails.append(f"C4b 检查点应停在种子的最后一行 {seeded['rows']}，实际 {after.seq}")

    if len(turns) > 1:
        second = turns[1]
        s2 = second["results"].get(AUTODREAM_TOOL, "")
        if AUTODREAM_TOOL not in second["calls"]:
            fails.append(f"C4c 第二次心跳没调 {AUTODREAM_TOOL}：{second['calls']}")
        elif not s2:
            print("  C4 第二轮：tool_result 还没落库（那一轮仍在跑）—— 有没有重折以计数表 runs 为准")
        else:
            print(f"  C4 第二轮心跳的正文：{s2.splitlines()[0]}")
            if "没跑" not in s2:
                fails.append(f"C4d 第二轮该被闸住，正文里却没有不跑的理由：{s2[:80]!r}")
    else:
        print("  C4 第二轮：没等（--once），闸门这一条只由单测覆盖")

    now = row_counts()
    print(f"  C5 明细：日志 {now['rows']} 行（种子 {seeded['rows']}）｜"
          f"工单 {now['tickets']}／预约 {now['appointments']}")
    if now["rows"] != seeded["rows"]:
        fails.append(f"C5 无人值守那几轮往日志里添了行：{now} vs 种子 {seeded}")

    prefs = mem.preferences()
    print(f"  偏好表落盘：{memory_path}")
    for line in prefs.render().splitlines():
        print(f"     {line}")
    return fails


async def main(once: bool) -> int:
    fails, seeded = await phase_a()
    for f in fails:
        print(f"  ✗ {f}")
    if fails:
        return 1

    shutil.copytree(_SRC_VECTOR, os.environ["HELPDESK_VECTOR_DB"])
    port = _free_port()
    async with await open_index(db_path=os.environ["HELPDESK_VECTOR_DB"]) as index:
        await index.ensure_ready()
        app = make_service_app(index, self_base=f"http://127.0.0.1:{port}")
        server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="error"))
        task = asyncio.create_task(server.serve())
        for _ in range(100):
            if server.started:
                break
            await asyncio.sleep(0.2)
        else:
            raise AssertionError("uvicorn 没起来")
        t0 = time.monotonic()
        try:
            fails += await phase_b(app, seeded, once, t0)
        finally:
            server.should_exit = True
            await asyncio.wait_for(_join(task), timeout=20)

    print("\n=== 结论")
    for f in fails:
        print(f"  ✗ {f}")
    code = 0 if not fails else 1
    print(f"  {'全部通过' if not fails else f'{len(fails)} 条不通过'}｜退出码 {code}")
    return code


if __name__ == "__main__":
    try:
        _code = asyncio.run(main("--once" in sys.argv))
    finally:
        shutil.rmtree(_WORLD, ignore_errors=True)
        print(f"清理一次性世界：{_WORLD}")
    raise SystemExit(_code)
