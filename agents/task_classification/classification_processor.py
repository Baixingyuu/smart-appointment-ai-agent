"""
分类流程处理器

协调任务分类、状态管理与路由的完整流程。
"""

from typing import AsyncGenerator
from .task_classifier import TaskClassifier
from .state_manager import StateManager
from .agent_router import AgentRouter
from .unrelated_handler import UnrelatedHandler


class ClassificationProcessor:
    """分类流程处理器"""

    def __init__(self, task_classifier: TaskClassifier, state_manager: StateManager,
                 agent_router: AgentRouter, unrelated_handler: UnrelatedHandler):
        self.task_classifier = task_classifier
        self.state_manager = state_manager
        self.agent_router = agent_router
        self.unrelated_handler = unrelated_handler

    async def process_task_stream(self, task: str) -> AsyncGenerator[str, None]:
        """流式处理任务分类和路由"""
        try:
            if self.state_manager.should_classify():
                category = await self.task_classifier.classify_task(task)

                if category == "ticketing" and self.agent_router.ticketing_agent:
                    async for token in self.agent_router.route_to_ticketing(task):
                        yield token
                elif category == "consultation" and self.agent_router.consultant_agent:
                    async for token in self.agent_router.route_to_consultation(task):
                        yield token
                else:
                    async for token in self.agent_router.handle_unsupported_task(category):
                        yield token
            else:
                async for token in self.agent_router.route_by_state(task):
                    yield token
        except Exception as e:
            yield f"[ERROR]处理任务时发生错误: {str(e)}"
            self.state_manager.force_reset()

    async def process_task_sync(self, task: str) -> str:
        """同步处理任务分类和路由"""
        try:
            if self.state_manager.should_classify():
                category = await self.task_classifier.classify_task(task)

                if category == "ticketing" and self.agent_router.ticketing_agent:
                    self.state_manager.transition_to_ticketing()
                    result = ""
                    async for token in self.agent_router.ticketing_agent.run_stream(user_input=task):
                        result += token
                    return result
                elif category == "consultation" and self.agent_router.consultant_agent:
                    self.state_manager.transition_to_consultation()
                    async with self.agent_router.consultant_agent as agent:
                        return await agent.consult(task)
                else:
                    return "抱歉，我暂时无法处理该请求。请咨询产品功能或提交工单。"
            else:
                if self.state_manager.is_in_ticketing_flow():
                    result = ""
                    async for token in self.agent_router.ticketing_agent.run_stream(user_input=task):
                        result += token
                    return result
                elif self.state_manager.is_in_consultation_flow():
                    async with self.agent_router.consultant_agent as agent:
                        return await agent.consult(task)
        except Exception as e:
            self.state_manager.force_reset()
            return f"处理任务时发生错误: {str(e)}"

    def get_current_state_info(self) -> dict:
        return {
            'current_state': self.state_manager.get_current_state(),
            'state_description': self.state_manager.get_state_description(),
            'available_services': self.agent_router.get_available_services(),
            'can_classify': self.state_manager.should_classify(),
        }

    def reset_conversation(self) -> None:
        self.state_manager.force_reset()
        self.unrelated_handler.reset_reply_rotation()

    async def handle_unrelated_request(self, user_input: str, async_mode: bool = True):
        if async_mode:
            async for token in self.unrelated_handler.handle_unrelated_async(user_input):
                yield token
        else:
            result = await self.unrelated_handler.handle_unrelated_sync(user_input)
            yield result
