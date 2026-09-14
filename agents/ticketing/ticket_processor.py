"""
工单流程处理器

负责协调整个工单创建流程：信息收集 -> 工程师匹配 -> 工单创建。
"""

from typing import Dict, Any, AsyncGenerator
from .input_parser import InputParser
from .engineer_finder import EngineerFinder
from .message_builder import MessageBuilder
from .ticket_database import TicketDatabase


class TicketProcessor:
    """工单流程处理器"""

    def __init__(self, input_parser: InputParser, engineer_finder: EngineerFinder,
                 message_builder: MessageBuilder, ticket_database: TicketDatabase,
                 llm=None):
        self.input_parser = input_parser
        self.engineer_finder = engineer_finder
        self.message_builder = message_builder
        self.ticket_database = ticket_database
        self.llm = llm

    def update_history_from_data(self, history: Dict[str, Any], data: Dict[str, Any]) -> bool:
        """从解析数据更新工单历史，返回信息是否完整"""
        for key in ["title", "description", "category", "priority", "engineer_name"]:
            if data.get(key) and data[key] != "未知":
                history[key] = data[key]

        # 必需信息：description
        has_description = bool(history.get('description')) and history['description'] != "未知"
        return has_description

    async def handle_unrelated_request(self, user_input: str, unrelated_callback, state) -> AsyncGenerator[str, None]:
        """处理与工单无关的请求"""
        if unrelated_callback:
            try:
                yield "[REPLY][工单机器人]和工单无关，已转交分类机器人处理\n"
                result = await unrelated_callback(user_input)
                if hasattr(result, '__aiter__'):
                    async for token in result:
                        yield token
                else:
                    yield result
            except Exception as e:
                yield f"[ERROR]处理请求时发生错误: {str(e)}\n"
                yield self.message_builder.create_unrelated_message()
        else:
            yield self.message_builder.create_unrelated_message()

    async def handle_complete_ticket(self, history: Dict[str, Any],
                                     customer_id: int, session_id: str) -> AsyncGenerator[str, None]:
        """处理工单信息完整的情况：匹配工程师并创建工单"""
        # 收集思考过程
        thought_msgs = []

        def collect(msg):
            thought_msgs.append(msg)

        engineer = self.engineer_finder.find_engineer(history, collect)

        for msg in thought_msgs:
            yield msg

        if not engineer:
            reply = self.message_builder.create_no_engineer_message()
            yield f"[REPLY][工单机器人]{reply}"
            return

        reply = await self._process_ticket_creation(engineer, history, customer_id, session_id)
        yield f"[REPLY][工单机器人]{reply}"

    async def _process_ticket_creation(self, engineer: Dict[str, Any],
                                       history: Dict[str, Any],
                                       customer_id: int, session_id: str) -> str:
        """创建工单并分派"""
        try:
            ticket = self.ticket_database.create_ticket(history, customer_id)
            if not ticket:
                return self.message_builder.create_save_failure_message()

            self.ticket_database.assign_ticket(ticket['id'], engineer['id'])

            ticket['engineer_name'] = engineer['name']

            self.ticket_database.record_activity(
                customer_id=str(customer_id),
                action_type='ticket_created',
                action_data={
                    'ticket_no': ticket['ticket_no'],
                    'category': history.get('category'),
                    'priority': history.get('priority'),
                },
                ticket_id=ticket['id'],
                session_id=session_id,
            )

            return self.message_builder.create_ticket_success_message(ticket, engineer)
        except Exception as e:
            print(f"工单创建失败: {e}")
            return self.message_builder.create_save_failure_message()

    async def handle_incomplete_info(self, data: Dict[str, Any], history: Dict[str, Any]) -> AsyncGenerator[str, None]:
        """处理信息不完整的情况"""
        missing = []
        if not history.get('description') or history.get('description') == "未知":
            missing.append('description')

        reply = self.message_builder.create_missing_info_questions(missing)
        yield f"[THOUGHT][工单机器人]工单信息不完整，缺少：{', '.join(missing)}，需要追问用户\n"
        yield f"[REPLY][工单机器人]{reply}"
