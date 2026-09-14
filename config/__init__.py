"""
配置模块

提供工单调度系统所需的常量、状态枚举与基本配置。
"""

from .constants import (
    StateEnum,
    SharedState,
    TicketCategory,
    TicketPriority,
    TicketStatus,
    CustomerPlan,
    EngineerLevel,
    PRIORITY_LEVEL_MAP,
    TICKET_STATUS_LABELS,
    TICKET_PRIORITY_LABELS,
    ENGINEER_LEVEL_LABELS,
)
from .settings import settings

__all__ = [
    # 状态与枚举
    'StateEnum',
    'SharedState',
    'TicketCategory',
    'TicketPriority',
    'TicketStatus',
    'CustomerPlan',
    'EngineerLevel',

    # 配置
    'PRIORITY_LEVEL_MAP',
    'TICKET_STATUS_LABELS',
    'TICKET_PRIORITY_LABELS',
    'ENGINEER_LEVEL_LABELS',

    # 基本设置
    'settings',
]
