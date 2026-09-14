"""
工单信息解析器

负责解析用户输入，提取工单相关信息（标题、描述、类型、优先级、期望工程师）。
"""

import json
from typing import Dict, Any, Generator
from langchain.prompts import PromptTemplate
from langchain_core.chat_history import InMemoryChatMessageHistory
from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import HumanMessage, AIMessage


class InputParser:
    """用户输入解析器 - 提取工单信息"""

    def __init__(self, llm: BaseChatModel):
        self.llm = llm
        self.prompt = self._create_prompt_template()
        self.chain = self.prompt | self.llm

    def _create_prompt_template(self) -> PromptTemplate:
        """创建工单信息提取的 Prompt 模板"""
        from config.time_config import time_config
        current_date = time_config.current_date_str()
        current_datetime = time_config.current_datetime_str()

        return PromptTemplate(
            input_variables=["history", "user_input"],
            template=(
                "你是一个技术支持工单系统的助手，负责从用户描述中提取工单信息。\n"
                f"当前日期是{current_date}，当前北京时间是{current_datetime}。\n"
                "当前已知信息：{history}\n"
                "用户输入：{user_input}\n"
                "重要：请你只输出纯JSON格式，不要添加任何markdown标记，直接输出JSON：\n"
                "{{\n"
                '  "title": "工单标题（简要概括问题，若用户未给出可自行概括，不超过20字）",\n'
                '  "description": "问题详细描述（必填，包含问题现象、影响范围等）",\n'
                '  "category": "工单类型（incident故障/consultation咨询/request服务请求/change变更请求，根据语义判断）",\n'
                '  "priority": "优先级（P0系统瘫痪或核心业务中断/P1核心功能不可用/P2部分功能异常/P3一般问题，根据影响程度判断）",\n'
                '  "engineer_name": "用户指定的工程师姓名（若用户明确提到工程师名字，如张伟、李娜，否则为未知）",\n'
                '  "info_complete": "若description不为未知则为true，否则为false",\n'
                '  "unrelated": "若用户的问题与技术支持/工单完全无关（如聊天、天气等）则为true，否则为false",\n'
                '  "missing_info": "若info_complete为false，列出缺少的关键信息，如[description]"\n'
                "}}\n"
                "判断逻辑：\n"
                "1. description 是唯一必需信息，请务必从用户描述中提取或概括\n"
                "2. priority 根据影响程度判断：系统瘫痪/核心业务中断=P0，核心功能不可用=P1，部分异常=P2，一般问题=P3\n"
                "3. 若用户明确指定工程师姓名，提取到engineer_name字段\n"
                "再次强调：只输出纯JSON，不要有任何代码块标记或其他文字。"
            )
        )

    def parse_stream(self, user_input: str, chat_history: InMemoryChatMessageHistory) -> Generator[str, None, str]:
        """流式解析用户输入"""
        chat_history.add_message(HumanMessage(content=user_input))

        history_str = "\n".join(
            [f"用户：{m.content}" if m.type == "human" else f"助手：{m.content}"
             for m in chat_history.messages]
        )

        response_stream = self.chain.stream({"history": history_str, "user_input": user_input})
        ai_content = ""

        for chunk in response_stream:
            token = chunk.content if hasattr(chunk, "content") else str(chunk)
            ai_content += token
            yield token

        chat_history.add_message(AIMessage(content=ai_content))
        return ai_content

    def parse_data(self, ai_content: str) -> Dict[str, Any]:
        """解析 AI 返回的 JSON 数据"""
        try:
            return json.loads(ai_content)
        except json.JSONDecodeError:
            return {
                "title": "未知",
                "description": "未知",
                "category": "incident",
                "priority": "P3",
                "engineer_name": "未知",
                "info_complete": False,
                "unrelated": False,
                "missing_info": ["description"],
            }
