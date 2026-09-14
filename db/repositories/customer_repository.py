from typing import List, Dict, Any, Optional
from ..base.interfaces import BaseCustomerRepository
from ..base.session_manager import SessionManager
from ..models import Customer


class CustomerRepository(BaseCustomerRepository):
    """
    客户数据访问对象

    职责：
    1. 客户的创建与查询
    2. 默认客户的兜底创建
    """

    DEFAULT_CUSTOMER_NAME = "演示客户"

    def __init__(self, session_manager: SessionManager):
        self.session_manager = session_manager

    def add_customer(self, name: str, company: Optional[str] = None,
                     plan: str = "standard") -> int:
        with self.session_manager.session_scope() as session:
            customer = Customer(name=name, company=company, plan=plan)
            session.add(customer)
            session.flush()
            return customer.id

    def get_customer_by_id(self, customer_id: int) -> Optional[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            customer = session.query(Customer).filter(Customer.id == customer_id).first()
            return self._to_dict(customer) if customer else None

    def get_or_create_default_customer(self) -> Dict[str, Any]:
        """获取或创建默认客户（单客户演示场景）"""
        with self.session_manager.session_scope() as session:
            customer = session.query(Customer).filter(
                Customer.name == self.DEFAULT_CUSTOMER_NAME
            ).first()
            if not customer:
                customer = Customer(
                    name=self.DEFAULT_CUSTOMER_NAME,
                    company="示例科技有限公司",
                    plan="enterprise",
                )
                session.add(customer)
                session.flush()
            return self._to_dict(customer)

    def get_all_customers(self) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            customers = session.query(Customer).all()
            return [self._to_dict(c) for c in customers]

    def _to_dict(self, customer: Customer) -> Dict[str, Any]:
        return {
            'id': customer.id,
            'name': customer.name,
            'company': customer.company,
            'plan': customer.plan,
        }
