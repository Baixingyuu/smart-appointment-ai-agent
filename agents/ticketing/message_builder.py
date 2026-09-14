"""
消息构建器

负责构建工单流程中的各类响应消息。
"""

from typing import Dict, Any, List


class MessageBuilder:
    """消息构建器"""

    def __init__(self):
        self.missing_info_prompts = {
            "description": "请问您遇到的具体问题是什么？可以描述一下问题现象和影响范围吗？",
        }

    def create_ticket_success_message(self, ticket: Dict[str, Any], engineer: Dict[str, Any]) -> str:
        """创建工单创建成功消息"""
        return (
            f"\n机器人：工单已创建并分派！\n"
            f"工单号：{ticket['ticket_no']}\n"
            f"负责工程师：{engineer['name']}（{engineer.get('level', '')}）\n"
            f"优先级：{ticket['priority']}\n"
        )

    def create_no_engineer_message(self) -> str:
        """创建无匹配工程师消息"""
        return "\n机器人：抱歉，当前没有匹配的工程师承接该工单，请稍后重试或联系客户成功经理。\n"

    def create_engineer_not_found_message(self, engineer_name: str) -> str:
        """创建工程师不存在消息"""
        return f"\n机器人：抱歉，没有找到名为'{engineer_name}'的工程师，请确认姓名后重试。\n"

    def create_missing_info_questions(self, missing_info: List[str]) -> str:
        """根据缺失信息创建追问"""
        questions = [self.missing_info_prompts.get(field, f"请补充{field}信息") for field in missing_info]
        return "\n" + " ".join(questions) + "\n"

    def create_unrelated_message(self) -> str:
        """创建无关请求消息"""
        return "[REPLY][工单机器人]抱歉，我无法处理这个问题。我只能帮您提交工单或查询工单状态。\n"

    def create_parse_error_message(self) -> str:
        """创建解析错误消息"""
        return "[REPLY][工单机器人]\n机器人：解析失败，请重试。\n"

    def create_save_failure_message(self) -> str:
        """创建保存失败消息"""
        return "\n机器人：抱歉，工单创建失败，请重试。\n"
