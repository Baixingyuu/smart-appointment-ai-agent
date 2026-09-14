from typing import List, Dict, Any, Optional
from datetime import datetime, timedelta
from sqlalchemy import func
from ..base.interfaces import BaseCustomerActivityRepository
from ..base.session_manager import SessionManager
from ..models import CustomerActivity, Ticket


class CustomerActivityRepository(BaseCustomerActivityRepository):
    """
    客户行为数据访问对象

    职责：
    1. 记录客户行为（提交工单 / 咨询 / 查询工单）
    2. 统计客户行为（用于健康度分析）
    """

    def __init__(self, session_manager: SessionManager):
        self.session_manager = session_manager

    def record_activity(self, customer_id: str, action_type: str,
                        action_data: Optional[Dict[str, Any]] = None,
                        ticket_id: Optional[int] = None,
                        session_id: Optional[str] = None) -> int:
        with self.session_manager.session_scope() as session:
            activity = CustomerActivity(
                customer_id=customer_id,
                action_type=action_type,
                action_data=action_data,
                ticket_id=ticket_id,
                session_id=session_id,
            )
            session.add(activity)
            session.flush()
            return activity.id

    def get_activities(self, customer_id: str, action_type: Optional[str] = None,
                       days_back: Optional[int] = None) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            query = session.query(CustomerActivity).filter(
                CustomerActivity.customer_id == customer_id
            )
            if action_type:
                query = query.filter(CustomerActivity.action_type == action_type)
            if days_back:
                cutoff = datetime.utcnow() - timedelta(days=days_back)
                query = query.filter(CustomerActivity.created_at >= cutoff)
            activities = query.order_by(CustomerActivity.created_at.desc()).all()
            return [self._to_dict(a) for a in activities]

    def get_customer_statistics(self, customer_id: str, days_back: int = 30) -> Dict[str, Any]:
        """统计客户在指定天数内的行为（用于健康度分析）"""
        with self.session_manager.session_scope() as session:
            cutoff = datetime.utcnow() - timedelta(days=days_back)

            total = session.query(CustomerActivity).filter(
                CustomerActivity.customer_id == customer_id,
                CustomerActivity.created_at >= cutoff,
            ).count()

            ticket_created = session.query(CustomerActivity).filter(
                CustomerActivity.customer_id == customer_id,
                CustomerActivity.action_type == 'ticket_created',
                CustomerActivity.created_at >= cutoff,
            ).count()

            consultation = session.query(CustomerActivity).filter(
                CustomerActivity.customer_id == customer_id,
                CustomerActivity.action_type == 'consultation',
                CustomerActivity.created_at >= cutoff,
            ).count()

            last_activity = session.query(CustomerActivity).filter(
                CustomerActivity.customer_id == customer_id,
            ).order_by(CustomerActivity.created_at.desc()).first()

            days_since_last = None
            if last_activity:
                days_since_last = (datetime.utcnow() - last_activity.created_at).days

            return {
                'total_activities': total,
                'ticket_created_count': ticket_created,
                'consultation_count': consultation,
                'days_since_last_activity': days_since_last,
                'period_days': days_back,
            }

    def _to_dict(self, activity: CustomerActivity) -> Dict[str, Any]:
        return {
            'id': activity.id,
            'customer_id': activity.customer_id,
            'action_type': activity.action_type,
            'action_data': activity.action_data,
            'ticket_id': activity.ticket_id,
            'session_id': activity.session_id,
            'created_at': activity.created_at,
        }
