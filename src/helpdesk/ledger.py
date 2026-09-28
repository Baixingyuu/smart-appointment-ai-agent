"""按用户追加的业务事件日志：AutoDream 的唯一事实源。

为什么要多这一份：托管路径的工单簿和预约簿都是**按 session 现造**的
（`service.py:ctx_for`），一场对话结束它就跟着没了 —— 后台任务想跨会话看"这个人
历来约上午还是下午"，没有地方可读。框架持久化的是会话与 `AgentState`，不是业务事实。
所以分层是这样切的：短期上下文归框架，跨会话的结论归 `memory.py`，而中间这一层
"发生过什么"归这里的 JSONL。检查点只是一个行号，重放就是从这个号往后读。

三条不显然的约束：

1. **seq 是行号，不是时间戳。** 检查点要和"读到哪了"一一对应，时间戳做不到
   （同一毫秒两行、时钟回拨），行号可以。代价是追加必须原子：`open("a")` 一行
   一次 `write` + `flush`，单机进程内够用。
2. **只追加，不改写。** 派生结论一旦能从原始行重算出来，日志本身就不需要修正；
   要修的是折叠规则（`autodream.fold`），不是事实源。
3. **末行可能是半行。** 进程在 `write` 中间被杀，最后一行就是坏 JSON。这里只把
   **最后一行**当断尾丢掉，中间坏行直接抛 —— 悄悄跳过损坏的历史会让检查结果
   看起来比实际少，而那正是"模型说的话没进偏好"这类怪事的源头。
"""
from __future__ import annotations

import fcntl
import json
import os
from collections.abc import Callable
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import TYPE_CHECKING, Any, Iterator

if TYPE_CHECKING:
    from .appointment import Appointment, BookingEvent
    from .domain import Progress, Ticket


@dataclass(frozen=True)
class Row:
    """一行事实。`seq` 从 1 开始，等于这行在文件里的行号。"""

    seq: int
    at: datetime
    user_id: str
    session_id: str
    kind: str
    subject: str
    detail: dict[str, Any]


class Ledger:
    """一个用户一份 JSONL。`path` 的父目录自动建，测试用 `tmp_path` 指进来。"""

    def __init__(self, path: str | os.PathLike[str], *, user_id: str, session_id: str = "") -> None:
        self.path = Path(path)
        self.user_id = user_id
        self.session_id = session_id
        self._next = 0

    def with_session(self, session_id: str) -> "Ledger":
        """同一个文件换一条会话的视角。托管路径每个 session 一份 ctx，日志共用一份。"""
        clone = Ledger(self.path, user_id=self.user_id, session_id=session_id)
        clone._next = self._next
        return clone

    @property
    def next_seq(self) -> int:
        """下一条追加会拿到的行号 = 已有行数 + 1。"""
        if not self._next:
            self._next = sum(1 for _ in self._lines()) + 1
        return self._next

    def append(
        self,
        kind: str,
        subject: str,
        detail: dict[str, Any] | None = None,
        *,
        at: datetime | None = None,
    ) -> Row:
        """追加一行并返回它。

        返回里的 `seq` 是**这个写入视角**下的行号；同一份日志被多条会话同时追加时
        它会偏一位，权威行号在回放时按物理行位置重算 —— 检查点读的是回放的那个，
        所以偏了也不影响折叠结果。
        """
        seq = self.next_seq
        self.path.parent.mkdir(parents=True, exist_ok=True)
        stamp = at or datetime.now()
        payload = {
            "at": stamp.isoformat(timespec="seconds"),
            "user": self.user_id,
            "session": self.session_id,
            "kind": kind,
            "subject": str(subject),
            "detail": detail or {},
        }
        with open(self.path, "a", encoding="utf-8") as f:
            f.write(json.dumps(payload, ensure_ascii=False) + "\n")
            f.flush()
            os.fsync(f.fileno())
        self._next = seq + 1
        return Row(
            seq=seq,
            at=stamp,
            user_id=self.user_id,
            session_id=self.session_id,
            kind=kind,
            subject=str(subject),
            detail=payload["detail"],
        )

    def rows(self, *, since: int = 0) -> Iterator[Row]:
        """回放 `since` 之后的行。`since` 就是检查点，0 表示从头。"""
        for seq, payload in enumerate(self._lines(), start=1):
            if seq <= since:
                continue
            yield Row(
                seq=seq,
                at=datetime.fromisoformat(payload["at"]),
                user_id=payload.get("user", self.user_id),
                session_id=payload.get("session", ""),
                kind=payload["kind"],
                subject=str(payload.get("subject", "")),
                detail=payload.get("detail") or {},
            )

    def tail_seq(self) -> int:
        """当前已落盘的行数。跑完一轮就把检查点推到它。"""
        return max(0, self.next_seq - 1)

    def _lines(self) -> Iterator[dict[str, Any]]:
        if not self.path.exists():
            return
        with open(self.path, encoding="utf-8") as f:
            lines = f.read().splitlines()
        for i, line in enumerate(lines):
            try:
                yield json.loads(line)
            except json.JSONDecodeError as exc:
                if i == len(lines) - 1:
                    return  # 断尾：进程写到一半被杀，下次追加会盖过它
                raise ValueError(f"日志第 {i + 1} 行坏了：{self.path}") from exc

    def lock(self, *, holder: str = "") -> "_FileLock":
        """跨进程互斥。AutoDream 的"同一时刻只有一台在折叠"就靠它。"""
        return _FileLock(self.path.with_suffix(self.path.suffix + ".lock"), holder=holder)


class _FileLock:
    """`flock` 的上下文包装。拿不到就是拿不到，不自旋、不排队 —— 后台任务错过一轮
    还有下一轮 cron，抢成一串队反而会让同一批行被折叠多次。"""

    def __init__(self, path: Path, *, holder: str = "") -> None:
        self.path = path
        self.holder = holder
        self._fh: Any = None

    def acquire_nowait(self) -> bool:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = open(self.path, "w", encoding="utf-8")
        try:
            fcntl.flock(self._fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self._fh.close()
            self._fh = None
            return False
        self._fh.write(self.holder or str(os.getpid()))
        self._fh.flush()
        return True

    def release(self) -> None:
        if self._fh is not None:
            fcntl.flock(self._fh.fileno(), fcntl.LOCK_UN)
            self._fh.close()
            self._fh = None


def ticket_sink(ledger: Ledger) -> Callable[[Progress, Ticket], None]:
    """`TicketStore.sink` 的形状：进展行 → 日志行。

    `missing` 是工单当时的快照，而这个字段建单之后不再改（只有 `create` 写它），
    所以同一张单的每一条进展行都带着同一份缺失。折叠时只认 `ticket.created`
    那一行 —— 不是因为只有那里读得到，是因为读五条会把一次缺失算成五遍。
    """

    def emit(progress: Progress, ticket: Ticket) -> None:
        ledger.append(
            f"ticket.{progress.kind.value}",
            ticket.id,
            {
                "category": ticket.category.value,
                "priority": ticket.priority.value,
                "status": ticket.status.value,
                "assignee_id": ticket.assignee_id,
                "missing": list(ticket.missing_info),
            },
            at=None,
        )

    return emit


def appointment_sink(ledger: Ledger) -> Callable[[BookingEvent, Appointment], None]:
    """`AppointmentStore.sink` 的形状：变更日志行 → 日志行（带上结构化时刻）。

    为什么不在折叠时解析 `BookingEvent.detail` 那段中文：那是给人看的渲染，
    措辞一改结论就漂。时刻/时长/人这三个字段是域里的真字段，直接落。
    """

    def emit(event: BookingEvent, appt: Appointment) -> None:
        ledger.append(
            f"appointment.{event.kind}",
            event.appointment_id,
            {
                "engineer_id": appt.engineer_id,
                "start": appt.start.isoformat(timespec="minutes"),
                "duration_minutes": appt.duration_minutes,
                "service_id": appt.service_id,
                "ticket_id": appt.ticket_id,
                "resolved": appt.resolved,
            },
            at=event.at,
        )

    return emit


__all__ = ["Ledger", "Row", "appointment_sink", "ticket_sink"]
