"""
客户洞察 Agent

负责分析客户健康度、生成续费/流失预警。
"""

import logging
from typing import Dict, Any, Optional
from config.model_provider import create_chat_model
from .customer_insight import HealthAnalyzer


class CustomerInsightAgent:
    """客户洞察 Agent"""

    def __init__(self):
        self.logger = logging.getLogger(__name__)
        self.llm = create_chat_model(temperature=0.7)
        self.health_analyzer = HealthAnalyzer(self.llm)

    def get_customer_health(self, customer_id: str = "default_customer") -> Dict[str, Any]:
        """获取客户健康度分析"""
        try:
            return self.health_analyzer.analyze(customer_id)
        except Exception as e:
            self.logger.error(f"客户健康度分析失败: {e}")
            return {
                'customer_id': customer_id,
                'health_score': 0,
                'health_level': '未知',
                'churn_risk': '未知',
                'statistics': {},
            }

    async def generate_insight(self, customer_id: str = "default_customer") -> Optional[str]:
        """生成客户洞察文案"""
        try:
            return await self.health_analyzer.generate_insight_message(customer_id)
        except Exception as e:
            self.logger.error(f"生成客户洞察失败: {e}")
            return None
