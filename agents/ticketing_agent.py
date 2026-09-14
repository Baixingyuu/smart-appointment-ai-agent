"""
工单 Agent 主控制器

职责：
1. 初始化工单处理组件
2. 管理会话状态
3. 协调整个工单创建流程
"""

from dotenv import load_dotenv
import uuid
from langchain_core.chat_history import InMemoryChatMessageHistory
from config.model_provider import create_chat_model
from .ticketing import (
    InputParser,
    EngineerFinder,
    TicketProcessor,
    MessageBuilder,
    TicketDatabase,
)

load_dotenv()


class TicketingAgent:
    """工单 Agent 主控制器"""

    def __init__(self, session_id=None, unrelated_callback=None):
        self.session_id = session_id or str(uuid.uuid4())
        self.unrelated_callback = unrelated_callback
        self.state = None

        self.llm = self._initialize_llm()

        self.input_parser = InputParser(self.llm)
        self.engineer_finder = EngineerFinder()
        self.message_builder = MessageBuilder()
        self.ticket_database = TicketDatabase()
        self.ticket_processor = TicketProcessor(
            self.input_parser,
            self.engineer_finder,
            self.message_builder,
            self.ticket_database,
            self.llm,
        )

        self.chats_by_session_id = {}
        self.chat_history = self._get_chat_history(self.session_id)
        self.reset()

    def _initialize_llm(self):
        return create_chat_model(temperature=0)

    def _get_chat_history(self, session_id: str) -> InMemoryChatMessageHistory:
        chat_history = self.chats_by_session_id.get(session_id)
        if chat_history is None:
            chat_history = InMemoryChatMessageHistory()
            self.chats_by_session_id[session_id] = chat_history
        return chat_history

    def reset(self):
        """重置工单历史与状态"""
        self.ticket_history = {
            "title": None,
            "description": None,
            "category": None,
            "priority": None,
            "engineer_name": None,
        }
        self.finished = False
        self.chat_history.clear()

    def set_shared_state(self, shared_state):
        self.state = shared_state

    def set_unrelated_callback(self, callback):
        self.unrelated_callback = callback

    async def run_stream(self, user_input=None):
        """流式处理用户工单请求的主函数"""
        if user_input is None:
            user_input = input("用户：")

        # 1. 解析用户输入
        ai_content = ""
        for token in self.input_parser.parse_stream(user_input, self.chat_history):
            ai_content += token

        try:
            data = self.input_parser.parse_data(ai_content)
            self.finished = self.ticket_processor.update_history_from_data(self.ticket_history, data)

            # 2. 处理与工单无关的请求
            if data.get("unrelated", False):
                if self.state:
                    from config.constants import StateEnum
                    self.state.value = StateEnum.CLASSIFY

                async for token in self.ticket_processor.handle_unrelated_request(
                    user_input, self.unrelated_callback, self.state
                ):
                    yield token
                return

            # 3. 处理信息完整的情况
            if self.finished:
                customer_id = self._get_customer_id()

                async for token in self.ticket_processor.handle_complete_ticket(
                    self.ticket_history, customer_id, self.session_id
                ):
                    yield token

                self._reset_state_after_ticket()
                return

            # 4. 处理信息不完整的情况
            async for token in self.ticket_processor.handle_incomplete_info(data, self.ticket_history):
                yield token

        except Exception as e:
            yield self.message_builder.create_parse_error_message()

    def _get_customer_id(self) -> int:
        """获取默认客户 ID"""
        from services.customer_service import CustomerService
        customer = CustomerService().get_default_customer()
        return customer['id']

    def _reset_state_after_ticket(self):
        self.reset()
        if self.state:
            from config.constants import StateEnum
            self.state.value = StateEnum.CLASSIFY
