"""
Web 界面路由

处理前端页面渲染与流式聊天功能。
"""

from fastapi import APIRouter, Request
from fastapi.responses import HTMLResponse, StreamingResponse
from fastapi.templating import Jinja2Templates
from pydantic import BaseModel
from api.chat_handler import ProcessUserInput_stream

templates = Jinja2Templates(directory="web/templates")
router = APIRouter(tags=["Web界面"])


class ChatRequest(BaseModel):
    message: str
    state: str | None = None


@router.get("/", response_class=HTMLResponse, summary="主页")
async def read_root(request: Request):
    """渲染主页聊天界面"""
    return templates.TemplateResponse("index.html", {"request": request})


@router.post("/chat/stream", summary="流式聊天")
async def chat_stream_endpoint(chat: ChatRequest):
    """处理流式聊天请求"""
    async def token_generator():
        async for token in ProcessUserInput_stream(chat.message):
            yield token
    return StreamingResponse(token_generator(), media_type="text/plain")


@router.get("/knowledge", response_class=HTMLResponse, summary="知识库管理页面")
async def knowledge_page(request: Request):
    """知识库管理页面"""
    return templates.TemplateResponse("knowledge_management.html", {"request": request})


@router.get("/engineers", response_class=HTMLResponse, summary="工程师管理页面")
async def engineers_page(request: Request):
    """工程师管理页面"""
    return templates.TemplateResponse("engineers.html", {"request": request})


@router.get("/tickets", response_class=HTMLResponse, summary="工单列表页面")
async def tickets_page(request: Request):
    """工单列表页面"""
    return templates.TemplateResponse("tickets.html", {"request": request})


@router.get("/customers", response_class=HTMLResponse, summary="客户健康度页面")
async def customers_page(request: Request):
    """客户健康度分析页面"""
    return templates.TemplateResponse("customers.html", {"request": request})
