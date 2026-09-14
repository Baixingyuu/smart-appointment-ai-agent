"""
工单服务

职责：
1. 工单创建（含优先级校验）
2. 工单分派与状态流转
3. 工单查询
"""

from typing import List, Dict, Any, Optional
from db.db_router import DatabaseRouter
from config.constants import (
    TicketStatus,
    TICKET_STATUS_LABELS,
    TICKET_PRIORITY_LABELS,
)
import logging

logger = logging.getLogger(__name__)


class TicketService:
    """工单服务类"""

    def __init__(self, db_path: str = 'sqlite:///data/support_ticketing.db'):
        self.db = DatabaseRouter(db_path)

    def create_ticket(self, customer_id: int, title: str, description: str,
                      category: str = "incident", priority: str = "P3") -> Optional[Dict[str, Any]]:
        """创建工单，返回工单信息"""
        try:
            customer = self.db.customers.get_customer_by_id(customer_id)
            if not customer:
                logger.error(f"客户 {customer_id} 不存在")
                return None

            ticket_id = self.db.tickets.create_ticket(
                customer_id=customer_id,
                title=title,
                description=description,
                category=category,
                priority=priority,
                status=TicketStatus.PENDING.value,
            )

            return self.db.tickets.get_ticket_by_id(ticket_id)
        except Exception as e:
            logger.error(f"创建工单失败: {e}")
            return None

    def assign_ticket(self, ticket_id: int, engineer_id: int) -> bool:
        """将工单分派给工程师"""
        try:
            return self.db.tickets.assign_ticket(ticket_id, engineer_id)
        except Exception as e:
            logger.error(f"分派工单失败: {e}")
            return False

    def update_status(self, ticket_id: int, status: str) -> bool:
        """更新工单状态"""
        try:
            return self.db.tickets.update_ticket_status(ticket_id, status)
        except Exception as e:
            logger.error(f"更新工单状态失败: {e}")
            return False

    def get_ticket(self, ticket_id: Optional[int] = None, ticket_no: Optional[str] = None) -> Optional[Dict[str, Any]]:
        """根据 ID 或工单号查询工单"""
        if ticket_id:
            return self.db.tickets.get_ticket_by_id(ticket_id)
        if ticket_no:
            return self.db.tickets.get_ticket_by_no(ticket_no)
        return None

    def get_tickets_by_customer(self, customer_id: int) -> List[Dict[str, Any]]:
        return self.db.tickets.get_tickets_by_customer(customer_id)

    def get_all_tickets(self) -> List[Dict[str, Any]]:
        return self.db.tickets.get_all_tickets()

    def get_tickets_by_status(self, status: str) -> List[Dict[str, Any]]:
        return self.db.tickets.get_tickets_by_status(status)

    def format_ticket_summary(self, ticket: Dict[str, Any]) -> str:
        """格式化工单摘要信息"""
        status_label = TICKET_STATUS_LABELS.get(ticket.get('status'), ticket.get('status'))
        priority_label = TICKET_PRIORITY_LABELS.get(ticket.get('priority'), ticket.get('priority'))
        engineer = ticket.get('engineer_name') or "待分派"
        return (
            f"工单号：{ticket.get('ticket_no')}\n"
            f"标题：{ticket.get('title')}\n"
            f"优先级：{priority_label}（{ticket.get('priority')}）\n"
            f"状态：{status_label}\n"
            f"负责工程师：{engineer}"
        )
