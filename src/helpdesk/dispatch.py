"""派单两层：层 1 确定性服务抽取（dense，无 LLM），层 2 一次结构化指派。

原来的五特征加权、硬约束过滤、离散判弱、epsilon 排序整块删掉。
判空（"这单没有匹配的服务/无人可派"）不再由全局阈值表达 —— P0 探针已证明
单个 dense 阈值买不回被删的 6 桶置信门 —— 而是由层 2 的枚举承担。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

from pydantic import BaseModel, Field

from .catalog import ESCALATE_HUMAN, Service, employees, services_by_id, team_name
from .knowledge import HelpdeskIndex

TOP_K_SERVICES = 3

AssignmentAnswer = Literal[
    "101", "102", "103", "104", "105", "106", "107", "108", "109", ESCALATE_HUMAN,
]


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


class AssignmentDecision(BaseModel):
    """层 2 的结构化输出：9 个员工 + 14 个服务全摆得下，连候选生成都不要。"""

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


def render_roster() -> str:
    lines = []
    for e in employees():
        load = f"{e.current_load}/{e.max_concurrent}"
        status = "在职" if e.active else "已离职（不可派单）"
        lines.append(f"- {e.id} {e.name} 级别={e.level.value} {status} 负载={load} 画像={e.profile}")
    return "\n".join(lines)


def assignment_prompt(
    ticket_text: str,
    hits: tuple[ServiceHit, ...],
    extra: str = "",
) -> str:
    return (
        "你是 IT 服务台的派单员。从下面的在册员工里选出唯一负责人，"
        "并在枚举里填 ESCALATE_HUMAN 表示这单必须转人工。\n\n"
        f"【工单】\n{ticket_text}\n\n"
        f"【服务字典命中（top {len(hits)}，按相似度）】\n{render_services(hits)}\n\n"
        f"【在册员工】\n{render_roster()}\n\n"
        f"{extra}\n\n"
        "判定次序：已离职者绝不可派；默认派给命中服务的 owner；owner 无余量、"
        "技能明显不符或需要跨组隔离时派 backup；"
        "故障(P0/P1)优先 senior，但唯一会做的人比级别更重要；"
        "实习坐席只接前端类与低风险咨询；主责无法确定或无人可派时转人工。"
    )
