"""
任务分类器

负责判断用户请求类型，将任务归类为：
- consultation（咨询任务）
- ticketing（工单任务）
- other（其他任务）
"""

from langchain.prompts import PromptTemplate
from langchain_core.language_models.chat_models import BaseChatModel
from typing import Dict, Any


class TaskClassifier:
    """任务分类器 - 使用 LLM 进行智能任务分类"""

    def __init__(self, llm: BaseChatModel):
        self.llm = llm
        self._initialize_prompt()
        self.chain = self.prompt | self.llm

    def _initialize_prompt(self):
        self.prompt = PromptTemplate(
            input_variables=["task"],
            template=(
                "你是一个企业技术支持系统的助手，负责对用户消息进行意图分类。\n"
                "用户可能会咨询产品功能、价格、套餐、使用方法等，这类任务归类为咨询任务。\n"
                "用户可能会提交故障、报告问题、请求技术支持（如\"接口报错了\"\"系统打不开\"\"帮我看看这个问题\"），这类任务归类为工单任务。\n"
                "如果输入的任务与上述都无关（如聊天、天气等），归类为其它任务。\n"
                "请将以下任务归类为以下类别，输出只能选择以下之一：\n"
                "1. consultation（咨询任务）\n"
                "2. ticketing（工单任务）\n"
                "3. other（其它任务）\n"
                "只返回类别英文名。\n\n"
                "举例说明：假如task为'企业版支持私有化部署吗'，则输出consultation。\n"
                "假如输入为'我们的API接口一直返回500错误，影响线上业务'，则输出ticketing。\n"
                "以下是本次归类任务:\n"
                "任务内容：{task}"
            )
        )

    async def classify_task(self, task: str) -> str:
        """分类任务，返回 'consultation' / 'ticketing' / 'other'"""
        try:
            category_msg = await self.chain.ainvoke({"task": task})
            category = category_msg.content.strip().lower()

            valid = {'consultation', 'ticketing', 'other'}
            if category not in valid:
                return 'other'

            return category
        except Exception as e:
            print(f"任务分类失败: {str(e)}")
            return 'other'

    def get_category_description(self, category: str) -> str:
        descriptions = {
            'consultation': '咨询任务 - 用户咨询产品功能、价格、套餐等',
            'ticketing': '工单任务 - 用户提交故障或请求技术支持',
            'other': '其他任务 - 与技术支持无关的请求',
        }
        return descriptions.get(category, '未知任务类型')
