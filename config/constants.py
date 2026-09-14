"""
企业技术支持工单调度系统 - 常量定义

包含：
- 对话流转状态枚举
- 工单类型 / 优先级 / 状态枚举
- 客户订阅套餐
- 优先级与工程师等级的匹配规则
"""

from enum import Enum


class StateEnum(Enum):
    """多 Agent 对话流转状态"""
    CLASSIFY = "classify"       # 等待意图分类
    TICKETING = "ticketing"     # 工单处理流程
    CONSULT = "consult"         # 咨询流程
    OTHER = "other"


class TicketCategory(str, Enum):
    """工单类型"""
    INCIDENT = "incident"               # 故障
    CONSULTATION = "consultation"       # 咨询
    REQUEST = "request"                 # 服务请求
    CHANGE = "change"                   # 变更请求


class TicketPriority(str, Enum):
    """工单优先级"""
    P0 = "P0"   # 紧急：系统瘫痪 / 核心业务中断
    P1 = "P1"   # 高：核心功能不可用
    P2 = "P2"   # 中：部分功能异常
    P3 = "P3"   # 低：一般问题 / 优化建议


class TicketStatus(str, Enum):
    """工单状态流转"""
    PENDING = "pending"                    # 待分派
    ASSIGNED = "assigned"                  # 已分派（待处理）
    IN_PROGRESS = "in_progress"            # 处理中
    PENDING_CONFIRM = "pending_confirm"    # 待客户确认
    RESOLVED = "resolved"                  # 已解决
    CLOSED = "closed"                      # 已关闭


class CustomerPlan(str, Enum):
    """客户订阅套餐"""
    STANDARD = "standard"
    ENTERPRISE = "enterprise"
    ULTIMATE = "ultimate"


class EngineerLevel(str, Enum):
    """工程师等级"""
    JUNIOR = "junior"              # 初级
    INTERMEDIATE = "intermediate"  # 中级
    SENIOR = "senior"              # 高级
    EXPERT = "expert"              # 专家


# 优先级 -> 允许承接的工程师等级（最低等级要求）
PRIORITY_LEVEL_MAP = {
    TicketPriority.P0.value: [EngineerLevel.EXPERT.value],
    TicketPriority.P1.value: [EngineerLevel.EXPERT.value, EngineerLevel.SENIOR.value],
    TicketPriority.P2.value: [EngineerLevel.SENIOR.value, EngineerLevel.INTERMEDIATE.value],
    TicketPriority.P3.value: [EngineerLevel.INTERMEDIATE.value, EngineerLevel.JUNIOR.value],
}

# 工单状态中文描述（用于展示）
TICKET_STATUS_LABELS = {
    TicketStatus.PENDING.value: "待分派",
    TicketStatus.ASSIGNED.value: "已分派",
    TicketStatus.IN_PROGRESS.value: "处理中",
    TicketStatus.PENDING_CONFIRM.value: "待客户确认",
    TicketStatus.RESOLVED.value: "已解决",
    TicketStatus.CLOSED.value: "已关闭",
}

# 优先级中文描述
TICKET_PRIORITY_LABELS = {
    TicketPriority.P0.value: "紧急",
    TicketPriority.P1.value: "高",
    TicketPriority.P2.value: "中",
    TicketPriority.P3.value: "低",
}

# 工程师等级中文描述
ENGINEER_LEVEL_LABELS = {
    EngineerLevel.JUNIOR.value: "初级",
    EngineerLevel.INTERMEDIATE.value: "中级",
    EngineerLevel.SENIOR.value: "高级",
    EngineerLevel.EXPERT.value: "专家",
}


class SharedState:
    """跨 Agent 共享的对话状态"""

    def __init__(self):
        self.value = StateEnum.CLASSIFY
