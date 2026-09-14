"""
Repositories Module

数据访问对象模块，包含：
- 工程师数据仓库
- 工单数据仓库
- 客户数据仓库
- 知识库数据仓库
- 客户行为数据仓库
"""

from .engineer_repository import EngineerRepository
from .ticket_repository import TicketRepository
from .customer_repository import CustomerRepository
from .knowledge_repository import KnowledgeRepository
from .customer_activity_repository import CustomerActivityRepository

__all__ = [
    'EngineerRepository',
    'TicketRepository',
    'CustomerRepository',
    'KnowledgeRepository',
    'CustomerActivityRepository',
]
