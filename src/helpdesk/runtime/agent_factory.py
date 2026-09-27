"""Agent 组装：能用框架接线的一律接线，这里只剩业务提示词和参数。

没有自研的中间件做"工具调用计数"：P0 探针②实测 `max_iters` 数的是模型轮次而不是
tool_call 次数（一轮 3 个调用只 +1），而调用与归因本来就在事件流里 —— 计数交给
评测侧的事件归约器（`eval/` 读 ToolResultEndEvent.state），不在运行时再维护一份副本。
"""
from __future__ import annotations

import os

from agentscope.agent import Agent, ContextConfig, ReActConfig
from agentscope.credential import OllamaCredential
from agentscope.middleware import RAGMiddleware, ReplyBudgetControlMiddleware
from agentscope.model import OllamaChatModel
from agentscope.tool import Toolkit

from ..knowledge import KNOWLEDGE_SCORE_THRESHOLD, HelpdeskIndex
from ..ticket_store import TicketStore
from ..tools import HelpdeskContext, build_tools
from .intake import AskOutLoudMiddleware, IntakeInjectionMiddleware
from .tool_surface import ToolSurfaceMiddleware
from .toolcall_ids import UniqueToolCallIds

CHAT_MODEL = os.environ.get("HELPDESK_CHAT_MODEL", "qwen3:8b")
TOKEN_BUDGET = int(os.environ.get("HELPDESK_TOKEN_BUDGET", "16000"))
MAX_ITERS = int(os.environ.get("HELPDESK_MAX_ITERS", "8"))

SYSTEM_PROMPT = """你是公司 IT 服务台的受理助手，负责把用户的一句话变成一张有人负责的工单。

工作次序：
1. 先复述你对问题的理解。缺不缺信息以用户原话为准：能在原话里找到逐字证据的阻塞槽位
   就不算缺，不要为此追问。
2. **向用户提问只有这一种方式：调用 ask_user。禁止在回复文本里直接发问** ——
   不走过 ask_user 的追问不会被计数，就会出现同一个问题问两遍。
   ask_user 之后系统会撤掉所有工具再叫你一次：那一次必须用中文口语把问题问出来，
   不要输出 JSON、不要模仿工具调用格式。ask_user 说额度已用尽时，停止提问，直接建单。
3. 需要处置依据时调 search_knowledge；返回"没有相关内容"就照实说，不要编。
4. 建单前用 find_open_ticket 查重；命中重复就追加进展，不要新建。
5. 用 create_ticket 建单。这是写操作，会等用户确认；缺的阻塞槽位会由系统记进
   missing_info —— 宁建缺信息的工单，也不能丢单。
6. 建单成功后用 match_service 一次取齐派单材料（服务定位 + 召回候选人 + 已结历史），
   再用 assign_ticket 指派。判断次序：已离职的人绝不可派；默认派给命中服务的 owner；
   owner 无余量或技能不符时派 backup；故障（P0/P1）优先 senior，但唯一会做的人比级别
   更重要；实习坐席只接前端与低风险咨询；主责无法确定或无人可派时，assignee 填
   ESCALATE_HUMAN。历史工单只说明"同类问题当初谁在处理"，不能拿来投票；引用它时要在
   理由里写出「工单 N」。候选人之外的人也可以选，但那等于说召回漏了，系统会记下来。
7. 相似度分数只说明"像不像"，不存在能把"什么都不像"切开的阈值，判空由你填 ESCALATE_HUMAN。

风格：中文、短句、不客套。每次只推进一件事，别把追问和确认混在一句话里。"""


def make_chat_model() -> OllamaChatModel:
    return OllamaChatModel(
        credential=OllamaCredential(host=os.environ.get("OLLAMA_HOST")),
        model=CHAT_MODEL,
        # thinking_enable=False：Go 侧实测思考模型 completion≈837 token，多数是被
        # formatter 丢掉的 reasoning，成本轴会把这部分算成有效输出。
        parameters=OllamaChatModel.Parameters(temperature=0.0, thinking_enable=False),
        stream=True,
    )


async def make_agent(
    index: HelpdeskIndex,
    store: TicketStore | None = None,
    *,
    model: OllamaChatModel | None = None,
    max_iters: int = MAX_ITERS,
    token_budget: int = TOKEN_BUDGET,
    name: str = "helpdesk",
) -> tuple[Agent, HelpdeskContext]:
    ctx = HelpdeskContext(index=index, store=store or TicketStore())
    # 只绑知识语料：服务字典进这里会污染答案检索。
    rag = RAGMiddleware(
        [index.knowledge],
        RAGMiddleware.Parameters(
            mode="agentic",
            top_k=5,
            score_threshold=KNOWLEDGE_SCORE_THRESHOLD,
        ),
    )
    agent = Agent(
        name=name,
        system_prompt=SYSTEM_PROMPT,
        model=model or make_chat_model(),
        # Agent 不收集 middleware 的工具（只有托管路径
        # app/_service/_toolkit.py:233 会 await mw.list_tools()），所以这里自己并。
        toolkit=Toolkit(tools=[*build_tools(ctx), *await rag.list_tools()]),
        middlewares=[
            ToolSurfaceMiddleware(),
            rag,
            IntakeInjectionMiddleware(),
            AskOutLoudMiddleware(),
            ReplyBudgetControlMiddleware(token_budget=token_budget),
            UniqueToolCallIds(),
        ],
        react_config=ReActConfig(max_iters=max_iters),
        context_config=ContextConfig(trigger_ratio=0.8, tool_result_limit=1500),
    )
    return agent, ctx
