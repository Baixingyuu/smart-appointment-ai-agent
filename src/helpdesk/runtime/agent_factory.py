"""Agent 组装：能用框架接线的一律接线，这里只剩业务提示词和参数。

没有自研的中间件做"工具调用计数"：P0 探针②实测 `max_iters` 数的是模型轮次而不是
tool_call 次数（一轮 3 个调用只 +1），而调用与归因本来就在事件流里 —— 计数交给
评测侧的事件归约器（`eval/` 读 ToolResultEndEvent.state），不在运行时再维护一份副本。
"""
from __future__ import annotations

import os

from agentscope.agent import Agent, ContextConfig, InjectionConfig, ReActConfig
from agentscope.credential import OllamaCredential
from agentscope.middleware import RAGMiddleware, ReplyBudgetControlMiddleware
from agentscope.model import OllamaChatModel
from agentscope.tool import Toolkit

from ..knowledge import KNOWLEDGE_SCORE_THRESHOLD, HelpdeskIndex
from ..ticket_store import TicketStore
from ..tools import HelpdeskContext, build_tools
from .intake import AskOutLoudMiddleware, IntakeInjectionMiddleware
from .intent_router import IntentRouterMiddleware
from .specialists import SPECIALISTS_ENABLED, make_specialist_tools
from .tool_surface import ToolSurfaceMiddleware
from .toolcall_ids import UniqueToolCallIds

CHAT_MODEL = os.environ.get("HELPDESK_CHAT_MODEL", "qwen3:8b")
TOKEN_BUDGET = int(os.environ.get("HELPDESK_TOKEN_BUDGET", "16000"))
MAX_ITERS = int(os.environ.get("HELPDESK_MAX_ITERS", "8"))
#: 框架注入给模型的"现在几点"默认按 UTC 墙钟（`InjectionConfig.timezone`），
#: 而上门窗口比的是本地时刻 —— 不改正差八小时，模型会把"下午三点"写成 15:00Z。
TIMEZONE = os.environ.get("HELPDESK_TZ", "Asia/Shanghai")
#: 窗口占用过六成就让框架的 compress_context 先收敛一次。留 0.8 等于贴着上限才压，
#: 长会话里最后一轮工具结果就能把上下文顶穿。
CONTEXT_TRIGGER_RATIO = float(os.environ.get("HELPDESK_CONTEXT_TRIGGER_RATIO", "0.6"))
TOOL_RESULT_LIMIT = 1500
#: 同一份阈值的两种形状：console 吃 ContextConfig 对象，托管路径吃 POST /agent/ 的 JSON。
CONTEXT_CONFIG = {
    "trigger_ratio": CONTEXT_TRIGGER_RATIO,
    "tool_result_limit": TOOL_RESULT_LIMIT,
}

SYSTEM_PROMPT = """你是公司 IT 服务台的受理助手，负责把用户的一句话变成一张工单，
给出该由谁处理的推荐、在用户选定后完成指派，并在需要人到现场处理时把上门时间约下来。

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
6. 建单成功后调 match_service 拿推荐材料（服务定位 + 召回候选人 + 已结历史），
   从中挑 2-3 个候选工程师，每个带一句理由；需要上门时再调 propose_appointments
   拿可选时段。然后用 suggest_assignment 把候选和时段一起弹成卡片让用户点选
   （这是写操作，会等用户点选）。不要自己替用户选，也不要把候选只念成文字就停。
   判断次序：已离职的人绝不推荐；默认命中服务的 owner，owner 无余量或技能不符时
   推荐 backup；故障（P0/P1）优先 senior；主责无法确定或没有合适候选人时照实说
   "建议转人工认领"。历史工单只作证据不作投票。用户点选后系统会自动落指派和预约。
7. 相似度分数只说明"像不像"，不存在能把"什么都不像"切开的阈值，判空就照实说没有合适的
   工程师，不要为了给出答案而编一个人。
8. 要上门时先 propose_appointments 拿真实时段，把时刻念给用户挑，用户认可后才
   book_appointment（写操作，会等确认）。**时刻只能来自工具返回**：工具说这个窗口
   没人能上门，就照实说并给出放宽或改远程的选项，不要自己编一个时刻。
   用户在几个时刻之间犹豫时可以先 recall_preferences 看看这人历来约几点，
   但那只是参考样本，不能替用户决定，也不要把它的数字念给用户听。
   用户问"约的什么时候""上次谁来过"必须 upcoming_appointments 查了再答。
   上门回来用 close_appointment 登记回访，它会把结论并进关联工单的进展。

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
    specialists: bool = SPECIALISTS_ENABLED,
) -> tuple[Agent, HelpdeskContext]:
    ctx = HelpdeskContext(index=index, store=store or TicketStore())
    chat_model = model or make_chat_model()
    # 只绑知识语料：服务字典进这里会污染答案检索。
    rag = RAGMiddleware(
        [index.knowledge],
        RAGMiddleware.Parameters(
            mode="agentic",
            top_k=5,
            score_threshold=KNOWLEDGE_SCORE_THRESHOLD,
        ),
    )
    # 专线共用同一个模型对象和同一个 ctx：模型只是通道，业务事实只有一份。
    extra_tools = await make_specialist_tools(ctx, model=chat_model) if specialists else []
    agent = Agent(
        name=name,
        system_prompt=SYSTEM_PROMPT,
        model=chat_model,
        # Agent 不收集 middleware 的工具（只有托管路径
        # app/_service/_toolkit.py:233 会 await mw.list_tools()），所以这里自己并。
        toolkit=Toolkit(tools=[*build_tools(ctx), *await rag.list_tools(), *extra_tools]),
        middlewares=[
            ToolSurfaceMiddleware(),
            IntentRouterMiddleware(ctx.store),
            rag,
            IntakeInjectionMiddleware(),
            AskOutLoudMiddleware(),
            ReplyBudgetControlMiddleware(token_budget=token_budget),
            UniqueToolCallIds(),
        ],
        react_config=ReActConfig(max_iters=max_iters),
        context_config=ContextConfig(
            trigger_ratio=CONTEXT_TRIGGER_RATIO,
            tool_result_limit=TOOL_RESULT_LIMIT,
        ),
        injection_config=InjectionConfig(timezone=TIMEZONE),
    )
    return agent, ctx


class LocalClockAgent(Agent):
    """只把注入时区带回本地，其余一律沿用框架默认。

    接缝证据：托管路径每轮重组 agent 时只传 `context_config` 与 `react_config`
    （`app/_service/_chat.py:1109-1120`），`AgentData` 里也没有 injection_config 这个
    字段 —— 于是前端会话拿到的"现在几点"永远是 UTC 默认值。`custom_agent_cls`
    是唯一能改它的入口，而预约窗口不能差这八小时。
    """

    def __init__(self, *args: object, **kwargs: object) -> None:
        kwargs.setdefault("injection_config", InjectionConfig(timezone=TIMEZONE))
        super().__init__(*args, **kwargs)  # type: ignore[arg-type]
