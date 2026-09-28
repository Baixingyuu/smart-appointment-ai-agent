"""AutoDream：把事件日志增量折成长期记忆的后台任务，以及它挂上框架调度器的那段线。

三件套各有其因，缺一个就会长出很难查的错：

- **检查点**（`Counters.seq`）：只折"上次之后"的行。没有它，每轮都从头重算，
  计数会指数膨胀；有它，崩溃之后重跑等于没跑过（落盘成功才推进 seq）。
- **任务锁**（`Ledger.lock`）：折叠是 读表 → 改 → 整表写回，两台交错就能让同一批行
  被折两次。拿不到锁直接跳过这一轮，不自旋 —— cron 还会再来。
- **变更日志**（`ledger.py`）：结论必须可重算，所以事实源单独留一份追加式记录。

为什么走"定时唤醒一次 agent、由它调用工具"而不是自己起一个 `asyncio` 循环：
框架的调度器把定时器、多节点归属（只有一个节点持有 timer，storage 才是事实源）、
misfire 宽限、以及"这一轮跑在哪个 session 里、事件流看得见"都一并给了 ——
自己重写这些就是 原生优先 这条裁决要避免的事。代价也写清楚：每次触发要多花一轮
模型调用，而调度器能送的只有 `description` 这段文本，所以"做什么"必须长在
`run_autodream` 工具里，模型的活儿只是把它调一次。
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import Any

from .ledger import Ledger, Row
from .memory import Counters, MemoryStore, Preferences, fold, summarize

#: 满这么多次**新会话**才值得做一次增量回放。少于这个数，折出来的"偏好"全是噪声。
MIN_SESSIONS = int(os.environ.get("HELPDESK_AUTODREAM_MIN_SESSIONS", "5"))
#: 两次运行之间的最小间隔。cron 是心跳，真正的条件是这两条。
MIN_INTERVAL = timedelta(hours=float(os.environ.get("HELPDESK_AUTODREAM_MIN_INTERVAL_HOURS", "24")))

AUTODREAM_NAME = "autodream"
#: 凌晨三点：站班窗口（8–20）之外，不跟白天的对话抢同一个人的注意力。
AUTODREAM_CRON = os.environ.get("HELPDESK_AUTODREAM_CRON", "0 3 * * *")

#: 这就是调度器唯一能递给 agent 的东西 —— 所以"只调这一个工具"要写死在这里。
#: `force 留空`那句不是洁癖：真机 2026-09-27（`probes/p6_autodream_cron.py`）两次心跳
#: 模型都自己传了 `{"force": true}` —— 它把"立刻"读成了"强制"，而 force 正是那两道闸的
#: 旁路。调度路径上没人能纠正它，所以只能在这段文本里把参数说死。
AUTODREAM_PROMPT = (
    "后台整理任务：立刻调用一次 run_autodream 工具，force 参数留空（也就是 false），"
    "把它回给你的那一行结果原样转述，然后结束。\n"
    "不要调用任何其它工具，不要建单、不要指派、不要落预约 —— 这是一个人在睡觉时跑的批次，"
    "任何写操作都没有人能确认。"
)


@dataclass(frozen=True)
class DreamResult:
    """一次 AutoDream 的账。`ran=False` 时 `reason` 必须能看出为什么不跑。"""

    ran: bool
    reason: str
    rows: int = 0
    sessions: int = 0
    until_seq: int = 0
    preferences: Preferences | None = None

    def text(self) -> str:
        if not self.ran:
            return f"AutoDream 本轮没跑：{self.reason}"
        assert self.preferences is not None
        head = (
            f"AutoDream：折叠 {self.rows} 行事实（覆盖 {self.sessions} 次会话），"
            f"检查点推到 seq={self.until_seq}。"
        )
        return f"{head}\n{self.preferences.render()}"


def pending(ledger: Ledger, counters: Counters) -> tuple[list[Row], int]:
    """待折的行 + 这些行里出现过多少个不同会话。

    会话数从**待折的行**里数，不从计数表里数：计数表只在跑成功之后才更新，
    拿它当闸门就是"要等它非空才肯跑，而它只有跑过才非空"的死锁。
    """
    rows = list(ledger.rows(since=counters.seq))
    return rows, len({r.session_id for r in rows if r.session_id})


def gate(
    counters: Counters,
    *,
    rows: int,
    sessions: int,
    now: datetime,
    min_sessions: int = MIN_SESSIONS,
    min_interval: timedelta = MIN_INTERVAL,
) -> tuple[bool, str]:
    if rows == 0:
        return False, "日志里没有比检查点更新的事实"
    if counters.last_run_at is not None:
        waited = now - counters.last_run_at
        if waited < min_interval:
            return False, f"距上次运行 {waited.total_seconds() / 3600:.1f}h，不足 {min_interval.total_seconds() / 3600:.0f}h"
    if sessions < min_sessions:
        return False, f"新会话 {sessions} 次，不足 {min_sessions} 次"
    return True, "够数"


def run(
    *,
    ledger: Ledger,
    memory: MemoryStore,
    now: datetime | None = None,
    force: bool = False,
    min_sessions: int = MIN_SESSIONS,
    min_interval: timedelta = MIN_INTERVAL,
    holder: str = "",
) -> DreamResult:
    """跑一轮增量回放。幂等：落盘成功之后才推进检查点，中途崩了重跑等于没跑过。"""
    stamp = now or datetime.now()
    lock = ledger.lock(holder=holder or "autodream")
    if not lock.acquire_nowait():
        return DreamResult(ran=False, reason="另一个进程正持有任务锁，本轮跳过")
    try:
        counters = memory.load()
        rows, sessions = pending(ledger, counters)
        if force:
            # force 跳的是"够不够数"那两道闸，跳不过"没有新事实"这件事实：
            # 检查点要推到 `rows[-1]`，空批在那儿就是 IndexError（真机 2026-09-27 20:44
            # 第二次心跳撞上过，框架把这句异常当成工具正文回给了模型）。
            if not rows:
                return DreamResult(ran=False, reason="日志里没有比检查点更新的事实，force 也折不出东西")
            ok, reason = True, "手动重算"
        else:
            ok, reason = gate(
                counters,
                rows=len(rows),
                sessions=sessions,
                now=stamp,
                min_sessions=min_sessions,
                min_interval=min_interval,
            )
        if not ok:
            return DreamResult(ran=False, reason=reason, rows=len(rows), sessions=sessions)
        fold(counters, rows)
        # 只认领读到手的这批行：跑的过程中追加进来的行留给下一轮，不然它们的 seq
        # 会被一次都没折过的检查点跳过。
        counters.seq = rows[-1].seq
        counters.last_run_at = stamp
        counters.runs += 1
        memory.save(counters)
        return DreamResult(
            ran=True,
            reason=reason,
            rows=len(rows),
            sessions=sessions,
            until_seq=counters.seq,
            preferences=summarize(counters),
        )
    finally:
        lock.release()


# ---- 接框架调度器 ----------------------------------------------------------

async def ensure_autodream_schedule(
    *,
    storage: Any,
    scheduler: Any,
    user_id: str,
    agent_id: str,
    chat_model_config: dict[str, Any],
    timezone: str,
    cron_expression: str = AUTODREAM_CRON,
    description: str = AUTODREAM_PROMPT,
) -> str | None:
    """把 AutoDream 的 cron 落到框架的调度存储里，并且**收敛**：库里那份与本进程想写的
    那份不一致就原地改（同一条记录，不堆第二条）。一字不差时返回 None，写过返回记录 id。

    为什么不走框架白送的 `ScheduleCreate` 工具：那是给模型用的，而我们的工具面白名单
    刻意不放行任何调度件 —— 让模型给自己加 cron，等于把"什么时候能醒"交给提示词。
    写入形状照 `ScheduleCreate.__call__`：`validate_schedule` 先验 cron，
    `upsert_schedule` 落库，`notify_changed` 提醒持有 timer 的那个节点来 reconcile。

    `permission_mode=DONT_ASK` 是框架给调度任务的默认值，对我们是硬保证而不是提醒：
    该模式把每一条 ASK 换成 DENY（`permission/_types.py` 的 DONT_ASK 一行），所以这一轮
    里 `create_ticket` / `book_appointment` 这类要人确认的写工具物理上执行不了。
    """
    from agentscope.app.storage import ChatModelConfig, ScheduleData, ScheduleRecord, ScheduleSource
    from agentscope.permission import PermissionMode

    existing = await storage.list_schedules(user_id)
    found = next((r for r in existing if r.data.name == AUTODREAM_NAME), None)
    if found is None:
        record = ScheduleRecord(
            user_id=user_id,
            agent_id=agent_id,
            data=ScheduleData(
                name=AUTODREAM_NAME,
                description=description,
                cron_expression=cron_expression,
                timezone=timezone,
                started_at=datetime.now(),
                stateful=False,
                permission_mode=PermissionMode.DONT_ASK,
                source=ScheduleSource.AGENT,
                source_session_id="",
                chat_model_config=ChatModelConfig(**chat_model_config),
            ),
        )
    else:
        # 名字归我们所有，所以"这个进程想写的那份"才是事实源。原样跳过等于把改动永久留在
        # 家里：`AUTODREAM_PROMPT` 那句 force=false 是 2026-09-27 真机之后加的，
        # 而已经登记过的部署永远读不到它。`enabled` 不在名单里 —— 人把它关掉是熔断，
        # 不是漂移，别每次启动都替他打开。
        want = {
            "cron_expression": cron_expression,
            "timezone": timezone,
            "description": description,
            "chat_model_config": ChatModelConfig(**chat_model_config),
            # 这两个是安全边界而不是配置：无人看管的那一轮必须停在 DONT_ASK（ASK→DENY），
            # 折叠也必须每次一个新 session。旧记录里它们要是漂了，就是这里该修。
            "permission_mode": PermissionMode.DONT_ASK,
            "stateful": False,
        }
        drift = {k: v for k, v in want.items() if getattr(found.data, k) != v}
        if not drift and found.agent_id == agent_id:
            return None
        record = found.model_copy(
            update={"agent_id": agent_id, "data": found.data.model_copy(update=drift)},
        )
    scheduler.validate_schedule(record)
    await storage.upsert_schedule(user_id, record)
    await scheduler.notify_changed(record.id)
    return record.id


__all__ = [
    "AUTODREAM_CRON",
    "AUTODREAM_NAME",
    "AUTODREAM_PROMPT",
    "DreamResult",
    "MIN_INTERVAL",
    "MIN_SESSIONS",
    "ensure_autodream_schedule",
    "gate",
    "pending",
    "run",
]
