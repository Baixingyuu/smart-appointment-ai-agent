"""
工单数据库操作器

封装工单相关的数据库操作，通过 Services 层访问（符合分层架构）。
"""

from typing import Dict, Any, Optional


class TicketDatabase:
    """工单数据库操作器"""

    def __init__(self):
        self._ticket_service = None
        self._customer_service = None

    @property
    def ticket_service(self):
        if self._ticket_service is None:
            from services.ticket_service import TicketService
            self._ticket_service = TicketService()
        return self._ticket_service

    @property
    def customer_service(self):
        if self._customer_service is None:
            from services.customer_service import CustomerService
            self._customer_service = CustomerService()
        return self._customer_service

    def create_ticket(self, ticket_info: Dict[str, Any],
                      customer_id: int) -> Optional[Dict[str, Any]]:
        """创建工单"""
        try:
            return self.ticket_service.create_ticket(
                customer_id=customer_id,
                title=ticket_info.get('title', ''),
                description=ticket_info.get('description', ''),
                category=ticket_info.get('category', 'incident'),
                priority=ticket_info.get('priority', 'P3'),
            )
        except Exception as e:
            print(f"创建工单失败：{e}")
            return None

    def assign_ticket(self, ticket_id: int, engineer_id: int) -> bool:
        """分派工单"""
        return self.ticket_service.assign_ticket(ticket_id, engineer_id)

    def record_activity(self, customer_id: str, action_type: str,
                        action_data: Dict[str, Any], ticket_id: int = None,
                        session_id: str = None):
        """记录客户行为"""
        try:
            self.customer_service.record_activity(
                customer_id=customer_id,
                action_type=action_type,
                action_data=action_data,
                ticket_id=ticket_id,
                session_id=session_id,
            )
        except Exception as e:
            print(f"记录客户行为失败（工单仍已创建）：{e}")
