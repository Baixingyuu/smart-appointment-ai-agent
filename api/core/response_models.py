"""
API 响应模型
"""

from pydantic import BaseModel
from typing import Any, Dict, Optional
from datetime import datetime
from config.time_config import time_config


class BaseResponse(BaseModel):
    """基础响应模型"""
    message: str
    timestamp: datetime = time_config.now()


class DataResponse(BaseResponse):
    """数据响应模型"""
    data: Any


# 工单相关模型
class TicketCreateRequest(BaseModel):
    customer_id: int
    title: str
    description: str
    category: str = "incident"
    priority: str = "P3"


class TicketAssignRequest(BaseModel):
    engineer_id: int


class TicketStatusUpdateRequest(BaseModel):
    status: str


# 咨询相关模型
class ConsultationRequest(BaseModel):
    user_id: str
    question: str
    category: Optional[str] = None


# 任务分类相关模型
class TaskClassificationRequest(BaseModel):
    text: str


# 客户相关模型
class CustomerAnalysisRequest(BaseModel):
    customer_id: str = "default_customer"
