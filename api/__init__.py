"""
API 模块

提供各业务的路由。
"""

from .ticket import router as ticket_router
from .consultation import router as consultation_router
from .task import router as task_router
from .knowledge import router as knowledge_router
from .engineer import router as engineer_router
from .customer import router as customer_router

api_routers = [
    ticket_router,
    consultation_router,
    task_router,
    knowledge_router,
    engineer_router,
    customer_router,
]
