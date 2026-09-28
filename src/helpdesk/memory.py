"""长期记忆：把事件日志折成"这个人历来怎么样"，以及它的读取口。

分层是这样切的（这决定了为什么这里只有计数没有原文）：
- 短期：一轮对话的上下文，归框架的 `AgentState` + `compress_context` 的滚动摘要；
- 长期：跨会话的**结论**，就是这里的计数；原文留在 `ledger.py` 的日志里，
  因为结论要能重算，重算的依据不能在折完之后丢掉。

`seq` 这个字段同时是检查点：它说"日志前 seq 行已经折进这些计数了"。
写文件用 临时文件 + `os.replace`，落不下去就不动原文件 —— 半份偏好表比没有
偏好表更糟，因为它看起来是有依据的。
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable

WEEKDAYS = ("周一", "周二", "周三", "周四", "周五", "周六", "周日")
MORNING, AFTERNOON, EVENING = "上午", "下午", "晚间"


def bucket_of(at: datetime) -> str:
    if at.hour < 12:
        return MORNING
    if at.hour < 18:
        return AFTERNOON
    return EVENING


def _bump(table: dict[str, int], key: str, by: int = 1) -> None:
    table[key] = table.get(key, 0) + by


def _top(table: dict[str, int]) -> tuple[str, int] | None:
    """按 (次数, 键) 取最大。键参与比较是为了同一份输入永远给出同一个答案 ——
    偏好表是要被评测断言的，不能今天选上午明天选下午。"""
    if not table:
        return None
    return max(table.items(), key=lambda kv: (kv[1], kv[0]))


def _plurality(table: dict[str, int]) -> tuple[str, int] | None:
    """只有**严格多数**才算偏好。并列时返回 None：五个不同星期各一次，
    "最常约在周 X"这句就没有依据，而模型会把它当成依据。"""
    top = _top(table)
    if top is None:
        return None
    counts = sorted(table.values(), reverse=True)
    if len(counts) > 1 and counts[1] == counts[0]:
        return None
    return top


@dataclass
class Counters:
    """折出来的原始计数。不直接给模型看，给模型看的是 `summarize` 的结果。"""

    user_id: str
    seq: int = 0
    last_run_at: datetime | None = None
    runs: int = 0
    weekdays: dict[str, int] = field(default_factory=dict)
    buckets: dict[str, int] = field(default_factory=dict)
    durations: dict[str, int] = field(default_factory=dict)
    engineers: dict[str, int] = field(default_factory=dict)
    outcomes: dict[str, int] = field(default_factory=dict)
    missing_slots: dict[str, int] = field(default_factory=dict)
    tickets: int = 0

    def to_json(self) -> dict[str, Any]:
        return {
            "user_id": self.user_id,
            "seq": self.seq,
            "last_run_at": self.last_run_at.isoformat() if self.last_run_at else None,
            "runs": self.runs,
            "weekdays": self.weekdays,
            "buckets": self.buckets,
            "durations": self.durations,
            "engineers": self.engineers,
            "outcomes": self.outcomes,
            "missing_slots": self.missing_slots,
            "tickets": self.tickets,
        }

    @classmethod
    def from_json(cls, payload: dict[str, Any]) -> "Counters":
        stamp = payload.get("last_run_at")
        return cls(
            user_id=payload["user_id"],
            seq=payload.get("seq", 0),
            last_run_at=datetime.fromisoformat(stamp) if stamp else None,
            runs=payload.get("runs", 0),
            weekdays=payload.get("weekdays", {}),
            buckets=payload.get("buckets", {}),
            durations=payload.get("durations", {}),
            engineers=payload.get("engineers", {}),
            outcomes=payload.get("outcomes", {}),
            missing_slots=payload.get("missing_slots", {}),
            tickets=payload.get("tickets", 0),
        )


@dataclass(frozen=True)
class Preferences:
    """给模型/评测读的派生视图。每个结论都带样本数，弱结论要看得出来是弱的。"""

    user_id: str
    bookings: int
    preferred_bucket: tuple[str, int] | None
    preferred_weekday: tuple[str, int] | None
    typical_duration: tuple[str, int] | None
    top_engineers: tuple[tuple[str, int], ...]
    completion_rate: float | None
    cancel_rate: float | None
    common_missing_slots: tuple[tuple[str, int], ...]
    tickets: int
    seq: int
    last_run_at: datetime | None

    @property
    def empty(self) -> bool:
        return self.bookings == 0 and self.tickets == 0

    def render(self) -> str:
        """中文一行式。样本数写在每个结论后面，让"三次里两次"不像"一贯如此"。"""
        if self.empty:
            return "没有历史记录（第一次接触这位用户）。别拿假设当偏好。"
        bits: list[str] = []
        if self.preferred_bucket:
            name, n = self.preferred_bucket
            bits.append(f"上门时段：{name} {n}/{self.bookings} 次")
        if self.preferred_weekday:
            name, n = self.preferred_weekday
            bits.append(f"最常约在{name}（{n} 次）")
        if self.typical_duration:
            mins, n = self.typical_duration
            bits.append(f"时长多为 {mins} 分钟（{n} 次）")
        if self.top_engineers:
            who = "、".join(f"{e}×{n}" for e, n in self.top_engineers)
            bits.append(f"常由 {who} 上门")
        if self.completion_rate is not None:
            bits.append(f"履约完成率 {self.completion_rate:.0%}")
        if self.cancel_rate is not None:
            bits.append(f"取消率 {self.cancel_rate:.0%}")
        if self.common_missing_slots:
            miss = "、".join(f"{s}×{n}" for s, n in self.common_missing_slots)
            bits.append(f"历史上最常缺的槽位：{miss}（可提前一次问全）")
        bits.append(f"依据：{self.tickets} 张工单 / {self.bookings} 次预约")
        return "\n".join(f"- {b}" for b in bits)


def summarize(counters: Counters) -> Preferences:
    done = counters.outcomes.get("completed", 0)
    unresolved = counters.outcomes.get("unresolved", 0)
    cancelled = counters.outcomes.get("cancelled", 0)
    visits = done + unresolved
    bookings = sum(counters.buckets.values())
    engineers = sorted(counters.engineers.items(), key=lambda kv: (-kv[1], kv[0]))
    missing = sorted(counters.missing_slots.items(), key=lambda kv: (-kv[1], kv[0]))
    return Preferences(
        user_id=counters.user_id,
        bookings=bookings,
        preferred_bucket=_plurality(counters.buckets),
        preferred_weekday=_plurality(counters.weekdays),
        typical_duration=_plurality(counters.durations),
        top_engineers=tuple(engineers[:2]),
        #: 分母是"真的上过门"的次数：约了还没上门的既不算完成也不算失败。
        completion_rate=(done / visits) if visits else None,
        #: 分母是约成的次数：取消掉的就是取消掉的，跟还挂着没履约的无关。
        cancel_rate=(cancelled / bookings) if bookings else None,
        common_missing_slots=tuple(missing[:2]),
        tickets=counters.tickets,
        seq=counters.seq,
        last_run_at=counters.last_run_at,
    )


class MemoryStore:
    """一个用户一份计数表。`load` 在没有文件时给空表，不抛。"""

    def __init__(self, path: str | os.PathLike[str], *, user_id: str) -> None:
        self.path = Path(path)
        self.user_id = user_id

    def load(self) -> Counters:
        if not self.path.exists():
            return Counters(user_id=self.user_id)
        with open(self.path, encoding="utf-8") as f:
            return Counters.from_json(json.load(f))

    def save(self, counters: Counters) -> None:
        """临时文件 + `os.replace`：读者要么看到旧的完整表，要么看到新的完整表。"""
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(counters.to_json(), f, ensure_ascii=False, indent=2)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)

    def preferences(self) -> Preferences:
        return summarize(self.load())


def fold(counters: Counters, rows: Iterable[Any]) -> Counters:
    """把日志行折进计数。**就地改**并原样返回，检查点由调用方在落盘成功后推进。

    不认识的 kind 直接跳过而不是报错：日志是追加式的，以后还会加新的事件类型，
    旧的折叠规则读到新行不该炸 —— 但 `appointment.booked` 缺 start/时长是写日志
    自己的 bug，那种情况必须当场响。
    """
    for row in rows:
        if row.kind == "ticket.created":
            counters.tickets += 1
            for slot in row.detail.get("missing") or ():
                _bump(counters.missing_slots, str(slot))
        elif row.kind == "appointment.booked":
            start = row.detail.get("start")
            duration = row.detail.get("duration_minutes")
            if not start or duration is None:
                raise ValueError(f"{row.kind} 缺 start/duration_minutes，日志第 {row.seq} 行写坏了")
            when = datetime.fromisoformat(str(start))
            _bump(counters.weekdays, WEEKDAYS[when.weekday()])
            _bump(counters.buckets, bucket_of(when))
            _bump(counters.durations, str(duration))
            _bump(counters.engineers, str(row.detail.get("engineer_id", "?")))
            # `appointment.rescheduled` 故意不折进来：改期说明约成的那个时刻没保住，
            # 一行算一次的话，同一次上门会被当成两次偏好，模型读到的 n/total 全是虚的。
            # 它仍然留在日志里 —— 要统计改期率随时能从原始行重算。
        elif row.kind == "appointment.visited":
            _bump(counters.outcomes, "completed" if row.detail.get("resolved") else "unresolved")
        elif row.kind == "appointment.cancelled":
            _bump(counters.outcomes, "cancelled")
    return counters


__all__ = [
    "AFTERNOON",
    "EVENING",
    "MORNING",
    "WEEKDAYS",
    "Counters",
    "MemoryStore",
    "Preferences",
    "bucket_of",
    "fold",
    "summarize",
]
