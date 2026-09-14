"""
智能体路由器

根据分类结果将请求路由到对应的 Agent。
"""

from typing import Any, AsyncGenerator
from .state_manager import StateManager


class AgentRouter:
    """智能体路由器"""

    def __init__(self, ticketing_agent: Any, consultant_agent: Any, state_manager: StateManager):
        self.ticketing_agent = ticketing_agent
        self.consultant_agent = consultant_agent
        self.state_manager = state_manager
        self._setup_agent_states()

    def _setup_agent_states(self):
        if self.ticketing_agent and hasattr(self.ticketing_agent, 'set_shared_state'):
            self.ticketing_agent.set_shared_state(self.state_manager.state)
        if self.consultant_agent and hasattr(self.consultant_agent, 'set_shared_state'):
            self.consultant_agent.set_shared_state(self.state_manager.state)

    async def route_to_ticketing(self, task: str) -> AsyncGenerator[str, None]:
        """路由到工单 Agent"""
        if not self.ticketing_agent:
            yield "[ERROR]工单服务暂时不可用"
            return

        self.state_manager.transition_to_ticketing()
        yield "[THOUGHT][分类机器人] 分类机器人：这是工单任务，转给工单机器人处理。"

        try:
            async for token in self.ticketing_agent.run_stream(user_input=task):
                yield token
        except Exception as e:
            yield f"[ERROR]工单处理失败: {str(e)}"
            self.state_manager.reset_to_classify()

    async def route_to_consultation(self, task: str) -> AsyncGenerator[str, None]:
        """路由到咨询 Agent"""
        if not self.consultant_agent:
            yield "[ERROR]咨询服务暂时不可用"
            return

        self.state_manager.transition_to_consultation()
        yield "[THOUGHT][分类机器人] 分类机器人：这是咨询任务，转给咨询机器人处理。"

        try:
            async with self.consultant_agent as agent:
                async for token in agent.consult_stream(task):
                    yield token
        except Exception as e:
            yield f"[ERROR]咨询处理失败: {str(e)}"
            self.state_manager.reset_to_classify()

    async def handle_unsupported_task(self, category: str) -> AsyncGenerator[str, None]:
        reply = "抱歉，我暂时无法处理该请求。请咨询产品功能或提交工单。"
        yield "[REPLY][分类机器人]"
        for char in reply:
            yield char

    async def route_by_state(self, task: str) -> AsyncGenerator[str, None]:
        if self.state_manager.is_in_ticketing_flow():
            async for token in self.ticketing_agent.run_stream(user_input=task):
                yield token
        elif self.state_manager.is_in_consultation_flow():
            async with self.consultant_agent as agent:
                async for token in agent.consult_stream(task):
                    yield token
        else:
            self.state_manager.reset_to_classify()
            yield "[ERROR]会话状态异常，已重置。请重新开始对话。"

    def get_available_services(self) -> list:
        services = []
        if self.ticketing_agent:
            services.append("工单服务")
        if self.consultant_agent:
            services.append("咨询服务")
        return services
