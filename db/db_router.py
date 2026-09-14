"""
数据库路由器

提供统一的数据访问入口，协调各 Repository。
"""

from .base import SessionManager
from .repositories import (
    EngineerRepository,
    TicketRepository,
    CustomerRepository,
    KnowledgeRepository,
    CustomerActivityRepository,
)


class DatabaseRouter:
    """
    数据库路由器

    职责：
    1. 管理数据库连接和会话
    2. 提供统一的数据访问入口
    3. 协调各 Repository 的操作
    """

    def __init__(self, db_path: str = 'sqlite:///data/support_ticketing.db'):
        self.session_manager = SessionManager(db_path)

        self.engineer_repo = EngineerRepository(self.session_manager)
        self.ticket_repo = TicketRepository(self.session_manager)
        self.customer_repo = CustomerRepository(self.session_manager)
        self.knowledge_repo = KnowledgeRepository(self.session_manager)
        self.customer_activity_repo = CustomerActivityRepository(self.session_manager)

    @property
    def engineers(self) -> EngineerRepository:
        """获取工程师数据仓库"""
        return self.engineer_repo

    @property
    def tickets(self) -> TicketRepository:
        """获取工单数据仓库"""
        return self.ticket_repo

    @property
    def customers(self) -> CustomerRepository:
        """获取客户数据仓库"""
        return self.customer_repo

    @property
    def knowledge(self) -> KnowledgeRepository:
        """获取知识库数据仓库"""
        return self.knowledge_repo

    @property
    def customer_activities(self) -> CustomerActivityRepository:
        """获取客户行为数据仓库"""
        return self.customer_activity_repo

    def close(self):
        """关闭数据库连接"""
        self.session_manager.close()
