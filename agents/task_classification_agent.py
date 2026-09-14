"""
任务分类 Agent 主控制器

职责：
1. 初始化分类组件
2. 提供统一的任务分类接口
3. 协调与其他 Agent 的路由
"""

from dotenv import load_dotenv
from config.model_provider import create_chat_model
from config.constants import SharedState, StateEnum
from .task_classification import (
    TaskClassifier,
    StateManager,
    AgentRouter,
    UnrelatedHandler,
    ClassificationProcessor,
)

load_dotenv()


class TaskClassificationAgent:
    """任务分类 Agent 主控制器"""

    def __init__(self, ticketing_agent, consultant_agent):
        self.ticketing_agent = ticketing_agent
        self.consultant_agent = consultant_agent

        self.llm = self._initialize_llm()

        self.state_manager = StateManager(SharedState())
        self.task_classifier = TaskClassifier(self.llm)
        self.agent_router = AgentRouter(
            ticketing_agent,
            consultant_agent,
            self.state_manager,
        )
        self.unrelated_handler = UnrelatedHandler(self.state_manager)
        self.classification_processor = ClassificationProcessor(
            self.task_classifier,
            self.state_manager,
            self.agent_router,
            self.unrelated_handler,
        )

        self._setup_callbacks()
        self.state = self.state_manager.state

    def _initialize_llm(self):
        return create_chat_model(temperature=0)

    def _setup_callbacks(self):
        if self.ticketing_agent and hasattr(self.ticketing_agent, 'unrelated_callback'):
            self.ticketing_agent.unrelated_callback = self.handle_unrelated
        if self.consultant_agent and hasattr(self.consultant_agent, 'set_unrelated_callback'):
            self.consultant_agent.set_unrelated_callback(self.handle_unrelated_async)

    async def classify_task(self, task):
        return await self.classification_processor.process_task_sync(task)

    async def classify_task_stream(self, task):
        async for token in self.classification_processor.process_task_stream(task):
            yield token

    async def handle_unrelated(self, user_input):
        result = ""
        async for token in self.classification_processor.process_task_stream(user_input):
            result += token
        return result

    async def handle_unrelated_async(self, user_input):
        async for token in self.classification_processor.process_task_stream(user_input):
            yield token

    def get_classification_info(self):
        return self.classification_processor.get_current_state_info()

    def reset_conversation(self):
        self.classification_processor.reset_conversation()

    def set_business_context(self, service_name: str = "技术支持服务"):
        self.unrelated_handler.set_business_context(service_name)
