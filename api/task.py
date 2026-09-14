"""
任务分类 API

提供用户意图分类接口。
"""

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

router = APIRouter(prefix="/api/task", tags=["任务分类"])


class TaskClassificationRequest(BaseModel):
    text: str


@router.post("/classify")
async def classify_task(request: TaskClassificationRequest):
    """分类任务意图"""
    try:
        from config.model_provider import create_chat_model
        from agents.task_classification.task_classifier import TaskClassifier

        classifier = TaskClassifier(create_chat_model(temperature=0))
        category = await classifier.classify_task(request.text)
        return {
            "status": "success",
            "data": {
                "category": category,
                "description": classifier.get_category_description(category),
            }
        }
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
