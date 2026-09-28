"""AutoDream（P6-a②）：闸门、增量折叠、崩溃重放、任务锁，以及接框架调度器那段线。

全离线。这里能离线钉死的是**账**：闸门为什么不跑、折进去几条、检查点推到几、
崩在落盘之前重跑会不会把同一批行折两遍。跑在真调度器上是什么表现由
`probes/p6_autodream_cron.py` 量 —— 2026-09-27 那一跑就是从真机带回来一条下面钉住的
崩法（`force` 遇上空批），所以这两处是一来一回，不是各说各话。

有一条是硬依赖而不是风格：`run_autodream` 的权限必须是 ALLOW。调度器给的 session
跑在 `permission_mode=DONT_ASK` 下，那个模式把每一条 ASK 换成 DENY —— 一个要走确认
闸的整理工具等于 AutoDream 永远跑不了。这个反向保证同时是安全边界：同一轮里
`create_ticket` / `book_appointment` 物理上执行不了。
"""
from __future__ import annotations

import sys
from datetime import datetime, timedelta
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from agentscope.app._manager import SchedulerManager  # noqa: E402
from agentscope.app.storage import ScheduleRecord, ScheduleSource  # noqa: E402
from agentscope.message import ToolResultState  # noqa: E402
from agentscope.permission import PermissionBehavior, PermissionMode  # noqa: E402

from helpdesk import autodream  # noqa: E402
from helpdesk.appointment import AppointmentStore  # noqa: E402
from helpdesk.dispatch import AssignmentDecision  # noqa: E402
from helpdesk.autodream import (  # noqa: E402
    AUTODREAM_CRON,
    AUTODREAM_NAME,
    MIN_INTERVAL,
    MIN_SESSIONS,
    Counters,
    DreamResult,
    ensure_autodream_schedule,
    gate,
    pending,
    run,
)
from helpdesk.domain import Category, Priority  # noqa: E402
from helpdesk.eval.fakes import FakeIndex  # noqa: E402
from helpdesk.ledger import Ledger, appointment_sink, ticket_sink  # noqa: E402
from helpdesk.memory import MemoryStore, fold, summarize  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402
from helpdesk.tools import HelpdeskContext, build_tools  # noqa: E402

#: 与真实时钟隔开，`last_run_at` 的账才不受"今天几号"影响；比所有被测时刻都早，
#: 否则预约域那道"不能约到过去"的闸会先把造数据的这一步挡下。
NOW = datetime(2026, 10, 8, 8, 0)


def _paths(tmp_path: Path) -> tuple[Ledger, MemoryStore]:
    return (
        Ledger(tmp_path / "ops.ledger.jsonl", user_id="ops"),
        MemoryStore(tmp_path / "ops.memory.json", user_id="ops"),
    )


def _book_session(ledger: Ledger, *, session: str, day: int, hour: int = 10) -> None:
    """一次会话：一张单 + 一个上门。写穿之后日志里就是两行。"""
    view = ledger.with_session(session)
    store = TicketStore(sink=ticket_sink(view))
    appts = AppointmentStore(sink=appointment_sink(view))
    created = store.create(
        title="下单接口 401",
        description="点了就报 401，重试也一样",
        category=Category.INCIDENT,
        priority=Priority.P1,
        missing_info=("affected_system",),
    )
    appts.book(
        engineer_id=101,
        start=datetime(2026, 10, day, hour, 0),
        duration_minutes=60,
        need="现场排查下单接口 401",
        ticket_id=created.ticket.id,
        now=NOW,
    )


def _seed(tmp_path: Path, *, sessions: int = MIN_SESSIONS) -> tuple[Ledger, MemoryStore]:
    ledger, memory = _paths(tmp_path)
    for i in range(sessions):
        _book_session(ledger, session=f"s{i + 1}", day=12 + i)
    return ledger, memory


def _folded(ledger: Ledger) -> Counters:
    counters = Counters(user_id="ops")
    return fold(counters, ledger.rows())


# --- 1. 闸门 ---------------------------------------------------------------


def test_没有新事实时不跑() -> None:
    ok, reason = gate(Counters(user_id="ops"), rows=0, sessions=0, now=NOW)
    assert (ok, reason) == (False, "日志里没有比检查点更新的事实")


def test_间隔不足时不跑() -> None:
    counters = Counters(user_id="ops", last_run_at=NOW - timedelta(hours=23))
    ok, reason = gate(counters, rows=12, sessions=9, now=NOW)
    assert ok is False
    assert "23.0h" in reason
    assert f"不足 {MIN_INTERVAL.total_seconds() / 3600:.0f}h" in reason


def test_会话数不足时不跑() -> None:
    ok, reason = gate(Counters(user_id="ops"), rows=4, sessions=MIN_SESSIONS - 1, now=NOW)
    assert ok is False
    assert reason == f"新会话 {MIN_SESSIONS - 1} 次，不足 {MIN_SESSIONS} 次"


def test_三条理由彼此分开() -> None:
    """`reason` 是给运维看的：都写成"没跑"就查不出是等数还是等时间。"""
    reasons = {
        gate(Counters(user_id="ops"), rows=0, sessions=0, now=NOW)[1],
        gate(Counters(user_id="ops", last_run_at=NOW), rows=9, sessions=9, now=NOW)[1],
        gate(Counters(user_id="ops"), rows=9, sessions=1, now=NOW)[1],
    }
    assert len(reasons) == 3


def test_够数才放行() -> None:
    assert gate(Counters(user_id="ops"), rows=10, sessions=MIN_SESSIONS, now=NOW) == (True, "够数")


# --- 2. 待折的行与会话数 ----------------------------------------------------


def test_检查点之后的行才算新事实(tmp_path: Path) -> None:
    ledger, _ = _seed(tmp_path)
    rows, sessions = pending(ledger, Counters(user_id="ops"))
    assert (len(rows), sessions) == (2 * MIN_SESSIONS, MIN_SESSIONS)
    rows8, _ = pending(ledger, Counters(user_id="ops", seq=8))
    assert [r.seq for r in rows8] == [9, 10]
    assert list(pending(ledger, Counters(user_id="ops", seq=10))[0]) == []


def test_会话数从待折的行里数(tmp_path: Path) -> None:
    """计数表只在跑成功之后更新 —— 拿它当闸门就是"要等它非空才肯跑，而它跑过才非空"。

    跑过一轮之后再写一行（只来自一条新会话）：新事实有，会话数不够，闸门应该按
    "1 次会话"报，而不是读那份已经非空的计数表说"好几次了"。
    """
    ledger, memory = _seed(tmp_path)
    run(ledger=ledger, memory=memory, now=NOW)
    _book_session(ledger, session="s-new", day=25)
    rows, sessions = pending(ledger, memory.load())
    assert (len(rows), sessions) == (2, 1)
    result = run(ledger=ledger, memory=memory, now=NOW + timedelta(days=1))
    assert result.ran is False and result.sessions == 1


# --- 3. 跑一轮的账 ----------------------------------------------------------


def test_跑一轮把检查点推到当时最后一行(tmp_path: Path) -> None:
    ledger, memory = _seed(tmp_path)
    result = run(ledger=ledger, memory=memory, now=NOW)
    assert (result.ran, result.rows, result.sessions, result.until_seq) == (True, 10, 5, 10)
    assert result.reason == "够数"
    counters = memory.load()
    assert (counters.runs, counters.seq, counters.last_run_at) == (1, 10, NOW)


def test_同一批事实不会被折两次(tmp_path: Path) -> None:
    ledger, memory = _seed(tmp_path)
    run(ledger=ledger, memory=memory, now=NOW)
    again = run(ledger=ledger, memory=memory, now=NOW + timedelta(days=2))
    assert again.ran is False
    counters = memory.load()
    assert (counters.tickets, sum(counters.buckets.values()), counters.runs) == (5, 5, 1)


def test_force跳过闸门但只折新行(tmp_path: Path) -> None:
    ledger, memory = _seed(tmp_path, sessions=2)
    result = run(ledger=ledger, memory=memory, now=NOW, force=True)
    assert result.ran is True and result.sessions == 2
    assert memory.load().tickets == 2


def test_没东西可折时force也不许把检查点推到空批(tmp_path: Path) -> None:
    """真机 2026-09-27 20:44 的第二次心跳：模型给 `run_autodream` 传了 force，
    而检查点已经追上日志尾 —— `rows[-1]` 当场 IndexError，框架把那句异常当工具正文
    回给了模型。force 跳的是"够不够数"两道阈值，跳不过"没有新事实"这件事实。
    """
    ledger, memory = _seed(tmp_path)
    assert run(ledger=ledger, memory=memory, now=NOW).ran is True
    again = run(ledger=ledger, memory=memory, now=NOW + timedelta(hours=1), force=True)
    assert again.ran is False
    assert "检查点" in again.reason
    counters = memory.load()
    assert (counters.runs, counters.seq) == (1, 10)


def test_崩在落盘之前重跑等于没跑过(tmp_path: Path) -> None:
    """检查点是"已折到第几行"，落盘失败就不能推进 —— 否则那批行永远丢掉。"""
    ledger, memory = _seed(tmp_path)
    real_save = memory.save

    def crash(counters: Counters) -> None:
        raise OSError("磁盘满了")

    memory.save = crash  # type: ignore[method-assign]
    with pytest.raises(OSError):
        run(ledger=ledger, memory=memory, now=NOW)
    assert not memory.path.exists()
    memory.save = real_save  # type: ignore[method-assign]
    result = run(ledger=ledger, memory=memory, now=NOW)
    assert (result.rows, result.until_seq) == (10, 10)
    counters = memory.load()
    assert (counters.tickets, sum(counters.buckets.values())) == (5, 5)


def test_跑的过程中追加的行留给下一轮(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """检查点只能推到**读到手**的那一批：跑到一半写进来的行一条都没折，推上去就是丢。"""
    ledger, memory = _seed(tmp_path)
    real_fold = autodream.fold

    def folding(*args, **kwargs):
        _book_session(ledger, session="s-late", day=25)  # 抢在落盘之前追加
        return real_fold(*args, **kwargs)

    monkeypatch.setattr(autodream, "fold", folding)
    result = run(ledger=ledger, memory=memory, now=NOW)
    assert (result.rows, result.until_seq) == (10, 10)
    assert memory.load().tickets == 5
    left = run(ledger=ledger, memory=memory, now=NOW + timedelta(days=1), force=True)
    assert (left.rows, left.until_seq) == (2, 12)
    assert memory.load().tickets == 6


def test_拿不到锁就跳过这一轮(tmp_path: Path) -> None:
    ledger, memory = _seed(tmp_path)
    held = ledger.lock(holder="别的进程")
    assert held.acquire_nowait() is True
    result = run(ledger=ledger, memory=memory, now=NOW)
    assert result.ran is False and "任务锁" in result.reason
    assert not memory.path.exists()  # 连表都没读出来，更没写
    held.release()


def test_跑完之后锁还在(tmp_path: Path) -> None:
    """`finally` 里释放：漏放一次，之后每一轮都只会回"另一个进程正持有任务锁"。"""
    ledger, memory = _seed(tmp_path)
    run(ledger=ledger, memory=memory, now=NOW)
    probe = ledger.lock()
    assert probe.acquire_nowait() is True
    probe.release()


def test_没跑成的结果也要能读出一行话(tmp_path: Path) -> None:
    """调度那一轮要把工具回的那行话原样转述给人看，`ran=False` 不能是空字符串。"""
    result = DreamResult(ran=False, reason="距上次运行 1.0h，不足 24h")
    assert result.text() == "AutoDream 本轮没跑：距上次运行 1.0h，不足 24h"


# --- 4. 折叠规则 -----------------------------------------------------------


def test_上门的时刻折成星期与时段(tmp_path: Path) -> None:
    ledger, _ = _seed(tmp_path)
    counters = _folded(ledger)
    assert counters.tickets == 5
    assert counters.buckets == {"上午": 5}
    assert counters.durations == {"60": 5}
    assert counters.engineers == {"101": 5}
    assert counters.missing_slots == {"affected_system": 5}


def test_一张单的五条进展只算一次缺失(tmp_path: Path) -> None:
    ledger = Ledger(tmp_path / "l.jsonl", user_id="ops", session_id="s1")
    store = TicketStore(sink=ticket_sink(ledger))
    created = store.create(
        title="VPN 连不上",
        description="客户端起不来",
        category=Category.INCIDENT,
        priority=Priority.P2,
        missing_info=("affected_system",),
    )
    tid = created.ticket.id
    store.assign(tid, AssignmentDecision(assignee="101", rationale="服务主责", confidence=0.8))
    store.accept(tid)
    store.comment(tid, "补充：macOS 客户端")
    store.resolve(tid)
    counters = _folded(ledger)
    assert len(list(ledger.rows())) == 5
    assert (counters.tickets, counters.missing_slots) == (1, {"affected_system": 1})


def test_改期不会被当成第二次偏好(tmp_path: Path) -> None:
    """一次上门折一次。改期行留在日志里，要统计改期率随时能重算。"""
    ledger = Ledger(tmp_path / "l.jsonl", user_id="ops", session_id="s1")
    store = AppointmentStore(sink=appointment_sink(ledger))
    appt = store.book(
        engineer_id=101,
        start=datetime(2026, 10, 12, 10, 0),
        duration_minutes=60,
        need="现场排查",
        now=NOW,
    )
    store.reschedule(appt.id, start=datetime(2026, 10, 14, 15, 0), now=NOW)
    counters = _folded(ledger)
    assert counters.buckets == {"上午": 1}
    assert counters.weekdays == {"周一": 1}


def test_不认识的kind跳过但写坏的旧行必须响(tmp_path: Path) -> None:
    """以后还会加新事件类型，旧规则读到新行不该炸；写坏的旧行反过来不能悄悄放过。"""
    ledger = Ledger(tmp_path / "l.jsonl", user_id="ops", session_id="s1")
    ledger.append("channel.message", "x", {"whatever": 1})
    assert _folded(ledger).tickets == 0
    ledger.append("appointment.booked", 2, {"engineer_id": 101})
    counters = Counters(user_id="ops")
    with pytest.raises(ValueError, match="第 2 行"):
        fold(counters, ledger.rows(since=1))


# --- 5. 派生视图的账 --------------------------------------------------------


def test_并列不算偏好(tmp_path: Path) -> None:
    """五个不同星期各一次 —— "最常约在周X"这句没有依据，而模型会把它当依据。"""
    ledger, _ = _paths(tmp_path)
    for i in range(5):
        _book_session(ledger, session=f"s{i}", day=12 + i)  # 10-12 起连续五天，五个不同星期
    counters = _folded(ledger)
    prefs = summarize(counters)
    assert len(counters.weekdays) == 5
    assert prefs.preferred_weekday is None
    assert prefs.preferred_bucket == ("上午", 5)


def test_完成率与取消率各有各的分母(tmp_path: Path) -> None:
    """约了还没上门的既不算完成也不算失败；取消率的分母是约成的次数。"""
    ledger = Ledger(tmp_path / "l.jsonl", user_id="ops", session_id="s1")
    store = AppointmentStore(sink=appointment_sink(ledger))
    def book(day: int) -> int:
        return store.book(
            engineer_id=101,
            start=datetime(2026, 10, day, 10, 0),
            duration_minutes=60,
            need="现场排查",
            now=NOW,
        ).id

    done, still_open, gone = book(12), book(13), book(14)
    store.visit(done, resolved=True, followup_note="换了令牌，已验证", now=NOW)
    store.cancel(gone, reason="改线上", now=NOW)
    prefs = summarize(_folded(ledger))
    assert prefs.bookings == 3
    assert prefs.completion_rate == 1.0  # 分母是"真的上过门"的那一次
    assert prefs.cancel_rate == pytest.approx(1 / 3)
    assert store.get(still_open).occupies  # 约着没上门：既不算完成也不算失败


def test_每个结论后面都跟着样本数(tmp_path: Path) -> None:
    ledger, memory = _seed(tmp_path)
    prefs = run(ledger=ledger, memory=memory, now=NOW).preferences
    lines = prefs.render().splitlines()
    assert "- 上门时段：上午 5/5 次" in lines
    assert lines[-1] == "- 依据：5 张工单 / 5 次预约"


def test_空表说的是没有记录(tmp_path: Path) -> None:
    prefs = summarize(Counters(user_id="ops"))
    assert prefs.empty is True
    assert prefs.render() == "没有历史记录（第一次接触这位用户）。别拿假设当偏好。"


# --- 6. 工具与调度记录 ------------------------------------------------------


def _ctx(tmp_path: Path, *, sessions: int = MIN_SESSIONS) -> HelpdeskContext:
    ledger, memory = _seed(tmp_path, sessions=sessions)
    return HelpdeskContext(
        index=FakeIndex(),
        store=TicketStore(sink=ticket_sink(ledger)),
        appointments=AppointmentStore(sink=appointment_sink(ledger)),
        ledger=ledger,
        memory=memory,
    )


def _tools(ctx: HelpdeskContext) -> dict:
    return {t.name: t for t in build_tools(ctx)}


async def test_整理工具必须是可以直接跑的(tmp_path: Path) -> None:
    """DONT_ASK 把 ASK 换成 DENY：这里若是 ASK，调度醒来的那一轮什么都做不了。"""
    tools = _tools(_ctx(tmp_path))
    for name in ("run_autodream", "recall_preferences"):
        decision = await tools[name].check_permissions({}, None)
        assert decision.behavior is PermissionBehavior.ALLOW, name


async def test_run_autodream把账写在metadata里(tmp_path: Path) -> None:
    tools = _tools(_ctx(tmp_path))
    chunk = await tools["run_autodream"].call(force=False)
    assert chunk.state is not ToolResultState.ERROR
    assert chunk.metadata == {
        "ran": True,
        "reason": "够数",
        "rows": 10,
        "sessions": 5,
        "until_seq": 10,
        "empty": False,
    }
    assert "折叠 10 行事实" in chunk.content[0].text
    again = await tools["run_autodream"].call()
    assert again.metadata["ran"] is False
    assert again.metadata["rows"] == 0


async def test_没挂落盘事实源时不递交整理工具(tmp_path: Path) -> None:
    """console / 单测 / 评测的上下文没有日志，递交一个永远回 not_wired 的工具只会多烧一轮。"""
    ctx = HelpdeskContext(index=FakeIndex(), ledger=None, memory=None)
    assert "run_autodream" not in _tools(ctx)
    assert "recall_preferences" in _tools(ctx)


async def test_读偏好在没表的上下文里如实说没有(tmp_path: Path) -> None:
    ctx = HelpdeskContext(index=FakeIndex(), store=TicketStore())
    chunk = await _tools(ctx)["recall_preferences"].call()
    assert "不落盘" in chunk.content[0].text
    assert chunk.metadata == {"empty": True, "wired": False}


async def test_读偏好念的是折好的结论(tmp_path: Path) -> None:
    ctx = _ctx(tmp_path)
    run(ledger=ctx.ledger, memory=ctx.memory, now=NOW)
    chunk = await _tools(ctx)["recall_preferences"].call()
    assert "- 上门时段：上午 5/5 次" in chunk.content[0].text
    assert chunk.metadata == {"empty": False, "bookings": 5, "tickets": 5, "seq": 10, "wired": True}


class _FakeStorage:
    def __init__(self) -> None:
        self.rows: list[ScheduleRecord] = []

    async def list_schedules(self, user_id: str) -> list[ScheduleRecord]:
        return [r for r in self.rows if r.user_id == user_id]

    async def upsert_schedule(self, user_id: str, record: ScheduleRecord) -> None:
        """照真的 `_write_row`：按 id 覆盖。收敛那条线要测的就是"同一条被改，而不是多一条"。"""
        for i, row in enumerate(self.rows):
            if row.id == record.id:
                self.rows[i] = record
                return
        self.rows.append(record)


class _FakeScheduler:
    """只记账，但 cron 交给**真的** `validate_schedule` 验 —— 假调度器不该替我们把错放行。"""

    def __init__(self) -> None:
        self.notified: list[str] = []
        self.validated: int = 0

    def validate_schedule(self, record: ScheduleRecord):
        self.validated += 1
        return SchedulerManager.validate_schedule(record)

    async def notify_changed(self, schedule_id: str) -> None:
        self.notified.append(schedule_id)


async def test_调度记录的形状() -> None:
    storage, scheduler = _FakeStorage(), _FakeScheduler()
    schedule_id = await ensure_autodream_schedule(
        storage=storage,
        scheduler=scheduler,
        user_id="ops",
        agent_id="agent-1",
        chat_model_config={
            "type": "ollama_credential",
            "credential_id": "cred-1",
            "model": "qwen3:8b",
            "parameters": {"temperature": 0.0, "thinking_enable": False},
        },
        timezone="Asia/Shanghai",
    )
    assert schedule_id is not None
    (record,) = storage.rows
    data = record.data
    assert (record.user_id, record.agent_id) == ("ops", "agent-1")
    assert (data.name, data.cron_expression, data.timezone) == (
        AUTODREAM_NAME,
        AUTODREAM_CRON,
        "Asia/Shanghai",
    )
    # 无状态：每次醒来都是一个新 session，不该把整理当成"上次对话的继续"
    assert data.stateful is False
    # 后台没人看管：ASK 一律变 DENY，写操作物理上执行不了
    assert data.permission_mode is PermissionMode.DONT_ASK
    assert data.source is ScheduleSource.AGENT
    assert "run_autodream" in data.description
    assert data.chat_model_config.model == "qwen3:8b"
    # 先验后写，写完提醒持有 timer 的那个节点来 reconcile
    assert (scheduler.validated, scheduler.notified) == (1, [record.id])


async def test_坏cron在落库之前就被拦下() -> None:
    storage, scheduler = _FakeStorage(), _FakeScheduler()
    with pytest.raises(ValueError):
        await ensure_autodream_schedule(
            storage=storage,
            scheduler=scheduler,
            user_id="ops",
            agent_id="agent-1",
            chat_model_config={"type": "ollama_credential", "credential_id": "c", "model": "m", "parameters": {}},
            timezone="Asia/Shanghai",
            cron_expression="每天三点",
        )
    assert storage.rows == []


def _reg_kwargs(storage: _FakeStorage, scheduler: _FakeScheduler) -> dict:
    return dict(
        storage=storage,
        scheduler=scheduler,
        user_id="ops",
        agent_id="agent-1",
        chat_model_config={
            "type": "ollama_credential",
            "credential_id": "c",
            "model": "qwen3:8b",
            "parameters": {},
        },
        timezone="Asia/Shanghai",
    )


async def test_注册是幂等的() -> None:
    """重启一次堆一条 cron，等于每天醒很多遍 —— 判据是 name，不是 id（id 每次新生成）。"""
    storage, scheduler = _FakeStorage(), _FakeScheduler()
    first = await ensure_autodream_schedule(**_reg_kwargs(storage, scheduler))
    second = await ensure_autodream_schedule(**_reg_kwargs(storage, scheduler))
    assert first is not None and second is None
    assert len(storage.rows) == 1
    assert scheduler.notified == [first]  # 一字不差就不写，也不惊动持有 timer 的那台


async def test_改了周期或措辞是刷那一条而不是堆第二条() -> None:
    """「有同名就跳过」等于把改动永久留在家里：已经登记过的部署永远不会读到新的
    `AUTODREAM_PROMPT` —— 真机 2026-09-27 那条 force 旁路就是这么留在原地的。
    现在按字段收敛，且**同一条记录**（换了 id 就是两条 cron、每天醒两遍）。
    """
    storage, scheduler = _FakeStorage(), _FakeScheduler()
    first = await ensure_autodream_schedule(**_reg_kwargs(storage, scheduler))
    kwargs = _reg_kwargs(storage, scheduler)
    changed = await ensure_autodream_schedule(
        **kwargs, cron_expression="0 4 * * *", description="换一句措辞"
    )
    assert changed == first
    assert len(storage.rows) == 1
    data = storage.rows[0].data
    assert (data.cron_expression, data.description) == ("0 4 * * *", "换一句措辞")
    # 改完的这份照样要先过真 `validate_schedule`，并提醒持有 timer 的那台来 reconcile
    assert (scheduler.validated, scheduler.notified) == (2, [first, first])
    assert await ensure_autodream_schedule(
        **kwargs, cron_expression="0 4 * * *", description="换一句措辞"
    ) is None


async def test_人关掉的调度不会被启动重新打开() -> None:
    """`enabled` 不在收敛的名单里：那是熔断，不是漂移。"""
    storage, scheduler = _FakeStorage(), _FakeScheduler()
    await ensure_autodream_schedule(**_reg_kwargs(storage, scheduler))
    row = storage.rows[0]
    storage.rows[0] = row.model_copy(
        update={"data": row.data.model_copy(update={"enabled": False})},
    )
    assert await ensure_autodream_schedule(**_reg_kwargs(storage, scheduler)) is None
    assert storage.rows[0].data.enabled is False


# --- 7. 托管服务的接线 ------------------------------------------------------


async def test_托管路径的每个会话都写进同一份日志(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """`ctx_for` 是这条链唯一的入口：它要是忘了挂 sink，AutoDream 就永远看不到事实。

    读的是 `app.state.extra_agent_tools`（框架存下来的就是 `service.tools` 那个闭包），
    所以这条测的是真接线，不是照着接线又写一遍的替身。
    """
    from helpdesk import service

    monkeypatch.setattr(service, "MEMORY_DIR", str(tmp_path))
    monkeypatch.setattr(service, "SERVICE_DB", str(tmp_path / "service.db"))
    monkeypatch.setattr(service, "WORKSPACE_DIR", str(tmp_path / "workspaces"))
    app = service.make_service_app(FakeIndex())

    names = {t.name for t in await app.state.extra_agent_tools("ops", "agent-1", "sess-a")}
    assert "run_autodream" in names  # 挂上了表 → 调度醒来看得见这个工具

    for session in ("sess-a", "sess-b"):
        await app.state.extra_agent_tools("ops", "agent-1", session)
        ctx = app.state.helpdesk_contexts[session]
        ctx.store.create(
            title="下单接口 401",
            description="点了就报 401",
            category=Category.INCIDENT,
            priority=Priority.P1,
        )
    ledger: Ledger = app.state.helpdesk_ledger
    rows = list(ledger.rows())
    assert [r.session_id for r in rows] == ["sess-a", "sess-b"]
    assert {r.kind for r in rows} == {"ticket.created"}
    # 两份会话簿，一份日志：跨会话的账不能跟着 session 一起没
    assert ledger.path == tmp_path / f"{service.USER_ID}.ledger.jsonl"
