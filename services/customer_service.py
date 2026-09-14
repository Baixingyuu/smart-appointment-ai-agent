"""
客户服务

职责：
1. 客户管理与默认客户创建
2. 客户健康度分析（基于行为统计，输出续费 / 流失预警）
"""

from typing import List, Dict, Any, Optional
from db.db_router import DatabaseRouter
import logging

logger = logging.getLogger(__name__)


class CustomerService:
    """客户服务类"""

    def __init__(self, db_path: str = 'sqlite:///data/support_ticketing.db'):
        self.db = DatabaseRouter(db_path)

    def get_default_customer(self) -> Dict[str, Any]:
        """获取或创建默认客户"""
        return self.db.customers.get_or_create_default_customer()

    def get_customer(self, customer_id: int) -> Optional[Dict[str, Any]]:
        return self.db.customers.get_customer_by_id(customer_id)

    def get_all_customers(self) -> List[Dict[str, Any]]:
        return self.db.customers.get_all_customers()

    def record_activity(self, customer_id: str, action_type: str,
                        action_data: Dict[str, Any] = None,
                        ticket_id: int = None, session_id: str = None) -> bool:
        """记录客户行为"""
        try:
            activity_id = self.db.customer_activities.record_activity(
                customer_id=customer_id,
                action_type=action_type,
                action_data=action_data,
                ticket_id=ticket_id,
                session_id=session_id,
            )
            return bool(activity_id)
        except Exception as e:
            logger.error(f"记录客户行为失败: {e}")
            return False

    def get_customer_health(self, customer_id: str) -> Dict[str, Any]:
        """
        分析客户健康度

        基于近 30 天行为统计，输出健康度评分与续费/流失预警。
        """
        try:
            stats = self.db.customer_activities.get_customer_statistics(customer_id, days_back=30)

            # 健康度评分（满分 100）
            score = 100
            days_since_last = stats.get('days_since_last_activity')
            ticket_count = stats.get('ticket_created_count', 0)

            # 长期不活跃扣分
            if days_since_last is None:
                score -= 40
            elif days_since_last >= 30:
                score -= 40
            elif days_since_last >= 14:
                score -= 25
            elif days_since_last >= 7:
                score -= 10

            # 工单过多（可能遇到严重问题）轻微扣分
            if ticket_count >= 10:
                score -= 15
            elif ticket_count >= 5:
                score -= 5

            # 判断健康度等级
            if score >= 80:
                health = "健康"
                risk = "低"
            elif score >= 60:
                health = "一般"
                risk = "中"
            else:
                health = "风险"
                risk = "高"

            return {
                'customer_id': customer_id,
                'health_score': max(score, 0),
                'health_level': health,
                'churn_risk': risk,
                'statistics': stats,
            }
        except Exception as e:
            logger.error(f"客户健康度分析失败: {e}")
            return {
                'customer_id': customer_id,
                'health_score': 0,
                'health_level': "未知",
                'churn_risk': "未知",
                'statistics': {},
            }
