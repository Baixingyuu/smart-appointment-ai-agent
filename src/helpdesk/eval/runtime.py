"""运行时采集层：把事件流归约成"这一轮花了多少、走了哪条路"，再算分布。

缝在框架那边：每个事件都自带 `created_at`，`ModelCallEndEvent` 带 per-call token
（`Msg.usage` 只有整份 reply 的合计，分不出是哪一轮花的）。量表在这里，而且**延迟必须拆开**：

- `model_ms`：模型调用本身（各轮 `ModelCallStart→End` 之和）；
- `tool_ms`：工具执行（`match_service` 真机 1.6s，里面是 embedding 往返）；
- `wait_ms`：`_ASK` 工具 park 之后等人的时间；
- `residual_ms`：`wall - 上面三项`，看得见就是没被藏起来。

合起来报一个 P95 是错的：`data/service.db` 里那条真实会话，`create_ticket` 的 tool_call
到 tool_result 隔了 62 秒，而工具本身只花 50ms —— 那 62 秒是人在读卡片，不是系统的延迟。

`percentile` 用"向上取整的最近秩"（`ceil(q*n)-1`），不是 `metrics._pct` 的 `round(q*(n-1))`：
n=20 时后者把 P95 落在第 19 个样本上，等于报出一个不存在的分位数。n<20 时 P95 只是记号，
`runtime_summary` 会带 `p95Note` 说明它撑不起结论。
"""
from __future__ import annotations

import json
import math
from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Iterable

#: 需要用户确认才写得下去的工具（`tools.py` 里 permission=_ASK 的那几个）。
CONFIRM_TOOLS = frozenset({"create_ticket", "assign_ticket", "suggest_assignment", "book_appointment", "reschedule_appointment"})


def _parse_ts(value: Any) -> datetime | None:
    if value is None:
        return None
    if isinstance(value, datetime):
        return value
    try:
        return datetime.fromisoformat(str(value))
    except ValueError:
        return None


def _ms(origin: datetime | None, when: datetime | None) -> float | None:
    if origin is None or when is None:
        return None
    return round((when - origin).total_seconds() * 1000.0, 1)


def _json_obj(raw: Any) -> dict[str, Any]:
    try:
        parsed = json.loads(raw) if isinstance(raw, str) else (raw or {})
    except (TypeError, json.JSONDecodeError):
        return {}
    return parsed if isinstance(parsed, dict) else {}


def _error_info(raw: Any) -> tuple[str, str]:
    """错误 → (message, kind)。三种形状都见过：框架的 `ErrorInfo`、落库的 dict、裸字符串。

    kind 取自 `agentscope.types.ErrorType`（StrEnum：upstream / connection / rate_limit /
    internal / setup / …）。取不到就留空串 —— 归不了责的错误，gates 按最严的一档判。
    """
    if raw is None:
        return "", ""
    if isinstance(raw, dict):
        return str(raw.get("message") or ""), str(raw.get("type") or "")
    message = getattr(raw, "message", None)
    if message is not None:
        return str(message), str(getattr(raw, "type", "") or "")
    return str(raw), ""


@dataclass
class Step:
    """一次工具调用。时刻都是相对轨迹起点的毫秒偏移。

    不冻结：调用先被看见、结果后被看见，两条路径都写同一个对象。
    """

    name: str
    tool_call_id: str
    state: str = "pending"
    called_at: float | None = None
    finished_at: float | None = None
    round_index: int = 0
    waited: bool = False
    arguments: dict[str, Any] = field(default_factory=dict)

    @property
    def duration_ms(self) -> float | None:
        if self.called_at is None or self.finished_at is None:
            return None
        return round(self.finished_at - self.called_at, 1)


@dataclass
class Trace:
    """一条轨迹的全部事实。判分与统计都只读它。"""

    case_id: str = ""
    repeat: int = 0
    model: str = ""
    rounds: int = 0
    model_ms: float = 0.0
    tool_ms: float = 0.0
    wait_ms: float = 0.0
    wall_ms: float = 0.0
    input_tokens: int = 0
    output_tokens: int = 0
    cache_input_tokens: int = 0
    steps: list[Step] = field(default_factory=list)
    final_text: str = ""
    exceeded_max_iters: bool = False
    parked: list[str] = field(default_factory=list)
    error: str | None = None
    #: 错误归谁：托管落库的 `payload["error"]["type"]` 直接给了这个（实测 `"upstream"`）。
    #: 空串=没标。gates 用它分开判"上游模型 5xx"（可用性，改不动）与"我们把自己跑崩了"（红线）。
    error_kind: str = ""

    @property
    def tool_names(self) -> list[str]:
        return [s.name for s in self.steps]

    @property
    def by_round(self) -> list[list[str]]:
        out: list[list[str]] = []
        for s in self.steps:
            while len(out) <= s.round_index:
                out.append([])
            out[s.round_index].append(s.name)
        return out

    @property
    def errors(self) -> list[str]:
        return [s.name for s in self.steps if s.state == "error"]

    @property
    def pending(self) -> list[str]:
        return [s.name for s in self.steps if s.state == "pending"]

    @property
    def residual_ms(self) -> float:
        return round(self.wall_ms - self.model_ms - self.tool_ms - self.wait_ms, 1)

    def openai_messages(self, user_text: str = "") -> list[dict[str, Any]]:
        """成 openjudge grader 认的形状（assistant.tool_calls[].function.arguments 是字符串）。"""
        msgs: list[dict[str, Any]] = []
        if user_text:
            msgs.append({"role": "user", "content": user_text})
        for round_index, _names in enumerate(self.by_round):
            calls = [s for s in self.steps if s.round_index == round_index]
            if not calls:
                continue
            msgs.append(
                {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": s.tool_call_id,
                            "type": "function",
                            "function": {
                                "name": s.name,
                                "arguments": json.dumps(s.arguments, ensure_ascii=False),
                            },
                        }
                        for s in calls
                    ],
                },
            )
        if self.final_text:
            msgs.append({"role": "assistant", "content": self.final_text})
        return msgs


def trace_from_events(
    events: Iterable[Any],
    *,
    case_id: str = "",
    repeat: int = 0,
    model: str = "",
) -> Trace:
    """事件流 → Trace。纯函数：不碰网络、不读时钟，时刻全部来自事件自带的 `created_at`。"""
    from agentscope.event import (
        ExceedMaxItersEvent,
        ModelCallEndEvent,
        ModelCallStartEvent,
        ReplyEndEvent,
        RequireUserConfirmEvent,
        TextBlockDeltaEvent,
        TextBlockEndEvent,
        ToolCallStartEvent,
        ToolResultEndEvent,
        UserConfirmResultEvent,
    )

    trace = Trace(case_id=case_id, repeat=repeat, model=model)
    origin: datetime | None = None
    last: datetime | None = None
    model_started: datetime | None = None
    confirm_at: datetime | None = None
    open_calls: dict[str, tuple[str, datetime, int]] = {}
    chunks: dict[str, list[str]] = {}
    parked_ids: set[str] = set()
    round_index = -1

    def offset(when: datetime | None) -> float | None:
        if when is None or origin is None:
            return None
        return round((when - origin).total_seconds() * 1000.0, 1)

    def record(step: Step) -> None:
        """同一 tool_call_id 只留一份：park 先记 asking，落地后被结果事件改写。"""
        for i, prev in enumerate(trace.steps):
            if prev.tool_call_id == step.tool_call_id:
                trace.steps[i] = step
                return
        trace.steps.append(step)

    for evt in events:
        when = _parse_ts(getattr(evt, "created_at", None))
        if when is not None:
            origin = origin or when
            last = when
        if isinstance(evt, ModelCallStartEvent):
            round_index += 1
            trace.rounds += 1
            model_started = when
        elif isinstance(evt, ModelCallEndEvent):
            if model_started is not None and when is not None:
                trace.model_ms += (when - model_started).total_seconds() * 1000.0
            trace.input_tokens += evt.input_tokens or 0
            trace.output_tokens += evt.output_tokens or 0
            trace.cache_input_tokens += evt.cache_input_tokens or 0
        elif isinstance(evt, ToolCallStartEvent):
            open_calls[evt.tool_call_id] = (evt.tool_call_name, when or datetime.now(), round_index)
        elif isinstance(evt, ToolResultEndEvent):
            name, call_at, r_index = open_calls.pop(
                evt.tool_call_id,
                (str(evt.tool_call_id), when or datetime.now(), round_index),
            )
            record(
                Step(
                    name=name,
                    tool_call_id=evt.tool_call_id,
                    state=str(getattr(evt.state, "value", evt.state)),
                    called_at=offset(call_at),
                    finished_at=offset(when),
                    round_index=r_index,
                    waited=evt.tool_call_id in parked_ids,
                ),
            )
        elif isinstance(evt, RequireUserConfirmEvent):
            confirm_at = when
            for call in evt.tool_calls:
                cid = getattr(call, "id", None)
                parked_ids.add(cid or "")
                name = getattr(call, "name", "?")
                trace.parked.append(name)
                _n, call_at, r_index = open_calls.get(cid or "", (name, when, round_index))
                record(
                    Step(
                        name=name,
                        tool_call_id=str(cid or ""),
                        state="asking",
                        called_at=offset(call_at),
                        finished_at=None,
                        round_index=r_index,
                        waited=True,
                    ),
                )
        elif isinstance(evt, UserConfirmResultEvent):
            if confirm_at is not None and when is not None:
                trace.wait_ms += (when - confirm_at).total_seconds() * 1000.0
            confirm_at = None
        elif isinstance(evt, ExceedMaxItersEvent):
            trace.exceeded_max_iters = True
        elif isinstance(evt, TextBlockDeltaEvent):
            chunks.setdefault(evt.reply_id, []).append(evt.delta)
        elif isinstance(evt, TextBlockEndEvent):
            # end 事件只在"正文不是增量拼出来的"时才带 text（语音答到一半被打断）。
            if evt.text is not None:
                chunks[evt.reply_id] = [evt.text]
        elif isinstance(evt, ReplyEndEvent):
            message, kind = _error_info(getattr(evt, "error", None))
            if message and not trace.error:
                trace.error, trace.error_kind = message, kind
            text = "".join(chunks.pop(evt.reply_id, [])).strip()
            if text:
                trace.final_text = text

    if origin is not None and last is not None:
        trace.wall_ms = round((last - origin).total_seconds() * 1000.0, 1)
    trace.model_ms = round(trace.model_ms, 1)
    trace.wait_ms = round(trace.wait_ms, 1)
    trace.tool_ms = round(sum(s.duration_ms or 0.0 for s in trace.steps if not s.waited), 1)
    return trace


def attach_arguments(trace: Trace, messages: Iterable[Any]) -> Trace:
    """把 tool_call 的入参从落定的 `Msg` 并回来（事件流只给 id 与名字）。"""
    args_by_id: dict[str, dict[str, Any]] = {}
    for msg in messages:
        if not hasattr(msg, "get_content_blocks"):
            continue
        for block in msg.get_content_blocks("tool_call"):
            cid = getattr(block, "id", None) or (block.get("id") if isinstance(block, dict) else None)
            raw = getattr(block, "input", None)
            if raw is None and isinstance(block, dict):
                raw = block.get("input")
            try:
                args_by_id[str(cid)] = json.loads(raw or "{}")
            except (TypeError, json.JSONDecodeError):
                args_by_id[str(cid)] = {}
    trace.steps = [
        Step(
            name=s.name,
            tool_call_id=s.tool_call_id,
            state=s.state,
            called_at=s.called_at,
            finished_at=s.finished_at,
            round_index=s.round_index,
            waited=s.waited,
            arguments=args_by_id.get(s.tool_call_id, {}),
        )
        for s in trace.steps
    ]
    return trace


def traces_from_messages_db(db_path: str | Path, *, limit: int | None = None) -> list[Trace]:
    """托管路径的落库会话 → Trace。这是**真机流量**的成本与延迟来源，不跑模型。

    时刻表是从 `data/service.db` 的块结构实测出来的（每块都带 created_at/finished_at）：

    - `tool_call` 块本身 ~50ms 就封板了，那不是工具执行；
    - `tool_result` 块的 created_at→finished_at 才是执行窗口（match_service 1.62s、
      assign_ticket 72ms、create_ticket 0.5ms）；
    - 确认类工具从 call 封板到 result 出现之间隔的是人读卡片的时间（实测 7~74s）；
    - 这条时间轴上其余的空隙都是模型生成，收尾正文块的 created_at→finished_at 也是。

    token 只能按**整份 reply** 记：`Msg.usage` 没有分轮，分轮归因只有事件流那条路
    （`trace_from_events`）。所以这张表能回答"一次受理花多少"，回答不了"哪一轮花的"。
    """
    import sqlite3

    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        rows = conn.execute(
            "select session_id, payload from messages order by created_at",
        ).fetchall()
    finally:
        conn.close()

    traces: list[Trace] = []
    seen: dict[str, int] = {}
    for session_id, raw in rows:
        payload = json.loads(raw)
        if payload.get("role") != "assistant":
            continue
        blocks = payload.get("content") or []
        origin = _parse_ts(payload.get("created_at")) or next(
            (t for t in (_parse_ts(b.get("created_at")) for b in blocks) if t is not None),
            None,
        )
        if origin is None:
            continue
        trace = Trace(
            case_id=f"{str(session_id)[:8]}#{seen.get(str(session_id), 0)}",
            model=str(payload.get("name") or ""),
        )
        seen[str(session_id)] = seen.get(str(session_id), 0) + 1
        cursor = origin
        usage = payload.get("usage") or {}
        trace.input_tokens = int(usage.get("input_tokens") or 0)
        trace.output_tokens = int(usage.get("output_tokens") or 0)
        trace.cache_input_tokens = int(usage.get("cache_input_tokens") or 0)
        calls = {str(b.get("id")): b for b in blocks if b.get("type") == "tool_call"}
        n_text = sum(1 for b in blocks if b.get("type") == "text" and (b.get("text") or "").strip())
        steps_by_id: dict[str, Step] = {}

        for block in blocks:
            start = _parse_ts(block.get("created_at"))
            end = _parse_ts(block.get("finished_at"))
            gap = (start - cursor).total_seconds() * 1000.0 if start is not None and start > cursor else 0.0
            span = (end - start).total_seconds() * 1000.0 if start is not None and end is not None and end > start else 0.0
            kind = block.get("type")
            step = steps_by_id.get(str(block.get("id"))) if kind == "tool_result" else None
            gated = step is not None and (step.name in CONFIRM_TOOLS or step.state == "asking")
            if kind == "tool_result" and gated and gap > 0:
                trace.wait_ms += gap
                step.waited = True
            else:
                trace.model_ms += gap
            if kind == "tool_call":
                new_step = Step(
                    name=str(block.get("name")),
                    tool_call_id=str(block.get("id")),
                    state="asking" if block.get("state") == "asking" else "pending",
                    called_at=_ms(origin, start),
                )
                new_step.arguments = _json_obj(block.get("input"))
                trace.steps.append(new_step)
                steps_by_id[new_step.tool_call_id] = new_step
                trace.model_ms += span
            elif kind == "tool_result":
                trace.tool_ms += span
                if step is not None:
                    step.state = str(block.get("state") or "success")
                    step.finished_at = _ms(origin, end)
            else:
                trace.model_ms += span
            for moment in (start, end):
                if moment is not None and moment > cursor:
                    cursor = moment
        trace.rounds = len(calls) + (1 if n_text else 0)
        message, kind = _error_info(payload.get("error"))
        if not message and payload.get("finished_reason") == "error":
            message, kind = "finished_reason=error", ""
        trace.error, trace.error_kind = message or None, kind
        trace.final_text = "\n".join(
            str(b.get("text") or "") for b in blocks if b.get("type") == "text"
        ).strip()
        trace.parked = [s.name for s in trace.steps if s.waited]
        if cursor is not None:
            trace.wall_ms = round((cursor - origin).total_seconds() * 1000.0, 1)
        trace.model_ms = round(trace.model_ms, 1)
        trace.tool_ms = round(trace.tool_ms, 1)
        trace.wait_ms = round(trace.wait_ms, 1)
        traces.append(trace)
        if limit is not None and len(traces) >= limit:
            break
    return traces


def percentile(values: list[float], q: float) -> float | None:
    """向上取整的最近秩。空输入给 None 而不是 0 —— 0 会被读成"很快"。"""
    if not values:
        return None
    s = sorted(values)
    idx = min(len(s) - 1, max(0, math.ceil(q * len(s)) - 1))
    return round(float(s[idx]), 1)


def _block(values: list[float], qs: tuple[float, ...]) -> dict[str, Any]:
    out: dict[str, Any] = {"n": len(values)}
    for q in qs:
        out[f"p{int(q * 100)}"] = percentile(values, q) if values else None
    out["mean"] = round(sum(values) / len(values), 1) if values else None
    out["max"] = round(max(values), 1) if values else None
    return out


def runtime_summary(traces: list[Trace], qs: tuple[float, ...] = (0.5, 0.95)) -> dict[str, Any]:
    """一批 Trace → 延迟/轮次/token 的分布。只给形状，不判对错。"""
    per_tool: dict[str, list[float]] = defaultdict(list)
    for t in traces:
        for s in t.steps:
            if s.duration_ms is not None and not s.waited:
                per_tool[s.name].append(s.duration_ms)
    summary: dict[str, Any] = {
        "samples": len(traces),
        "wallMs": _block([t.wall_ms for t in traces], qs),
        "modelMs": _block([t.model_ms for t in traces], qs),
        "toolMs": _block([t.tool_ms for t in traces], qs),
        "waitMs": _block([t.wait_ms for t in traces], qs),
        "residualMs": _block([t.residual_ms for t in traces], qs),
        "rounds": _block([float(t.rounds) for t in traces], qs),
        "inputTokens": _block([float(t.input_tokens) for t in traces], qs),
        "outputTokens": _block([float(t.output_tokens) for t in traces], qs),
        "perToolMs": {name: _block(vals, qs) for name, vals in sorted(per_tool.items())},
    }
    p95_n = min((summary[k]["n"] for k in ("wallMs", "modelMs", "toolMs")), default=0)
    if p95_n < 20:
        summary["p95Note"] = f"n={p95_n} 撑不起 P95（向上最近秩下它取的是第 {math.ceil(0.95*p95_n) or 1} 名），只能看形状"
    return summary
