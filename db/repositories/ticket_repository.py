from typing import List, Dict, Any, Optional
from datetime import datetime
from ..base.interfaces import BaseTicketRepository
from ..base.session_manager import SessionManager
from ..models import Ticket


class TicketRepository(BaseTicketRepository):
    """
    工单数据访问对象

    职责：
    1. 工单的创建、分派与状态流转
    2. 按状态 / 客户 / 工程师查询
    """

    def __init__(self, session_manager: SessionManager):
        self.session_manager = session_manager

    def create_ticket(self, customer_id: int, title: str, description: str,
                      category: str, priority: str, status: str = "pending") -> int:
        with self.session_manager.session_scope() as session:
            ticket_no = self._generate_ticket_no(session)
            ticket = Ticket(
                ticket_no=ticket_no,
                customer_id=customer_id,
                title=title,
                description=description,
                category=category,
                priority=priority,
                status=status,
            )
            session.add(ticket)
            session.flush()
            return ticket.id

    def _generate_ticket_no(self, session) -> str:
        """生成工单号：TK-YYYYMMDD-序号"""
        from config.time_config import time_config
        date_str = time_config.now().strftime("%Y%m%d")
        prefix = f"TK-{date_str}-"
        count = session.query(Ticket).filter(Ticket.ticket_no.like(f"{prefix}%")).count()
        return f"{prefix}{count + 1:04d}"

    def get_ticket_by_id(self, ticket_id: int) -> Optional[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            ticket = session.query(Ticket).filter(Ticket.id == ticket_id).first()
            return self._to_dict(ticket) if ticket else None

    def get_ticket_by_no(self, ticket_no: str) -> Optional[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            ticket = session.query(Ticket).filter(Ticket.ticket_no == ticket_no).first()
            return self._to_dict(ticket) if ticket else None

    def assign_ticket(self, ticket_id: int, engineer_id: int) -> bool:
        with self.session_manager.session_scope() as session:
            ticket = session.query(Ticket).filter(Ticket.id == ticket_id).first()
            if not ticket:
                return False
            ticket.engineer_id = engineer_id
            ticket.status = "assigned"
            ticket.updated_at = datetime.utcnow()
            return True

    def update_ticket_status(self, ticket_id: int, status: str) -> bool:
        with self.session_manager.session_scope() as session:
            ticket = session.query(Ticket).filter(Ticket.id == ticket_id).first()
            if not ticket:
                return False
            ticket.status = status
            ticket.updated_at = datetime.utcnow()
            if status == "resolved":
                ticket.resolved_at = datetime.utcnow()
            return True

    def get_tickets_by_status(self, status: str) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            tickets = session.query(Ticket).filter(Ticket.status == status).all()
            return [self._to_dict(t) for t in tickets]

    def get_tickets_by_customer(self, customer_id: int) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            tickets = session.query(Ticket).filter(
                Ticket.customer_id == customer_id
            ).order_by(Ticket.created_at.desc()).all()
            return [self._to_dict(t) for t in tickets]

    def get_all_tickets(self) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            tickets = session.query(Ticket).order_by(Ticket.created_at.desc()).all()
            return [self._to_dict(t) for t in tickets]

    def _to_dict(self, ticket: Ticket) -> Dict[str, Any]:
        return {
            'id': ticket.id,
            'ticket_no': ticket.ticket_no,
            'customer_id': ticket.customer_id,
            'customer_name': ticket.customer.name if ticket.customer else None,
            'customer_company': ticket.customer.company if ticket.customer else None,
            'engineer_id': ticket.engineer_id,
            'engineer_name': ticket.engineer.name if ticket.engineer else None,
            'title': ticket.title,
            'description': ticket.description,
            'category': ticket.category,
            'priority': ticket.priority,
            'status': ticket.status,
            'created_at': ticket.created_at,
            'updated_at': ticket.updated_at,
            'resolved_at': ticket.resolved_at,
        }
