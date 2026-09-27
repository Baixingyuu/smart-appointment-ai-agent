"""派单两层：层 1 确定性服务抽取（dense，无 LLM），层 2 一次结构化指派。

原来的五特征加权、硬约束过滤、离散判弱、epsilon 排序整块删掉。
判空（"这单没有匹配的服务/无人可派"）不再由全局阈值表达 —— P0 探针已证明
单个 dense 阈值买不回被删的 6 桶置信门 —— 而是由层 2 的枚举承担。

v4 改的是层 2 的**输入**：三源（服务 / 员工画像 / 已结历史）全部在 `match_service`
内部固定召回，不靠模型自愿调检索 —— 实测过模型会整步跳过检索直接反问
（`docs/P3_AGENT.md` 的 kb_vague：1 次调用、零工具）。员工从"名册全量列出"改成
"先召回再选"，因为名册会长到装不进提示词；历史工单是新证据源，只做证据不做投票。
"""
from __future__ import annotations

import re
from dataclasses import dataclass, replace
from typing import Literal

from pydantic import BaseModel, Field

from .catalog import (
    ESCALATE_HUMAN,
    Employee,
    Service,
    employees,
    services_by_id,
    team_name,
)
from .knowledge import HelpdeskIndex

TOP_K_SERVICES = 3
TOP_K_EMPLOYEES = 4
TOP_K_HISTORY = 3

#: 语义候选的名额上限。归属候选（owner/backup）不占这个额度也不被它截掉 ——
#: 它是业务事实，不是检索分数。
#: **当前不生效**：`TOP_K_EMPLOYEES` 就等于 4，检索最多回来 4 条，这道闸轮不到触发。
#: 留着它的唯一理由是"top_k 一旦大于它立刻生效"；真正决定候选数的是
#: `TOP_K_EMPLOYEES + 结构候选数`，不是这里。
MAX_EXTRA_CANDIDATES = 4

#: 指派枚举 = 全名册 + 判空。从 roster 派生，避免名册一变新人就选不出来。
#: 离职者（109/124）**留在枚举里**：在职闸要能被观测到"模型选了但被 domain 拦下"，
#: 从枚举里悄悄删掉就再也测不出这条闸有没有生效。
ANSWER_CHOICES = tuple(str(e.id) for e in employees()) + (ESCALATE_HUMAN,)

AssignmentAnswer = Literal[ANSWER_CHOICES]


@dataclass(frozen=True)
class ServiceHit:
    service_id: int
    name: str
    score: float
    owner_id: int
    backup_owner_id: int | None
    team_name: str

    @property
    def owners(self) -> tuple[int, ...]:
        return (self.owner_id,) if self.backup_owner_id is None else (self.owner_id, self.backup_owner_id)


@dataclass
class Extraction:
    """层 1 输出。判空不在这里做，只带分数下去给层 2 看。"""

    query: str
    hits: tuple[ServiceHit, ...]

    @property
    def top1(self) -> ServiceHit | None:
        return self.hits[0] if self.hits else None


def to_hit(service_id: int, score: float) -> ServiceHit:
    svc: Service = services_by_id()[service_id]
    return ServiceHit(
        service_id=svc.id,
        name=svc.name,
        score=score,
        owner_id=svc.owner_id,
        backup_owner_id=svc.backup_owner_id,
        team_name=team_name(svc.team_id),
    )


async def extract_services(
    index: HelpdeskIndex,
    query: str,
    top_k: int = TOP_K_SERVICES,
    score_threshold: float | None = None,
) -> Extraction:
    results = await index.search_services(query, top_k=top_k, score_threshold=score_threshold)
    hits = tuple(to_hit(int(r.document_id), r.score) for r in results)
    return Extraction(query=query, hits=hits)


@dataclass(frozen=True)
class EmployeeCandidate:
    """一个候选人 + 他为什么在候选里。

    `score` 为 None 表示纯结构候选（owner/backup），它不参与相似度排序；
    离职者**保留在候选里并标注状态** —— 拦下他是 `domain.assign` 的在职闸和模型的职责，
    从候选里悄悄删掉会让这条闸变成无法观测的死代码。
    """

    employee: Employee
    score: float | None
    reasons: tuple[str, ...]

    @property
    def id(self) -> int:
        return self.employee.id

    @property
    def structural(self) -> bool:
        return any(r.startswith(("owner@", "backup@")) for r in self.reasons)


@dataclass(frozen=True)
class HistoryCase:
    ticket_id: int
    title: str
    assignee_id: int | None
    service_id: int | None
    score: float


def _employee_by_id() -> dict[int, Employee]:
    return {e.id: e for e in employees()}


async def recall_employees(
    index: HelpdeskIndex,
    query: str,
    hits: tuple[ServiceHit, ...],
    top_k: int = TOP_K_EMPLOYEES,
) -> tuple[EmployeeCandidate, ...]:
    """归属候选（不截断）∪ 语义候选（最多 MAX_EXTRA_CANDIDATES 个新面孔）。"""
    by_id = _employee_by_id()
    picked: dict[int, EmployeeCandidate] = {}
    order: list[int] = []

    def add(emp_id: int, reason: str, score: float | None) -> None:
        if emp_id not in by_id:
            return
        current = picked.get(emp_id)
        if current is None:
            picked[emp_id] = EmployeeCandidate(by_id[emp_id], score, (reason,))
            order.append(emp_id)
            return
        reasons = current.reasons if reason in current.reasons else current.reasons + (reason,)
        picked[emp_id] = replace(
            current,
            reasons=reasons,
            score=score if current.score is None else max(current.score, score),
        )

    for hit in hits:
        add(hit.owner_id, f"owner@{hit.service_id}", None)
        if hit.backup_owner_id is not None:
            add(hit.backup_owner_id, f"backup@{hit.service_id}", None)

    structural = len(order)
    for r in await index.search_employees(query, top_k=top_k):
        if len(order) - structural >= MAX_EXTRA_CANDIDATES and int(r.document_id) not in picked:
            continue
        add(int(r.document_id), "semantic", r.score)
    return tuple(picked[i] for i in order)


async def recall_history(
    index: HelpdeskIndex,
    query: str,
    top_k: int = TOP_K_HISTORY,
    score_threshold: float | None = None,
) -> tuple[HistoryCase, ...]:
    """已结历史工单：只作证据，不作票。未结单被框架的读取句柄挡在外面。"""
    cases: list[HistoryCase] = []
    for r in await index.search_history(query, top_k=top_k, score_threshold=score_threshold):
        meta = r.chunk.metadata
        cases.append(
            HistoryCase(
                ticket_id=int(meta["ticket_id"]),
                title=str(meta["title"]),
                assignee_id=int(meta["assignee_id"]) or None,
                service_id=int(meta["service_id"]) or None,
                score=r.score,
            ),
        )
    return tuple(cases)


@dataclass(frozen=True)
class DispatchEvidence:
    """一次派单看到的召回结果。越选与引证都由这里算，不信模型自述。"""

    candidates: tuple[EmployeeCandidate, ...] = ()
    history: tuple[HistoryCase, ...] = ()

    @property
    def candidate_ids(self) -> tuple[int, ...]:
        return tuple(c.id for c in self.candidates)

    def recalled(self, employee_id: int | None) -> bool:
        """转人工（None）算命中候选 —— 越选只指"选了没召回的人"，不包括判空。"""
        return employee_id is None or employee_id in self.candidate_ids

    def cites(self, rationale: str) -> tuple[int, ...]:
        return cited_tickets(rationale, self.history)


class AssignmentDecision(BaseModel):
    """层 2 的结构化输出。

    枚举仍是**全名册**而不是当次候选 —— 越选（选中没召回的人）要能被观测和统计，
    把枚举收窄成候选集等于把召回漏检伪装成 ESCALATE_HUMAN。
    """

    assignee: AssignmentAnswer = Field(
        description="唯一负责人员工号；确实无人可派时填 ESCALATE_HUMAN。",
    )
    rationale: str = Field(min_length=4, description="一句话说明为什么是他。")
    confidence: float = Field(ge=0.0, le=1.0)

    @property
    def escalated(self) -> bool:
        return self.assignee == ESCALATE_HUMAN

    @property
    def employee_id(self) -> int | None:
        return None if self.escalated else int(self.assignee)


def render_services(hits: tuple[ServiceHit, ...]) -> str:
    if not hits:
        return "服务字典检索无命中。"
    lines = []
    for i, h in enumerate(hits, start=1):
        backup = "无 backup" if h.backup_owner_id is None else f"backup={h.backup_owner_id}"
        lines.append(
            f"{i}. 服务 {h.service_id} {h.name}（{h.team_name}）"
            f" owner={h.owner_id} {backup} 相似度={h.score:.3f}",
        )
    return "\n".join(lines)


def render_candidates(candidates: tuple[EmployeeCandidate, ...]) -> str:
    if not candidates:
        return "无候选人。"
    lines = []
    for c in candidates:
        e = c.employee
        score = "" if c.score is None else f" 相似度={c.score:.3f}"
        status = "在职" if e.active else "已离职（不可派单）"
        lines.append(
            f"- {e.id} {e.name} 级别={e.level.value} {status}"
            f" 负载={e.current_load}/{e.max_concurrent}"
            f" 召回理由={'+'.join(c.reasons)}{score}",
        )
        lines.append(f"  画像：{e.profile}")
    return "\n".join(lines)


def render_history(cases: tuple[HistoryCase, ...]) -> str:
    if not cases:
        return "无可参考的已结历史工单。"
    by_id = _employee_by_id()
    lines = []
    for c in cases:
        if c.assignee_id is None:
            assignee = "未知"
        else:
            emp = by_id.get(c.assignee_id)
            assignee = str(c.assignee_id) if emp is None else f"{c.assignee_id} {emp.name}"
            if not emp.active:
                assignee += "（此人已离职，不可照抄）"
        service = "未知" if c.service_id is None else str(c.service_id)
        lines.append(
            f"- 工单 {c.ticket_id}《{c.title}》当初由 {assignee} 处理，归到服务 {service}"
            f"，相似度={c.score:.3f}",
        )
    return "\n".join(lines)


_CITATION = re.compile(r"(?:#|＃|工单\s*|单据\s*)(\d{1,6})")


def cited_tickets(rationale: str, cases: tuple[HistoryCase, ...]) -> tuple[int, ...]:
    """理由里真的引用了哪几条历史 —— 代码算，不信模型的自述。

    只认"工单 N／#N"这种带前缀的写法：裸数字会把服务号（2001）和员工号（101）
    当成工单号，而真实工单号恰恰是个位数。
    """
    mentioned = set(_CITATION.findall(rationale))
    return tuple(c.ticket_id for c in cases if str(c.ticket_id) in mentioned)


def assignment_prompt(
    ticket_text: str,
    hits: tuple[ServiceHit, ...],
    candidates: tuple[EmployeeCandidate, ...],
    history: tuple[HistoryCase, ...] = (),
    extra: str = "",
) -> str:
    return (
        "你是 IT 服务台的派单员。从下面的候选人里选出唯一负责人，"
        "并在枚举里填 ESCALATE_HUMAN 表示这单必须转人工。\n\n"
        f"【工单】\n{ticket_text}\n\n"
        f"【服务字典命中（top {len(hits)}，按相似度）】\n{render_services(hits)}\n\n"
        f"【召回候选人（top {len(candidates)}）】\n{render_candidates(candidates)}\n\n"
        f"【已结历史工单（仅作证据）】\n{render_history(history)}\n\n"
        f"{extra}\n\n"
        "判定次序：已离职者绝不可派；默认派给命中服务的 owner；owner 无余量、"
        "技能明显不符或需要跨组隔离时派 backup；"
        "故障(P0/P1)优先 senior，但唯一会做的人比级别更重要；"
        "实习坐席只接前端类与低风险咨询；主责无法确定或无人可派时转人工。\n"
        "历史工单只能用来说明'同类问题当初谁在处理'，不能当成投票；"
        "引用历史时请在理由里写出工单号。"
        "候选人之外的人理论上也能选（枚举是全名册），但那说明召回漏了，会被记录。"
    )
