"""
Database Module

数据库模块，包含：
- 数据模型定义
- 数据访问对象 (Repository)
- 数据库路由器
- 会话管理
"""

from .db_router import DatabaseRouter
from .repositories import (
    EngineerRepository,
    TicketRepository,
    CustomerRepository,
    KnowledgeRepository,
    CustomerActivityRepository,
)
from .base import SessionManager
from .models import (
    Base, Engineer, Ticket, Customer, KnowledgeDocument, CustomerActivity,
)

__all__ = [
    # 主要入口
    'DatabaseRouter',

    # Repository
    'EngineerRepository',
    'TicketRepository',
    'CustomerRepository',
    'KnowledgeRepository',
    'CustomerActivityRepository',

    # 基础设施
    'SessionManager',

    # 数据模型
    'Base',
    'Engineer',
    'Ticket',
    'Customer',
    'KnowledgeDocument',
    'CustomerActivity',
]
