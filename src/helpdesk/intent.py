"""意图识别：一句话 → 5 类意图 + 置信度。

分类标准对齐 `eval/datasets/intent.json` 的 taxonomy（chitchat / knowledge /
incident / handoff / out_of_scope，单标签互斥，handoff 优先级最高）。实现用框架的
`generate_structured_output` 强制模型输出 JSON —— 这是一次独立的模型调用，与主
agent 的推理分开，所以分类结果能在路由之前拿到。

分类是路由的前置，不是提示词建议：判成 knowledge 就只给检索、判成 incident 就
走建单+派单+预约。分类错误的代价集中在「incident 误判成 knowledge → 漏建单」，
所以 intent 评测轴要重点盯 incident 的召回（`eval/datasets/intent.json` 180 条）。
"""
from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, Field

from agentscope.message import UserMsg
from agentscope.model import ChatModelBase

IntentLabel = Literal["chitchat", "knowledge", "incident", "handoff", "out_of_scope"]

INTENT_LABELS: tuple[str, ...] = (
    "chitchat",
    "knowledge",
    "incident",
    "handoff",
    "out_of_scope",
)


class IntentAnswer(BaseModel):
    intent: IntentLabel = Field(description="用户意图，5 类之一")
    confidence: float = Field(ge=0.0, le=1.0, description="分类置信度 0~1")


INTENT_PROMPT = """你是 IT 服务台的意图分类器。把用户这句话归到下面 5 类之一，并给置信度（0~1）。

分类标准：
- chitchat：寒暄、致谢、告别、与技术支持无关的闲聊（你好、谢谢、在吗、今天天气不错）。只做简短社交回应。
- knowledge：询问产品功能、使用方法、排查步骤或某个报错现象的成因（接口返回401怎么办、怎么重置密码、人工客服电话是多少）。问的是"怎么做/为什么/原因/是什么"。
- incident：报告已发生的故障、要求登记问题或明确要求建单（系统打不开了、帮我建个工单、线上订单创建失败、库存扣成负数了）。
- handoff：明确要求转人工、找真人客服或拒绝继续与机器人交互（我要找人工、转真人客服）。
- out_of_scope：与 IT 技术支持无关的任何请求（帮我写诗、推荐餐厅、算方程、讲个笑话、查天气、红烧肉怎么做）。

判定次序：
1. 明确要求转接人工/找真人/拒绝机器人 → handoff，优先级最高，哪怕同时描述了故障或咨询。只是索取人工电话、或问"人工能不能解决"，那是在提问，归 knowledge，不算 handoff。
2. 描述了已经发生的具体现象（系统打不开、数据错了、通知推了十几遍、库存变负），哪怕是吐槽或带"是不是bug/怎么办"的疑问语气 → incident；只有在单纯问"怎么做/为什么/原因/是什么"而没描述已发生现象时 → knowledge。
3. 寒暄致谢告别 → chitchat；与 IT 无关的具体请求（做饭、查天气、讲笑话、写诗、算题）→ out_of_scope。
4. 混合意图按主导诉求归一类。

只输出 JSON，不要别的。用户的话："""


async def classify_intent(model: ChatModelBase, text: str) -> IntentAnswer:
    """一次模型调用，把一句话分成 5 类之一。失败会抛 StructuredOutputError。"""
    result = await model.generate_structured_output(
        messages=[UserMsg(name="user", content=INTENT_PROMPT + text)],
        structured_model=IntentAnswer,
    )
    return IntentAnswer.model_validate(result.content)


__all__ = [
    "INTENT_LABELS",
    "INTENT_PROMPT",
    "IntentAnswer",
    "IntentLabel",
    "classify_intent",
]
