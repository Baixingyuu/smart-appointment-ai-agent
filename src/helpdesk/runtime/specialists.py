"""专业子 Agent 封装成工具（P6-a③）：主管仍然握着写操作，专线只负责"查清楚＋给方案"。

为什么不是框架的团队件：`SubAgentTemplate` / `AgentCreate` 是"把活扔进另一个会话"
（fire-and-forget，结果落在团队会话里，靠消息总线传），不是一次同步子调用。
业务要的是"主管在一次推理里问一句、马上拿到答案"，所以这里用最原始的组合：
进程内 `Agent`（只有 name/system_prompt/model 是必填，`state` 自带默认，不需要
storage 与 message_bus）包进 `FunctionTool`。

三条不显然的约束，都是这条路的接缝：

1. **子 Agent 里不能有需要确认的写工具。** `FunctionTool` 不传 permission 时默认
   ASK（`tool/_adapters.py:127`），而子 Agent 的 `RequireUserConfirmEvent` 冒不到
   外层事件流上 —— 主管的工具调用是阻塞的，没人会去回答这个确认，整条链挂死。
   所以专线只拿只读件，写操作回到主管这一层走确认闸。HITL 的边界没有上移，
   只是"查资料"这件事从主管的上下文里搬出去了。

2. **嵌套的模型调用在外层事件流里看不见。** 主管只看到"一次工具调用"，
   而专线内部可能烧了好几轮 —— 成本轴如果只读外层事件就会少算。所以这里把
   子流里的 `ModelCallEndEvent` 聚合进返回值的 metadata，让评测侧的轮次账本
   能把它加回去（P0 探针②：`max_iters` 数的是模型轮次）。

3. **共享的是业务事实和用户原话，不是同一个 `AgentState` 对象。** 两个 agent
   写同一份 context 会让轮次归属和上下文压缩互相踩，所以进来的是
   `user_texts(state)` 的只读副本 —— 槽位证据那套闸本来就要求引文能在用户
   原话里查到，专线看到同样的原话才不会另编一套。
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any

from agentscope.agent import Agent, ReActConfig
from agentscope.event import (
    ExceedMaxItersEvent,
    ModelCallEndEvent,
    RequireUserConfirmEvent,
    ToolCallStartEvent,
)
from agentscope.message import TextBlock, UserMsg
from agentscope.middleware import RAGMiddleware
from agentscope.model import ChatModelBase
from agentscope.permission import PermissionBehavior, PermissionDecision
from agentscope.tool import FunctionTool, ToolChunk, Toolkit
from pydantic import BaseModel, Field

from ..knowledge import KNOWLEDGE_SCORE_THRESHOLD, HelpdeskIndex
from ..tools import HelpdeskContext, build_tools
from .traceability import user_texts

_ALLOW = PermissionDecision(
    behavior=PermissionBehavior.ALLOW,
    message="只读专线，不写任何业务事实",
)

#: 默认关。理由和 P5 那条一样：拆编排的收益没量过之前不该改变线上行为 ——
#: 探针要量的是"同一句话，带不带专线"的轮次与 token 差，不是"看起来更清晰"。
SPECIALISTS_ENABLED = os.environ.get("HELPDESK_SPECIALISTS", "0") == "1"
SPECIALIST_MAX_ITERS = int(os.environ.get("HELPDESK_SPECIALIST_MAX_ITERS", "3"))

CONSULT_TOOL = "consult_ticket"
PLAN_VISIT_TOOL = "plan_visit"
ADVISE_DISPATCH_TOOL = "advise_dispatch"
SPECIALIST_TOOLS = (CONSULT_TOOL, PLAN_VISIT_TOOL, ADVISE_DISPATCH_TOOL)

_STYLE = (
    "风格：中文、短句、只交结论。不要提问、不要复述用户原话、不要输出 JSON、"
    "不要模仿工具调用格式 —— 提问和落库都由上级做。"
)


class SpecialistTask(BaseModel):
    """专线只收一句话的任务书：它不看主管的推理过程，也不直接跟用户说话。"""

    task: str = Field(min_length=4, description="要专线查清楚的那件事，一句话说完")


@dataclass(frozen=True)
class NestedUsage:
    """一次子调用的真实开销。外层只看见一次工具调用，所以这份账必须由子流自己算。"""

    model_calls: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    tool_names: tuple[str, ...] = ()
    parked: str | None = None
    exceeded_iters: bool = False

    @property
    def total_tokens(self) -> int:
        return self.input_tokens + self.output_tokens

    def to_dict(self) -> dict[str, Any]:
        return {
            "model_calls": self.model_calls,
            "input_tokens": self.input_tokens,
            "output_tokens": self.output_tokens,
            "total_tokens": self.total_tokens,
            "tool_names": list(self.tool_names),
            "parked": self.parked,
            "exceeded_iters": self.exceeded_iters,
        }


@dataclass(frozen=True)
class SpecialistSpec:
    name: str
    description: str
    #: 专线的人设与交付物。它替代主管提示词里那一步的说明，所以必须写清"交什么"。
    instruction: str
    #: 从共享 ctx 里挑给它的工具名 —— 只允许只读件，写操作不在这张表里。
    tool_names: tuple[str, ...]


SPECIALIST_SPECS = (
    SpecialistSpec(
        name=CONSULT_TOOL,
        description=(
            "咨询专线：查知识库处置依据，给出用户能照做的步骤。"
            "适合『这个报错一般怎么处理』『该怎么配置』这类问句；它不建单。"
        ),
        instruction=(
            "你是 IT 服务台的知识检索员。上级给你一个要查清的问题，"
            "用 search_knowledge 查知识库，必要时用 find_open_ticket 看是否已有相关工单。\n"
            "交付：结论要点（用户能直接照做的步骤），以及依据来自哪几条知识；"
            "知识库里没有就明确写『知识库无相关内容』，不要编。\n" + _STYLE
        ),
        tool_names=("search_knowledge", "find_open_ticket"),
    ),
    SpecialistSpec(
        name=PLAN_VISIT_TOOL,
        description=(
            "预约专线：查可上门时段与既有安排，给出带依据的建议时刻和还缺哪些槽位。"
            "它只查不落约，落约由上级确认后执行。"
        ),
        instruction=(
            "你是上门预约的排期员。用 propose_appointments 拿真实可约时段，"
            "用 upcoming_appointments 核对该用户是否已有安排。\n"
            "交付：最多三个候选时段（工程师＋时刻＋为什么是他），"
            "以及还缺哪些预约槽位（希望哪天几点前后、需要多久、有无指定工程师）。\n"
            "时段只能来自工具返回；工具说没人能上门就照实说，并给出放宽窗口或改远程两个选项。\n"
            + _STYLE
        ),
        tool_names=("propose_appointments", "upcoming_appointments"),
    ),
    SpecialistSpec(
        name=ADVISE_DISPATCH_TOOL,
        description=(
            "派单专线：取回服务定位、召回候选人与已结历史，给出建议负责人和理由。"
            "它不指派，指派由上级执行。"
        ),
        instruction=(
            "你是 IT 服务台的派单参谋。用 match_service 一次取齐服务命中、召回候选人与已结历史。\n"
            "交付：建议负责人（员工号）＋一句话理由（引用历史时写出工单号）＋置信度；"
            "无人可派或主责不确定时建议 ESCALATE_HUMAN。\n"
            "判定次序：已离职者绝不可派；默认命中服务的 owner；owner 无余量或技能不符时派 backup；"
            "故障(P0/P1)优先 senior，但唯一会做的人比级别更重要；实习坐席只接前端与低风险咨询。\n"
            "相似度只说明『像不像』，不说明『能不能派』；没有能把『什么都不像』切开的阈值。\n"
            + _STYLE
        ),
        tool_names=("match_service",),
    ),
)


def _prompt(spec: SpecialistSpec, task: str, quotes: tuple[str, ...]) -> str:
    said = "\n".join(f"- {t}" for t in quotes) or "（上级没给到用户原话）"
    return (
        f"【上级交来的任务】\n{task}\n\n"
        f"【用户原话（引文必须能在这里查到）】\n{said}"
    )


async def _run(
    spec: SpecialistSpec,
    *,
    model: ChatModelBase,
    tools: list[FunctionTool],
    task: str,
    quotes: tuple[str, ...],
    max_iters: int,
) -> tuple[str, NestedUsage]:
    agent = Agent(
        name=spec.name,
        system_prompt=spec.instruction,
        model=model,
        toolkit=Toolkit(tools=tools),
        react_config=ReActConfig(max_iters=max_iters),
    )
    usage = NestedUsage()
    calls: list[str] = []
    model_calls = 0
    tokens = (0, 0)
    parked: str | None = None
    exceeded = False
    final = ""
    async for evt in agent.reply_stream(
        UserMsg(name="supervisor", content=_prompt(spec, task, quotes)),
        yield_final_msg=True,
    ):
        if isinstance(evt, ModelCallEndEvent):
            model_calls += 1
            tokens = (tokens[0] + evt.input_tokens, tokens[1] + evt.output_tokens)
        elif isinstance(evt, ToolCallStartEvent):
            calls.append(evt.tool_call_name)
        elif isinstance(evt, RequireUserConfirmEvent):
            parked = ",".join(t.name for t in evt.tool_calls)
            break  # 外层没人能回答这个确认，继续等就是挂死整条链
        elif isinstance(evt, ExceedMaxItersEvent):
            exceeded = True
        elif hasattr(evt, "get_text_content"):
            final = evt.get_text_content() or final
    return final, NestedUsage(
        model_calls=model_calls,
        input_tokens=tokens[0],
        output_tokens=tokens[1],
        tool_names=tuple(calls),
        parked=parked,
        exceeded_iters=exceeded,
    )


def _chunk(text: str, metadata: dict[str, Any]) -> ToolChunk:
    return ToolChunk(content=[TextBlock(type="text", text=text)], metadata=metadata)


async def specialist_pool(ctx: HelpdeskContext) -> dict[str, FunctionTool]:
    """专线能挑的工具池：业务件 + RAG 在 agentic 模式下注册的检索件。

    `search_knowledge` 由 RAGMiddleware 提供，而 `Agent` 不会自己收集 middleware
    的工具（只有托管路径 `app/_service/_toolkit.py:233` 会 `await mw.list_tools()`），
    所以这里自己取一次。业务件优先：同名时不能让中间件把业务工具顶掉。
    """
    rag = RAGMiddleware(
        [ctx.index.knowledge],
        RAGMiddleware.Parameters(
            mode="agentic",
            top_k=5,
            score_threshold=KNOWLEDGE_SCORE_THRESHOLD,
        ),
    )
    knowledge = {t.name: t for t in await rag.list_tools()}
    return {**knowledge, **{t.name: t for t in build_tools(ctx)}}


async def make_specialist_tools(
    ctx: HelpdeskContext,
    *,
    model: ChatModelBase,
    max_iters: int = SPECIALIST_MAX_ITERS,
    specs: tuple[SpecialistSpec, ...] = SPECIALIST_SPECS,
) -> list[FunctionTool]:
    """把每条专线包成一个工具。主管看得见的是工具描述，看不见专线内部怎么查。"""
    pool = await specialist_pool(ctx)
    tools: list[FunctionTool] = []
    for spec in specs:
        picked = [pool[name] for name in spec.tool_names if name in pool]
        tools.append(_wrap(spec, picked, model, max_iters))
    return tools


def _wrap(
    spec: SpecialistSpec,
    tools: list[FunctionTool],
    model: ChatModelBase,
    max_iters: int,
) -> FunctionTool:
    async def _call(task: str, _agent_state: Any = None) -> ToolChunk:
        base = {"specialist": spec.name}
        missing = set(spec.tool_names) - {t.name for t in tools}
        if missing:
            return _chunk(
                f"专线 {spec.name} 装配缺工具：{sorted(missing)}。自己按原步骤处理这件事。",
                {**base, "misconfigured": sorted(missing)},
            )
        final, usage = await _run(
            spec,
            model=model,
            tools=tools,
            task=task,
            quotes=user_texts(_agent_state),
            max_iters=max_iters,
        )
        meta = {**base, **usage.to_dict()}
        if usage.parked is not None:
            return _chunk(
                f"专线 {spec.name} 被中止：它试图调用需要用户确认的写操作（{usage.parked}）。"
                "子 Agent 里没人能回答确认，写操作请回到你自己这一层做。",
                meta,
            )
        if not final.strip():
            return _chunk(
                f"专线 {spec.name} 没交出结论"
                + ("（步数用尽）" if usage.exceeded_iters else "")
                + "。自己按原步骤处理这件事，不要把同一个任务再丢给它一次。",
                {**meta, "empty": True},
            )
        return _chunk(f"【{spec.name}】\n{final}", {**meta, "empty": False})

    return FunctionTool(
        _call,
        name=spec.name,
        description=spec.description,
        input_schema=SpecialistTask,
        is_state_injected=True,
        is_concurrency_safe=False,
        permission=_ALLOW,
    )


__all__ = [
    "ADVISE_DISPATCH_TOOL",
    "CONSULT_TOOL",
    "NestedUsage",
    "PLAN_VISIT_TOOL",
    "SPECIALIST_MAX_ITERS",
    "SPECIALIST_SPECS",
    "SPECIALIST_TOOLS",
    "SPECIALISTS_ENABLED",
    "SpecialistSpec",
    "SpecialistTask",
    "make_specialist_tools",
    "specialist_pool",
]
