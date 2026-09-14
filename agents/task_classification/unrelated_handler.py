"""
无关请求处理器

负责处理与技术支持业务无关的请求，提供友好回复并引导回到业务轨道。
"""

from typing import AsyncGenerator
from .state_manager import StateManager


class UnrelatedHandler:
    """无关请求处理器"""

    def __init__(self, state_manager: StateManager):
        self.state_manager = state_manager
        self._default_replies = [
            "抱歉，我无法处理这个问题。我只能帮您咨询产品功能或提交技术支持工单。请问您需要了解什么，或遇到了什么问题吗？",
            "很抱歉，我专门负责技术支持服务。如果您想咨询产品功能或提交工单，我很乐意为您提供帮助！",
            "对不起，这个问题超出了我的服务范围。我主要协助处理技术咨询和工单提交，有什么可以为您服务的吗？",
        ]
        self._reply_index = 0

    async def handle_unrelated_sync(self, user_input: str) -> str:
        self.state_manager.reset_to_classify()
        return self._get_next_reply()

    async def handle_unrelated_async(self, user_input: str) -> AsyncGenerator[str, None]:
        self.state_manager.reset_to_classify()
        reply = self._get_next_reply()
        yield "[REPLY][分类机器人]"
        for char in reply:
            yield char

    def _get_next_reply(self) -> str:
        reply = self._default_replies[self._reply_index]
        self._reply_index = (self._reply_index + 1) % len(self._default_replies)
        return reply

    def add_custom_reply(self, reply: str) -> None:
        if reply and reply not in self._default_replies:
            self._default_replies.append(reply)

    def set_business_context(self, service_name: str = "技术支持服务") -> None:
        self._default_replies = [
            f"抱歉，我无法处理这个问题。我只能帮您处理{service_name}相关的咨询和工单。请问您需要了解什么，或遇到了什么问题吗？",
            f"很抱歉，我专门负责{service_name}。如果您想了解我们的产品或提交工单，我很乐意为您提供帮助！",
            f"对不起，这个问题超出了我的服务范围。我主要协助处理{service_name}，有什么可以为您服务的吗？",
        ]

    def get_available_replies(self) -> list:
        return self._default_replies.copy()

    def reset_reply_rotation(self) -> None:
        self._reply_index = 0
