"""
工单系统 - Agent 工具定义

本文件用 LangChain 的 @tool 装饰器把各 Agent 的核心能力封装为可被 LLM 通过
Tool Calling / Function Calling 调用的工具。每个工具都有明确的:
  - name        工具名（LLM 看到的函数名）
  - description 工具用途（写在 docstring 首段，LLM 据此决定何时调用）
  - args_schema 由函数签名 + 类型注解自动推导
  - 实现        内部通过 Services/DB 层访问数据，保持分层架构

四个 Agent 的工具归属见文件末尾的分组表。
"""

from typing import List, Dict, Any, Optional
from langchain_core.tools import tool


# ============================================================
# Task Classification Agent - 工具
# ============================================================

@tool
def classify_intent(user_input: str) -> str:
    """意图分类工具 - 判断用户输入属于哪类任务。

    当用户发来任意自然语言消息时优先调用，用于决定后续路由到哪个专业 Agent。
    输入: 用户原始文本。
    输出: consultation | ticketing | other 之一。
    场景: Task Classification Agent 的第一步；对话状态为 CLASSIFY 时必调。
    """
    # 实际实现由 TaskClassifier（LLM Prompt）完成，此处为工具签名占位。
    # 运行时会委派给 agents/task_classification/task_classifier.py 的 LLM 链。
    from config.model_provider import create_chat_model
    from agents.task_classification.task_classifier import TaskClassifier
    import asyncio
    llm = create_chat_model(temperature=0)
    classifier = TaskClassifier(llm)
    return asyncio.run(classifier.classify_task(user_input))


@tool
def route_to_agent(intent: str) -> str:
    """路由分发工具 - 根据意图将任务分发给对应 Agent。

    在 classify_intent 之后调用，完成分发动作。
    输入: intent（consultation/ticketing/other）。
    输出: 目标 Agent 名及下一步动作描述。
    场景: Task Classification Agent 内部的状态机流转；命中 other 时触发兜底话术。
    """
    mapping = {
        "consultation": "ConsultantAgent.consult_stream",
        "ticketing": "TicketingAgent.run_stream",
        "other": "UnrelatedHandler.handle",
    }
    return mapping.get(intent, "UnrelatedHandler.handle")


# ============================================================
# Consultant Agent - 工具
# ============================================================

@tool
def search_knowledge_base(query: str, top_k: int = 3) -> List[Dict[str, Any]]:
    """知识库检索工具 - 从产品文档知识库中检索与问题最相关的片段。

    基于 FAISS 向量检索 + Embedding 相似度，返回 top_k 条知识文档。
    输入: query（用户问题）、top_k（返回条数，默认3）。
    输出: 知识文档列表，每条含 content/category/score。
    场景: Consultant Agent 收到咨询类问题时必调；检索不到时后续生成兜底回答。
    定义方式: 内部调用 services/knowledge_service.py 的 FAISS 检索，Embedding 走
             services/text_embedding.py，命中结果带相似度分数供排序。
    """
    import asyncio
    from agents.consultant.knowledge_retriever import KnowledgeRetriever
    retriever = KnowledgeRetriever()
    return asyncio.run(retriever.search_knowledge(query, top_k=top_k))


@tool
def generate_grounded_answer(question: str, knowledge_docs: List[Dict[str, Any]]) -> str:
    """ grounded 回答生成工具 - 基于检索到的知识片段生成最终回答。

    强制要求回答有据可依：优先使用 knowledge_docs 中的内容，缺失时走兜底话术而非编造。
    输入: question（用户问题）、knowledge_docs（上一步检索结果）。
    输出: 自然语言回答（支持流式，内部通过 ResponseGenerator 实现）。
    场景: Consultant Agent 在检索完成后调用；是 RAG 链路的第二步。
    定义方式: Prompt 由 agents/consultant/prompt_builder.py 构建，LLM 由
             config/model_provider.create_chat_model 提供，流式通过 AsyncGenerator 输出。
    """
    import asyncio
    from agents.consultant.response_generator import ResponseGenerator
    from agents.consultant.prompt_builder import PromptBuilder
    from config.model_provider import create_chat_model
    llm = create_chat_model(temperature=0.3)
    builder = PromptBuilder()
    generator = ResponseGenerator(llm)
    # 简化为非流式调用，流式版本在 consultant_agent.py 中通过 generate_response_stream 实现
    prompt = builder.build_consultation_prompt(question, knowledge_docs)
    return asyncio.run(llm.ainvoke([{"role": "user", "content": prompt}])).content


# ============================================================
# Ticketing Agent - 工具
# ============================================================

@tool
def parse_ticket_info(user_input: str, history: str = "") -> Dict[str, Any]:
    """工单信息抽取工具 - 从用户描述中抽取结构化工单字段。

    抽取字段: title / description / category(incident/consultation/request/change)
             / priority(P0-P3) / engineer_name / info_complete / unrelated。
    输入: user_input（当前轮用户输入）、history（多轮对话拼接，供补全上下文）。
    输出: JSON 结构化工单信息。
    场景: Ticketing Agent 的第一步；每轮用户输入后都调用，判断信息是否齐全以决定
          是追问还是进入匹配/建单。由 LLM 驱动，Prompt 在 agents/ticketing/input_parser.py 中定义。
    定义方式: LangChain PromptTemplate + LLM 链，流式解析后通过 json.loads 落库。
    """
    from config.model_provider import create_chat_model
    from agents.ticketing.input_parser import InputParser
    from langchain_core.chat_history import InMemoryChatMessageHistory
    llm = create_chat_model(temperature=0)
    parser = InputParser(llm)
    chat_history = InMemoryChatMessageHistory()
    # 为简化，单轮解析；多轮场景由 TicketingAgent 传入完整 chat_history
    ai_content = "".join(parser.parse_stream(user_input, chat_history))
    return parser.parse_data(ai_content)


@tool
def find_engineer_by_specialty(priority: str, description: str, category: str = "incident",
                                 engineer_name: Optional[str] = None) -> Optional[Dict[str, Any]]:
    """工程师匹配工具 - 按等级过滤 + 专长相似度排序匹配最合适的工程师。

    二级匹配策略: (1) 按 priority 过滤工程师等级（P0仅expert，P1需expert/senior等，
    映射在 config/constants.PRIORITY_LEVEL_MAP）；(2) 用 Embedding 计算
    "category + description" 与工程师 specialty 的相似度，取 Top-1。
    输入: priority / description / category / 可选的 engineer_name（用户指定时直接查找）。
    输出: 匹配到的工程师 dict（含 id/name/level/specialty），无匹配时返回 None。
    场景: Ticketing Agent 在工单信息齐全后调用；是派单决策的核心。
    定义方式: 封装 agents/ticketing/engineer_finder.py 的 EngineerFinder，
             专长相似度通过 services/text_embedding.find_best_match_indices 实现。
    """
    from agents.ticketing.engineer_finder import EngineerFinder
    finder = EngineerFinder()
    ticket_info = {
        "priority": priority,
        "description": description,
        "category": category,
        "engineer_name": engineer_name or "未知",
    }
    return finder.find_engineer(ticket_info)


@tool
def create_and_assign_ticket(customer_id: int, title: str, description: str,
                              category: str = "incident", priority: str = "P3",
                              engineer_id: Optional[int] = None) -> Dict[str, Any]:
    """工单创建与分派工具 - 创建工单并分派给指定工程师。

    输入: customer_id / title / description / category / priority / engineer_id。
    输出: 创建后的工单 dict（含 ticket_no/status/engineer_name）。
    场景: Ticketing Agent 在匹配到工程师后调用，完成建单+分派+行为记录的闭环。
    定义方式: 封装 agents/ticketing/ticket_database.py 的 TicketDatabase，
             内部通过 services/ticket_service + services/customer_service 落库，
             工单号格式 TK-YYYYMMDD-XXXX，状态自动置为 assigned。
    """
    from agents.ticketing.ticket_database import TicketDatabase
    db = TicketDatabase()
    ticket = db.create_ticket(
        {"title": title, "description": description, "category": category, "priority": priority},
        customer_id=customer_id,
    )
    if ticket and engineer_id:
        db.assign_ticket(ticket["id"], engineer_id)
        ticket["engineer_id"] = engineer_id
    if ticket:
        db.record_activity(
            customer_id=str(customer_id),
            action_type="ticket_created",
            action_data={"ticket_no": ticket["ticket_no"], "category": category, "priority": priority},
            ticket_id=ticket["id"],
        )
    return ticket or {}


# ============================================================
# Customer Insight Agent - 工具
# ============================================================

@tool
def get_customer_health(customer_id: str = "default_customer") -> Dict[str, Any]:
    """客户健康度查询工具 - 基于近30天行为计算健康分与流失风险。

    统计维度: 工单数 / 咨询数 / 活跃天数 / 平均响应，输出 health_score(0-100)、
    health_level(健康/亚健康/风险)、churn_risk(低/中/高) 及 statistics 明细。
    输入: customer_id。
    输出: 健康度报告 dict。
    场景: Customer Insight Agent 的核心；用于客户列表页、健康度看板、续费预警。
    定义方式: 封装 services/customer_service.get_customer_health，数据源为
             db/repositories/customer_activity_repository 的行为埋点。
    """
    from services.customer_service import CustomerService
    service = CustomerService()
    return service.get_customer_health(customer_id)


@tool
def analyze_customer_trend(customer_id: str = "default_customer") -> str:
    """客户趋势洞察工具 - 生成面向运营/客服的自然语言洞察与建议。

    在健康度分数基础上，由 LLM 生成可读的洞察文案（如"近7天工单激增，建议主动回访"）。
    输入: customer_id。
    输出: 洞察文案字符串。
    场景: Customer Insight Agent 在健康度查询后按需调用，用于预警推送或人工复核。
    定义方式: 封装 agents/customer_insight/health_analyzer.py 的 HealthAnalyzer，
             内部通过 LLM 对统计数据做归因与建议生成。
    """
    import asyncio
    from agents.customer_insight.health_analyzer import HealthAnalyzer
    from config.model_provider import create_chat_model
    llm = create_chat_model(temperature=0.7)
    analyzer = HealthAnalyzer(llm)
    return asyncio.run(analyzer.generate_insight_message(customer_id)) or ""


# ============================================================
# 工具分组总览（供面试讲解）
# ============================================================
# Task Classification Agent : classify_intent, route_to_agent
# Consultant Agent          : search_knowledge_base, generate_grounded_answer
# Ticketing Agent           : parse_ticket_info, find_engineer_by_specialty, create_and_assign_ticket
# Customer Insight Agent    : get_customer_health, analyze_customer_trend
# 共 9 个工具，全部通过 @tool 暴露给 LLM 做 Tool Calling，定义集中在本文件，
# 调用分散在各 Agent 的主流程中，保持“定义集中、调用分散”的可维护性。
