"""
提示词构建器

负责构建咨询流程的各类提示词。
"""

from typing import List, Dict, Any


class PromptBuilder:
    """提示词构建器"""

    def __init__(self):
        self.system_prompt = self._create_system_prompt()
        self.classification_prompt_template = self._create_classification_prompt_template()

    def _create_system_prompt(self) -> str:
        return (
            "你是一个企业技术支持中心的客服助手，负责为客户解答关于产品功能、套餐、"
            "支持方式、订阅政策等相关问题。"
            "我会为你提供相关的知识库信息，请基于这些信息来回答用户的问题。"
            "如果知识库中没有相关信息，请提供合理的兜底回答，比如："
            "- 对于价格/套餐问题：建议联系销售或客户成功经理获取详细报价。"
            "- 对于技术细节问题：建议提交工单，由工程师进一步排查。"
            "请用专业、礼貌、简洁的语言回复用户。"
            "如果用户的问题与技术支持完全无关（如天气、股票、新闻等），请礼貌地告知用户你只能回答产品技术支持相关问题。"
            "回答时要自然流畅，不要明显地表现出是在查阅资料。"
        )

    def _create_classification_prompt_template(self) -> str:
        return (
            "你是一个分类器，判断用户输入是否是关于产品技术支持的咨询类问题。\n"
            "咨询类问题包括：产品功能、价格、套餐、支持方式、订阅政策、使用方法等。\n"
            "非咨询类问题包括：提交工单（我要报障、帮我看看问题、系统报错了等）、查询工单、天气、股票、新闻等无关话题。\n"
            "如果是咨询类问题，回答'YES'。如果是提交工单类或完全无关问题，回答'NO'。\n"
            "只回答YES或NO。\n\n"
            "用户输入：{user_input}"
        )

    def build_consultation_prompt(self, user_input: str, knowledge_docs: List[Dict[str, Any]]) -> str:
        context = self._build_knowledge_context(knowledge_docs)
        return f"{self.system_prompt}\n\n{context}\n用户问题：{user_input}\n\n请回答用户的问题。"

    def build_classification_prompt(self, user_input: str) -> str:
        return self.classification_prompt_template.format(user_input=user_input)

    def _build_knowledge_context(self, knowledge_docs: List[Dict[str, Any]]) -> str:
        if not knowledge_docs:
            return "没有找到直接相关的知识库信息，请基于你对企业技术支持服务的一般了解回答。"

        context = "\n以下是相关的知识库信息：\n"
        for i, doc in enumerate(knowledge_docs, 1):
            context += f"{i}. {doc['content']}\n"
        context += "\n请基于以上信息回答用户问题。如果知识库信息不足以回答问题，请基于你对技术支持服务的一般了解来补充回答。\n"

        return context
