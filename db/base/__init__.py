"""
Database Base Module

数据库基础模块，包含：
- 会话管理
- 抽象接口
"""

from .session_manager import SessionManager
from .interfaces import (
    BaseEngineerRepository,
    BaseTicketRepository,
    BaseCustomerRepository,
    BaseKnowledgeRepository,
    BaseCustomerActivityRepository,
)

__all__ = [
    'SessionManager',
    'BaseEngineerRepository',
    'BaseTicketRepository',
    'BaseCustomerRepository',
    'BaseKnowledgeRepository',
    'BaseCustomerActivityRepository',
]
