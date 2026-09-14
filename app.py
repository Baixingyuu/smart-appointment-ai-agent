"""
FastAPI 应用入口

主应用入口，配置中间件、路由和异常处理。
启动时自动初始化知识库、工程师与默认客户数据。
"""

from fastapi import FastAPI
from fastapi.staticfiles import StaticFiles
from fastapi.middleware.cors import CORSMiddleware
from services.knowledge_service import KnowledgeService
from services.engineer_service import EngineerService
from services.customer_service import CustomerService
import logging

from api import api_routers
from api.core.exceptions import api_exception_handler, general_exception_handler, BusinessException
from web import router as web_router

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


async def initialize_system():
    """系统启动时自动初始化"""
    try:
        logger.info("正在初始化智能工单系统...")

        # 初始化知识库服务
        logger.info("初始化知识库服务...")
        knowledge_service = KnowledgeService()
        await knowledge_service.initialize()

        # 初始化工程师数据
        logger.info("初始化工程师数据...")
        engineer_service = EngineerService()
        engineer_service.initialize_default_engineers()

        # 初始化默认客户
        logger.info("初始化默认客户...")
        customer_service = CustomerService()
        customer_service.get_default_customer()

        logger.info("系统初始化完成！")

    except Exception as e:
        logger.error(f"系统初始化失败: {e}")
        raise


def create_app() -> FastAPI:
    """创建 FastAPI 应用实例"""

    app = FastAPI(
        title="智能工单助手",
        description="企业技术支持工单调度系统：多 Agent 协作、RAG 知识问答、智能派单与客户健康度分析",
        version="2.0.0",
        docs_url="/docs",
        redoc_url="/redoc",
    )

    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    app.add_exception_handler(BusinessException, api_exception_handler)
    app.add_exception_handler(Exception, general_exception_handler)

    for router in api_routers:
        app.include_router(router)

    app.include_router(web_router)
    app.mount("/static", StaticFiles(directory="web/static"), name="static")

    @app.on_event("startup")
    async def startup_event():
        await initialize_system()

    return app


app = create_app()

if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="127.0.0.1", port=8001)
