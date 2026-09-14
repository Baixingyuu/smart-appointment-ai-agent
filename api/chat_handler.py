"""
聊天处理入口

统一处理用户输入，路由到任务分类 Agent 进行意图识别与分发。

Agent 采用懒加载，避免模块导入时就创建 LLM 客户端（需要 .env 配置）。
"""

import uuid

# 全局 session_id，用于单客户演示场景
global_session_id = str(uuid.uuid4())

_task_agent = None


def _get_task_agent():
    """懒加载任务分类 Agent（首次真正处理用户输入时才创建）"""
    global _task_agent
    if _task_agent is None:
        from agents.task_classification_agent import TaskClassificationAgent
        from agents.ticketing_agent import TicketingAgent
        from agents.consultant_agent import ConsultantAgent

        _task_agent = TaskClassificationAgent(
            TicketingAgent(session_id=global_session_id),
            ConsultantAgent(session_id=global_session_id),
        )
    return _task_agent


async def ProcessUserInput_stream(user_input, state=None, context=None):
    """
    处理用户输入（流式）

    user_input: 用户输入
    state: 当前对话状态
    context: 多轮对话上下文
    返回: 流式 token
    """
    if context is None:
        context = {}

    task_agent = _get_task_agent()
    async for token in task_agent.classify_task_stream(user_input):
        yield token
