"""
客户健康度分析器

基于客户行为统计，分析健康度并生成续费/流失预警文案。
"""

from typing import Dict, Any, Optional


class HealthAnalyzer:
    """客户健康度分析器"""

    def __init__(self, llm=None):
        self.llm = llm

    def analyze(self, customer_id: str) -> Dict[str, Any]:
        """分析客户健康度"""
        from services.customer_service import CustomerService
        return CustomerService().get_customer_health(customer_id)

    async def generate_insight_message(self, customer_id: str) -> str:
        """生成客户洞察文案（续费/流失预警）"""
        health = self.analyze(customer_id)

        if self.llm:
            try:
                prompt = f"""
你是一个客户成功经理，请根据以下客户健康度数据生成一段简洁的洞察结论。

客户健康度数据：
- 健康评分：{health.get('health_score')} / 100
- 健康等级：{health.get('health_level')}
- 流失风险：{health.get('churn_risk')}
- 近30天工单数：{health.get('statistics', {}).get('ticket_created_count', 0)}
- 近30天咨询数：{health.get('statistics', {}).get('consultation_count', 0)}
- 距离上次活跃天数：{health.get('statistics', {}).get('days_since_last_activity')}

要求：
1. 若流失风险为"高"，给出续费挽留建议；风险"低"则肯定客户黏性
2. 语言专业、简洁
3. 80 字以内
"""
                response = await self.llm.ainvoke([{"role": "user", "content": prompt}])
                return response.content.strip()
            except Exception as e:
                print(f"LLM 生成洞察失败: {e}")

        # 兜底文案
        risk = health.get('churn_risk', '未知')
        score = health.get('health_score', 0)
        if risk == "高":
            return f"客户健康度 {score} 分，流失风险较高，建议客户成功经理主动触达，排查近期待解决工单并给出续费方案。"
        if risk == "中":
            return f"客户健康度 {score} 分，流失风险中等，建议关注工单解决质量，适时回访。"
        return f"客户健康度 {score} 分，黏性良好，可考虑挖掘增购/续约机会。"
