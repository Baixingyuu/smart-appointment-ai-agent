"""预约聚合：上门时段、工程师可用性、预约生命周期与变更日志。

为什么单独立一个聚合而不是给工单加一个类别：预约占的是"人和时间"，工单占的是"责任"。
同一条 P0 可以既派了人又约了上门，两边的状态互不蕴含（派了人不代表到访，到访了不代表结单）。

负载这条线在这里定案：名册里的 `current_load` 是运营基线（快照），预约占用是运行期事实，
两者相加才回答"这个人现在还能不能接"。所以派单侧也必须读同一个带占用的名册视图，
否则同一份事实在派单和预约两边各有一个答案 —— 那正是派单 v4 挂着没裁决的坑。

时区：全部按本地墙钟比较，不落 UTC 偏移。上门是对人说的事，"下周二下午三点"就是那个时刻。
"""
from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field, replace
from datetime import datetime, timedelta
from enum import StrEnum

from .catalog import TEAMS, Employee, employees, services_by_id, team_name
from .domain import ExtractedSlot

#: 上门时长只有这三档 —— 排班是按档排的，接受任意分钟数就等于伪造精度。
DURATION_CHOICES = (30, 60, 120)
PROPOSAL_LIMIT = 3
STEP_MINUTES = 30

BLOCKING_SLOTS = ("visit_time", "visit_duration")
OPTIONAL_SLOTS = ("service_preference",)

#: 问句的措辞只有一张表：`domain._SLOT_LABELS`。这里再写一份，就会被两份各说一遍 ——
#: 追问走的是 ask_user，它念的是那张表，不是这张。


class AppointmentStatus(StrEnum):
    BOOKED = "booked"
    VISITED = "visited"
    CANCELLED = "cancelled"
    MISSED = "missed"


class InvalidBooking(ValueError):
    pass


def missing_appointment_slots(extracted: tuple[ExtractedSlot, ...]) -> tuple[str, ...]:
    filled = {s.name for s in extracted if s.quote.strip()}
    return tuple(s for s in BLOCKING_SLOTS if s not in filled)


def on_site_windows(emp: Employee) -> tuple[tuple[int, int], ...]:
    return next(t.on_site for t in TEAMS if t.id == emp.team_id)


def with_overlay(base: tuple[Employee, ...], overlay: dict[int, int]) -> tuple[Employee, ...]:
    """把运行期占用加到运营基线上。离职者不叠 —— 他不接任何活。"""
    return tuple(
        replace(e, current_load=e.current_load + overlay[e.id])
        if e.active and overlay.get(e.id)
        else e
        for e in base
    )


@dataclass(frozen=True)
class Opening:
    """一个具体可约的格子：谁、什么时候、多长。"""

    engineer: Employee
    start: datetime
    duration_minutes: int
    reasons: tuple[str, ...] = ()

    @property
    def end(self) -> datetime:
        return self.start + timedelta(minutes=self.duration_minutes)

    @property
    def key(self) -> tuple[int, datetime, int]:
        return (self.engineer.id, self.start, self.duration_minutes)


@dataclass
class Appointment:
    id: int
    engineer_id: int
    start: datetime
    duration_minutes: int
    need: str
    status: AppointmentStatus = AppointmentStatus.BOOKED
    service_id: int | None = None
    ticket_id: int | None = None
    conversation_id: str | None = None
    missing_info: tuple[str, ...] = ()
    followup_note: str = ""
    #: None = 还没上门。回访结论要能直接被读，不该去反推变更日志的措辞。
    resolved: bool | None = None

    def validate(self) -> None:
        if self.duration_minutes not in DURATION_CHOICES:
            raise InvalidBooking(f"上门时长只能是 {DURATION_CHOICES} 分钟")
        if not self.need.strip():
            raise InvalidBooking("预约必须写清这次上门要做什么")

    @property
    def end(self) -> datetime:
        return self.start + timedelta(minutes=self.duration_minutes)

    @property
    def occupies(self) -> bool:
        """只有未履约的预约还占着人；visited/cancelled/missed 都放行。"""
        return self.status is AppointmentStatus.BOOKED

    @property
    def text(self) -> str:
        svc = "" if self.service_id is None else f"（服务 {self.service_id}）"
        return (
            f"预约 #{self.id} {self.start:%m-%d %H:%M}–{self.end:%H:%M} "
            f"上门 {self.duration_minutes} 分钟{svc}：{self.need}"
        )


@dataclass(frozen=True)
class BookingEvent:
    """变更日志一行。`detail` 是给人看的措辞，不是结论的依据 ——
    AutoDream 折的是它的写穿镜像（`ledger.appointment_sink`），那里落的是结构化字段。"""

    appointment_id: int
    kind: str
    at: datetime
    detail: str


# ---- 时段计算 -------------------------------------------------------------


def _covers(start: datetime, end: datetime, windows: tuple[tuple[int, int], ...]) -> bool:
    """整段必须落在同一天的同一个窗口里：跨天上门没有排班可言。"""
    if start.date() != end.date():
        return False
    return any(start.hour >= lo and end <= start.replace(hour=hi, minute=0) for lo, hi in windows)


def _ceil_to(value: datetime, step_minutes: int) -> datetime:
    floor = value.replace(second=0, microsecond=0)
    remainder = (floor.hour * 60 + floor.minute) % step_minutes
    return floor if remainder == 0 else floor + timedelta(minutes=step_minutes - remainder)


def _crosses(a_start: datetime, a_end: datetime, b_start: datetime, b_end: datetime) -> bool:
    # 背靠背允许：上一单的收工时刻等于下一单的开工时刻不算冲突。
    return a_start < b_end and b_start < a_end


def _why(emp: Employee, windows: tuple[tuple[int, int], ...]) -> tuple[str, ...]:
    reasons = [
        "上门窗口 " + "、".join(f"{a:02d}:00–{b:02d}:00" for a, b in windows),
        f"负载 {emp.current_load}/{emp.max_concurrent}",
    ]
    if emp.level.value == "senior":
        reasons.append("senior")
    return tuple(reasons)


def openings_for(
    emp: Employee,
    *,
    earliest: datetime,
    latest: datetime,
    duration_minutes: int,
    busy: tuple[tuple[datetime, datetime], ...] = (),
    step_minutes: int = STEP_MINUTES,
    limit: int = 2,
) -> tuple[Opening, ...]:
    """一个人从 earliest 到 latest 站得下的格子。只有窗口和冲突两道闸，没有打分。"""
    windows = on_site_windows(emp)
    if not windows or not emp.active:
        return ()
    found: list[Opening] = []
    cursor = _ceil_to(earliest, step_minutes)
    while cursor + timedelta(minutes=duration_minutes) <= latest:
        end = cursor + timedelta(minutes=duration_minutes)
        if _covers(cursor, end, windows) and not any(
            _crosses(cursor, end, b_start, b_end) for b_start, b_end in busy
        ):
            found.append(Opening(emp, cursor, duration_minutes, _why(emp, windows)))
            if len(found) >= limit:
                return tuple(found)
            cursor = end
        else:
            cursor = cursor + timedelta(minutes=step_minutes)
    return tuple(found)


# ---- 存储 -------------------------------------------------------------


@dataclass
class AppointmentStore:
    """进程内的预约簿 + 变更日志。与 TicketStore 同构：业务事实归这里，会话状态归框架。"""

    appointments: dict[int, Appointment] = field(default_factory=dict)
    changelog: list[BookingEvent] = field(default_factory=list)
    roster: tuple[Employee, ...] = field(default_factory=employees)
    #: 写穿：每追加一行变更日志就镜像一份到按用户的持久事实源（`ledger.Ledger`）。
    #: 簿是跟着会话死的，AutoDream 读的东西不能跟着它一起没。
    sink: Callable[[BookingEvent, "Appointment"], None] | None = None
    _next_id: int = 1

    # ---- 占用与视图 ----
    def occupancy(self) -> dict[int, int]:
        counts: dict[int, int] = {}
        for appt in self.appointments.values():
            if appt.occupies:
                counts[appt.engineer_id] = counts.get(appt.engineer_id, 0) + 1
        return counts

    def staff(self) -> tuple[Employee, ...]:
        """带运行期占用的名册视图 —— 派单与预约必须读同一个视图。"""
        return with_overlay(self.roster, self.occupancy())

    def by_id(self, engineer_id: int) -> Employee | None:
        return next((e for e in self.roster if e.id == engineer_id), None)

    def busy(self, engineer_id: int, *, ignore_id: int | None = None) -> tuple[tuple[datetime, datetime], ...]:
        return tuple(
            (a.start, a.end)
            for a in self.appointments.values()
            if a.occupies and a.engineer_id == engineer_id and a.id != ignore_id
        )

    def get(self, appointment_id: int) -> Appointment:
        try:
            return self.appointments[appointment_id]
        except KeyError:
            raise InvalidBooking(f"没有预约 #{appointment_id}") from None

    def upcoming(self, *, now: datetime, within_days: int = 7) -> tuple[Appointment, ...]:
        horizon = now + timedelta(days=within_days)
        return tuple(sorted((a for a in self.appointments.values() if a.occupies and a.start <= horizon), key=lambda a: a.start))

    # ---- 提案 ----
    def propose(
        self,
        *,
        need: str,
        earliest: datetime,
        latest: datetime,
        duration_minutes: int = 60,
        service_id: int | None = None,
        engineer_ids: tuple[int, ...] = (),
        now: datetime | None = None,
        per_person: int = 1,
    ) -> tuple[Opening, ...]:
        if duration_minutes not in DURATION_CHOICES:
            raise InvalidBooking(
                f"上门时长只能是 {'/'.join(str(d) for d in DURATION_CHOICES)} 分钟，不是 {duration_minutes}"
            )
        floor = max(earliest, now or earliest)
        if latest <= floor:
            raise InvalidBooking("最晚时间不晚于最早时间，没有可搜索的窗口")
        pool = self._candidates(service_id=service_id, engineer_ids=engineer_ids)
        found: list[Opening] = []
        for emp in pool:
            # 不再减 busy()：pool 来自 staff()，占用已经叠在 current_load 里了。
            # 这里多减一道会让"还有没有余量"在提案和写入两道闸上给出不同答案。
            room = emp.max_concurrent - emp.current_load
            if room <= 0:
                continue
            found.extend(
                openings_for(
                    emp,
                    earliest=floor,
                    latest=latest,
                    duration_minutes=duration_minutes,
                    busy=self.busy(emp.id),
                    limit=per_person,
                )
            )
        found.sort(key=lambda o: (o.start, -_skill_rank(o.engineer)))
        return tuple(found[:PROPOSAL_LIMIT])

    def _candidates(
        self,
        *,
        service_id: int | None,
        engineer_ids: tuple[int, ...],
    ) -> list[Employee]:
        """候选池 = 指定人 ∪（有服务时）该服务的 owner/backup；都空则全名册。
        排序按名册顺序，owner/backup 提到最前 —— 只有顺序，没有分数。"""
        staff = list(self.staff())
        if engineer_ids:
            picked = [e for e in staff if e.id in engineer_ids]
            if picked:
                return picked
        if service_id is not None:
            svc = services_by_id().get(service_id)
            if svc is not None:
                owners = {svc.owner_id, svc.backup_owner_id} - {None}
                staff.sort(key=lambda e: 0 if e.id in owners else 1)
        return staff

    # ---- 写入 ----
    def book(
        self,
        *,
        engineer_id: int,
        start: datetime,
        duration_minutes: int,
        need: str,
        service_id: int | None = None,
        ticket_id: int | None = None,
        conversation_id: str | None = None,
        missing_info: tuple[str, ...] = (),
        now: datetime | None = None,
    ) -> Appointment:
        emp = self.by_id(engineer_id)
        if emp is None:
            raise InvalidBooking(f"{engineer_id} 不在名册里")
        if not emp.active:
            raise InvalidBooking(f"{emp.name} 已离职，不能承接上门")
        duplicate = self.same_slot(engineer_id, start, duration_minutes)
        if duplicate is not None:
            # 必须排在冲突闸之前：否则这条会被"与自己时段冲突"挡下，
            # 确认闸之后模型再调一次就变成了报错。
            self._log(duplicate.id, "book-idempotent", f"重复提交命中已有预约：{duplicate.text}", now)
            return duplicate
        appt = Appointment(
            id=0,
            engineer_id=engineer_id,
            start=start,
            duration_minutes=duration_minutes,
            need=need,
            service_id=service_id,
            ticket_id=ticket_id,
            conversation_id=conversation_id,
            missing_info=missing_info,
        )
        appt.validate()
        if now is not None and start < now:
            raise InvalidBooking(f"{start:%m-%d %H:%M} 已经过去，不能预约到过去")
        self._assert_placeable(appt)
        appt.id = self._next_id
        self._next_id += 1
        self.appointments[appt.id] = appt
        self._log(appt.id, "booked", appt.text, now)
        return appt

    def reschedule(
        self,
        appointment_id: int,
        *,
        start: datetime,
        duration_minutes: int | None = None,
        engineer_id: int | None = None,
        now: datetime | None = None,
    ) -> Appointment:
        """改期在同一把闸里重算窗口与冲突。不做"先取消再重订"：
        那会在两步之间留下一个既不占人也不算取消的空洞，回滚无路。"""
        appt = self.get(appointment_id)
        if not appt.occupies:
            raise InvalidBooking(f"{appt.status.value} 的预约不能改期")
        moved = replace(
            appt,
            start=start,
            duration_minutes=duration_minutes if duration_minutes is not None else appt.duration_minutes,
            engineer_id=engineer_id if engineer_id is not None else appt.engineer_id,
        )
        moved.validate()
        if now is not None and start < now:
            raise InvalidBooking(f"{start:%m-%d %H:%M} 已经过去，不能改到过去")
        self._assert_placeable(moved, ignore_id=appt.id)
        detail = f"{appt.text} → {start:%m-%d %H:%M}"
        appt.start = moved.start
        appt.duration_minutes = moved.duration_minutes
        appt.engineer_id = moved.engineer_id
        self._log(appt.id, "rescheduled", detail, now)
        return appt

    def visit(
        self,
        appointment_id: int,
        *,
        resolved: bool,
        followup_note: str,
        now: datetime | None = None,
    ) -> Appointment:
        """到场登记 —— 回访记录长在这里，因为它就是这一趟的结果事实，
        也是行为分析 Agent 唯一能读到的"这次上门到底有没有解决问题"。"""
        appt = self.get(appointment_id)
        if not appt.occupies:
            raise InvalidBooking(f"{appt.status.value} 的预约不能重复登记到场")
        if not followup_note.strip():
            raise InvalidBooking("回访记录不能为空：这一趟解决了没有，得留下字")
        appt.status = AppointmentStatus.VISITED
        appt.followup_note = followup_note.strip()
        appt.resolved = resolved
        self._log(
            appt.id,
            "visited",
            f"{appt.text}｜{'已解决' if resolved else '未解决，需后续'}｜回访：{appt.followup_note}",
            now,
        )
        return appt

    def cancel(self, appointment_id: int, *, reason: str, now: datetime | None = None) -> Appointment:
        appt = self.get(appointment_id)
        if not appt.occupies:
            raise InvalidBooking(f"{appt.status.value} 的预约不能再取消")
        appt.status = AppointmentStatus.CANCELLED
        self._log(appt.id, "cancelled", f"{appt.text}｜取消原因：{reason}", now)
        return appt

    # ---- 闸 ----
    def _assert_placeable(
        self,
        appt: Appointment,
        *,
        ignore_id: int | None = None,
    ) -> None:
        emp = self.by_id(appt.engineer_id)
        if emp is None:
            raise InvalidBooking(f"{appt.engineer_id} 不在名册里")
        windows = on_site_windows(emp)
        if not windows:
            raise InvalidBooking(f"{team_name(emp.team_id)}不承接上门服务，{emp.name} 没有可约窗口")
        if not _covers(appt.start, appt.end, windows):
            span = "、".join(f"{a:02d}:00–{b:02d}:00" for a, b in windows)
            raise InvalidBooking(
                f"{appt.start:%m-%d %H:%M}–{appt.end:%H:%M} 不在 {emp.name} 的上门窗口内（{span}）"
            )
        for other in self.appointments.values():
            if not other.occupies or other.id == ignore_id or other.engineer_id != appt.engineer_id:
                continue
            if _crosses(appt.start, appt.end, other.start, other.end):
                raise InvalidBooking(
                    f"{emp.name} 在 {other.start:%m-%d %H:%M} 已有预约 #{other.id}，时段冲突"
                )
        busy = len(self.busy(appt.engineer_id, ignore_id=ignore_id))
        if emp.max_concurrent - emp.current_load - busy <= 0:
            raise InvalidBooking(
                f"{emp.name} 基线负载 {emp.current_load} + 已有占用 {busy}，"
                f"到上限 {emp.max_concurrent}，不能再约"
            )

    def same_slot(
        self,
        engineer_id: int,
        start: datetime,
        duration_minutes: int,
    ) -> Appointment | None:
        """同一 (人, 时刻, 时长) 的已有预约。工具层先查它，才能把"重复提交"如实报出来。"""
        return next(
            (
                a
                for a in self.appointments.values()
                if a.occupies
                and a.engineer_id == engineer_id
                and a.start == start
                and a.duration_minutes == duration_minutes
            ),
            None,
        )

    def _log(self, appointment_id: int, kind: str, detail: str, at: datetime | None) -> None:
        event = BookingEvent(
            appointment_id=appointment_id, kind=kind, at=at or datetime.now(), detail=detail
        )
        self.changelog.append(event)
        if self.sink is not None:
            self.sink(event, self.appointments[appointment_id])


# ---- 渲染 ----


def _skill_rank(emp: Employee) -> int:
    return {"senior": 2, "mid": 1, "junior": 0}[emp.level.value]


def render_openings(openings: tuple[Opening, ...]) -> str:
    if not openings:
        return "窗口内没有可约时段。"
    return "\n".join(
        f"- {o.start:%Y-%m-%d %H:%M}–{o.end:%H:%M} 员工 {o.engineer.id} {o.engineer.name}"
        f"（{team_name(o.engineer.team_id)}/{o.engineer.level.value}）{'；'.join(o.reasons)}"
        for o in openings
    )


def render_appointment(appt: Appointment) -> str:
    emp = next((e for e in employees() if e.id == appt.engineer_id), None)
    who = str(appt.engineer_id) if emp is None else f"{emp.id} {emp.name}"
    lines = [appt.text, f"状态：{appt.status.value}｜工程师：{who}"]
    if appt.ticket_id is not None:
        lines.append(f"关联工单：#{appt.ticket_id}")
    if appt.missing_info:
        lines.append("缺失槽位：" + "、".join(appt.missing_info))
    if appt.followup_note:
        lines.append(f"回访：{appt.followup_note}")
    return "\n".join(lines)


def booking_prompt(
    *,
    need: str,
    earliest: datetime,
    latest: datetime,
    duration_minutes: int,
    openings: tuple[Opening, ...],
) -> str:
    return (
        f"上门需求：{need}\n"
        f"期望窗口：{earliest:%Y-%m-%d %H:%M} 至 {latest:%Y-%m-%d %H:%M}，"
        f"每次 {duration_minutes} 分钟\n\n"
        f"【可约时段（{len(openings)} 个，按时间先后）】\n{render_openings(openings)}\n\n"
        "负载=基线+预约占用。把时段念给用户挑，不要替他定时间；"
        "用户认可之后才调 book_appointment。"
    )
