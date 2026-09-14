"""
API 核心组件初始化
"""

from .response_models import (
    BaseResponse,
    DataResponse,
    TicketCreateRequest,
    TicketAssignRequest,
    TicketStatusUpdateRequest,
    ConsultationRequest,
    TaskClassificationRequest,
    CustomerAnalysisRequest,
)
from .exceptions import BusinessException, api_exception_handler, general_exception_handler

__all__ = [
    'BaseResponse',
    'DataResponse',
    'TicketCreateRequest',
    'TicketAssignRequest',
    'TicketStatusUpdateRequest',
    'ConsultationRequest',
    'TaskClassificationRequest',
    'CustomerAnalysisRequest',
    'BusinessException',
    'api_exception_handler',
    'general_exception_handler',
]
