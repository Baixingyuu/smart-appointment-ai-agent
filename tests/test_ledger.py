"""事件日志（AutoDream 的事实源）与两处写穿接缝。全离线，一次模型都不调。

为什么要有这份日志：托管路径的工单簿和预约簿都是按 session 现造的
（`service.ctx_for`），会话一结束就跟着没了 —— 而"这个人历来约上午还是下午"
是跨会话的问题。框架持久化的是会话与 `AgentState`，不是业务事实，所以中间这层
"发生过什么"必须自己留一份追加式记录，结论才能重算。

这里钉的是它的三条契约：行号即检查点、只追加不改写、坏行只丢尾巴。
第四条是写穿的两处接缝：簿子里每一条进展/变更都要在日志里有一行，一一对应。
"""
from __future__ import annotations

import json
import sys
from datetime import datetime
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from helpdesk.appointment import AppointmentStore  # noqa: E402
from helpdesk.dispatch import AssignmentDecision  # noqa: E402
from helpdesk.domain import Category, Priority, ProgressKind  # noqa: E402
from helpdesk.ledger import Ledger, appointment_sink, ticket_sink  # noqa: E402
from helpdesk.ticket_store import TicketStore  # noqa: E402

NOW = datetime(2026, 10, 8, 8, 0)


def _ledger(tmp_path: Path, *, session: str = "s1") -> Ledger:
    return Ledger(tmp_path / "ops.ledger.jsonl", user_id="ops", session_id=session)


def _kinds(ledger: Ledger) -> list[str]:
    return [r.kind for r in ledger.rows()]


# --- 1. 行号与检查点 --------------------------------------------------------


def test_空日志没有行也没有尾(tmp_path: Path) -> None:
    ledger = _ledger(tmp_path)
    assert list(ledger.rows()) == []
    assert ledger.tail_seq() == 0
    assert not ledger.path.exists()  # 没写过就不该造文件


def test_行号从1开始连续_检查点就是它(tmp_path: Path) -> None:
    ledger = _ledger(tmp_path)
    for i in range(3):
        row = ledger.append("ticket.created", i + 1, {"i": i})
        assert row.seq == i + 1
    assert [r.seq for r in ledger.rows()] == [1, 2, 3]
    assert [r.seq for r in ledger.rows(since=2)] == [3]
    assert ledger.tail_seq() == 3


def test_回放里的行带着写它的那次会话(tmp_path: Path) -> None:
    """闸门数的是"新会话满几次"，所以每一行必须说得出自己是谁写的。"""
    ledger = _ledger(tmp_path)
    ledger.append("ticket.created", 1)
    ledger.with_session("s2").append("ticket.created", 2)
    ledger.with_session("s3").append("ticket.created", 3)
    assert [r.session_id for r in ledger.rows()] == ["s1", "s2", "s3"]


def test_重开一份日志接着数行号(tmp_path: Path) -> None:
    """`_next` 是懒算的行数，进程重启后不会从头写起、把检查点后面的历史盖掉。"""
    first = _ledger(tmp_path)
    first.append("ticket.created", 1)
    again = _ledger(tmp_path)
    assert again.next_seq == 2
    assert again.append("ticket.created", 2).seq == 2


def test_落盘的每一行是完整json(tmp_path: Path) -> None:
    ledger = _ledger(tmp_path)
    ledger.append("appointment.booked", 7, {"start": "2026-10-12 10:00"}, at=NOW)
    payload = json.loads(ledger.path.read_text(encoding="utf-8").splitlines()[0])
    assert payload == {
        "at": "2026-10-08T08:00:00",
        "user": "ops",
        "session": "s1",
        "kind": "appointment.booked",
        "subject": "7",
        "detail": {"start": "2026-10-12 10:00"},
    }


# --- 2. 坏行 ---------------------------------------------------------------


def test_尾巴上的半行丢掉不响(tmp_path: Path) -> None:
    """进程在 write 中间被杀：最后一行是坏 JSON，下次追加会盖过它。"""
    ledger = _ledger(tmp_path)
    ledger.append("ticket.created", 1)
    with open(ledger.path, "a", encoding="utf-8") as f:
        f.write('{"at": "2026-10-08T08:00:00", "kin')
    assert _kinds(ledger) == ["ticket.created"]


def test_中间坏行当场响(tmp_path: Path) -> None:
    """悄悄跳过损坏的历史 = 结论凭空少几行，而那正是"模型说的话没进偏好"的源头。"""
    ledger = _ledger(tmp_path)
    ledger.append("ticket.created", 1)
    ledger.append("ticket.created", 2)
    lines = ledger.path.read_text(encoding="utf-8").splitlines()
    ledger.path.write_text("\n".join([lines[0], "not json", lines[1]]) + "\n", encoding="utf-8")
    with pytest.raises(ValueError, match="第 2 行"):
        list(ledger.rows())


# --- 3. 任务锁 -------------------------------------------------------------


def test_同一时刻只有一台在折叠(tmp_path: Path) -> None:
    """拿不到锁就跳过这一轮：cron 还会再来，排队反而会让同一批行折两次。"""
    ledger = _ledger(tmp_path)
    held = ledger.lock(holder="a")
    other = ledger.lock(holder="b")
    try:
        assert held.acquire_nowait() is True
        assert other.acquire_nowait() is False
    finally:
        held.release()
    assert other.acquire_nowait() is True
    other.release()


def test_释放幂等_重复释放不炸(tmp_path: Path) -> None:
    """`run()` 的 finally 里无条件 release；没拿到锁时也要能安全收尾。"""
    ledger = _ledger(tmp_path)
    lock = ledger.lock()
    assert lock.acquire_nowait() is True
    lock.release()
    lock.release()
    lost = ledger.lock()
    lost.release()  # 没持有时 release 不该碰一个已关闭的 fd


# --- 4. 写穿：工单簿 -------------------------------------------------------


def _ticket_store(ledger: Ledger) -> TicketStore:
    return TicketStore(sink=ticket_sink(ledger))


def test_工单每一条进展在日志里都有一行(tmp_path: Path) -> None:
    """一一对应，不是"挑几条重要的写"：漏一条，折叠出来的分母就是假的。"""
    ledger = _ledger(tmp_path)
    store = _ticket_store(ledger)
    created = store.create(
        title="下单接口一直 401",
        description="点了就报 401，重试也一样",
        category=Category.INCIDENT,
        priority=Priority.P1,
        missing_info=("affected_system",),
    )
    tid = created.ticket.id
    store.assign(tid, AssignmentDecision(assignee="101", rationale="服务主责", confidence=0.8))
    store.accept(tid)
    store.comment(tid, "已让用户重装证书")
    store.resolve(tid)
    assert _kinds(ledger) == [f"ticket.{p.kind.value}" for p in store.progress]
    assert [r.subject for r in ledger.rows()] == [str(tid)] * 5


def test_缺失槽位是每条进展行都带着的快照(tmp_path: Path) -> None:
    """`missing_info` 建单之后不再改，所以每条进展行都重复同一份缺失。

    这不是冗余可以修掉：日志只追加不改写，一行得自己讲清当时的事实。
    去重的责任在折叠那一侧（`memory.fold` 只认 `ticket.created`）。
    """
    ledger = _ledger(tmp_path)
    store = _ticket_store(ledger)
    created = store.create(
        title="VPN 连不上",
        description="客户端起不来",
        category=Category.INCIDENT,
        priority=Priority.P2,
        missing_info=("affected_system", "planned_window"),
    )
    store.comment(created.ticket.id, "补充：是 macOS 客户端")
    rows = list(ledger.rows())
    assert [r.detail["missing"] for r in rows] == [
        ["affected_system", "planned_window"],
        ["affected_system", "planned_window"],
    ]


def test_没挂sink的簿子照旧写(tmp_path: Path) -> None:
    """单测/评测/console 都是这一路：不落盘，也就不会去碰别人的文件。"""
    store = TicketStore()
    store.create(
        title="打印队列卡住",
        description="全层的打印机都没反应",
        category=Category.INCIDENT,
        priority=Priority.P3,
    )
    assert not (tmp_path / "ops.ledger.jsonl").exists()


# --- 5. 写穿：预约簿 -------------------------------------------------------


def _appt_store(ledger: Ledger) -> AppointmentStore:
    return AppointmentStore(sink=appointment_sink(ledger))


def test_预约的每一步都落一行带结构化时刻(tmp_path: Path) -> None:
    ledger = _ledger(tmp_path)
    store = _appt_store(ledger)
    appt = store.book(
        engineer_id=101,
        start=datetime(2026, 10, 12, 10, 0),
        duration_minutes=60,
        need="现场排查下单接口 401",
        service_id=2001,
        now=NOW,
    )
    store.reschedule(appt.id, start=datetime(2026, 10, 13, 15, 0), now=NOW)
    store.visit(appt.id, resolved=True, followup_note="换了令牌，已验证", now=NOW)
    rows = list(ledger.rows())
    assert [r.kind for r in rows] == [
        "appointment.booked",
        "appointment.rescheduled",
        "appointment.visited",
    ]
    booked = rows[0].detail
    assert booked["engineer_id"] == 101
    assert booked["start"] == "2026-10-12T10:00"
    assert booked["duration_minutes"] == 60
    assert booked["service_id"] == 2001
    assert booked["resolved"] is None
    assert rows[1].detail["start"] == "2026-10-13T15:00"
    assert rows[2].detail["resolved"] is True


def test_取消也有一行(tmp_path: Path) -> None:
    ledger = _ledger(tmp_path)
    store = _appt_store(ledger)
    appt = store.book(
        engineer_id=101,
        start=datetime(2026, 10, 12, 10, 0),
        duration_minutes=60,
        need="现场排查",
        now=NOW,
    )
    store.cancel(appt.id, reason="用户改线上", now=NOW)
    assert _kinds(ledger) == ["appointment.booked", "appointment.cancelled"]


def test_预约写穿用的是变更时刻不是现在(tmp_path: Path) -> None:
    """`at` 取 `BookingEvent.at`：回放历史时"哪天约的"必须是当时那个时刻。"""
    ledger = _ledger(tmp_path)
    store = _appt_store(ledger)
    appt = store.book(
        engineer_id=101,
        start=datetime(2026, 10, 12, 10, 0),
        duration_minutes=60,
        need="现场排查",
        now=datetime(2026, 9, 1, 9, 30),
    )
    assert list(ledger.rows())[0].at == datetime(2026, 9, 1, 9, 30)
    assert appt.start == datetime(2026, 10, 12, 10, 0)
