"""
工程师匹配器

负责根据工单信息匹配合适的技术支持工程师：
1. 按优先级过滤工程师等级（P0 仅专家级）
2. 按问题描述 / 类型做技术栈相似度匹配（embedding），返回最匹配者
"""

from typing import Optional, Dict, Any, Callable
from services.text_embedding import find_best_match_indices
from config.constants import PRIORITY_LEVEL_MAP, ENGINEER_LEVEL_LABELS


class EngineerFinder:
    """工程师匹配器（等级过滤 + 专长匹配）"""

    def __init__(self):
        pass

    def _get_engineer_service(self):
        from services.engineer_service import EngineerService
        return EngineerService()

    def _filter_by_level(self, engineers: list, priority: str) -> list:
        """按优先级过滤工程师等级"""
        allowed_levels = PRIORITY_LEVEL_MAP.get(priority, [])
        if not allowed_levels:
            return engineers
        return [e for e in engineers if e.get('level') in allowed_levels]

    def _sort_by_specialty(self, engineers: list, query_text: str) -> list:
        """按技术栈相似度排序（embedding）"""
        if not engineers or not query_text:
            return engineers
        specialties = [e.get('specialty', '') for e in engineers]
        indices = find_best_match_indices(query_text, specialties)
        return [engineers[i] for i in indices]

    def find_specific_engineer(self, engineer_name: str, yield_func: Optional[Callable] = None) -> Optional[Dict]:
        """查找指定工程师"""
        service = self._get_engineer_service()
        if yield_func:
            yield_func(f"[THOUGHT][工单机器人] 用户指定了工程师：{engineer_name}\n")

        engineer = service.get_engineer_by_name(engineer_name)
        if not engineer:
            if yield_func:
                yield_func(f"[THOUGHT][工单机器人] 未找到名为'{engineer_name}'的工程师\n")
        return engineer

    def find_engineer(self, ticket_info: Dict[str, Any], yield_func: Optional[Callable] = None) -> Optional[Dict]:
        """工程师匹配主流程：等级过滤 -> 专长匹配"""
        service = self._get_engineer_service()

        priority = ticket_info.get('priority', 'P3')
        description = ticket_info.get('description', '')
        category = ticket_info.get('category', 'incident')
        engineer_name = ticket_info.get('engineer_name')

        # 1. 优先处理指定工程师
        if engineer_name and engineer_name != "未知":
            return self.find_specific_engineer(engineer_name, yield_func)

        # 2. 通用匹配：等级过滤 -> 专长排序
        if yield_func:
            yield_func("[THOUGHT][工单机器人] 正在匹配工程师...\n")

        all_engineers = service.get_all_engineers()
        if not all_engineers:
            return None

        filtered = self._filter_by_level(all_engineers, priority)
        if yield_func:
            allowed = PRIORITY_LEVEL_MAP.get(priority, [])
            labels = [ENGINEER_LEVEL_LABELS.get(l, l) for l in allowed]
            yield_func(f"[THOUGHT][工单机器人] 按优先级{priority}要求等级（{'/'.join(labels)}）过滤，剩{len(filtered)}位\n")

        if not filtered:
            if yield_func:
                yield_func("[THOUGHT][工单机器人] 无满足等级要求的工程师\n")
            return None

        # 用「问题描述 + 类型」做技术栈匹配，返回最匹配者
        query_text = f"{category} {description}"
        sorted_by_specialty = self._sort_by_specialty(filtered, query_text)

        matched = sorted_by_specialty[0]
        if yield_func:
            yield_func(f"[THOUGHT][工单机器人] 匹配到工程师：{matched['name']}（专长最相关）\n")
        return matched
