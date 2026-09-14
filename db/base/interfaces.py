"""
数据访问抽象接口

定义各实体的 Repository 抽象接口，保证 Services 层依赖抽象而非具体实现。
"""

from abc import ABC, abstractmethod
from typing import List, Dict, Any, Optional
from datetime import datetime


class BaseEngineerRepository(ABC):
    """工程师数据访问抽象接口"""

    @abstractmethod
    def add_engineer(self, name: str, level: Optional[str] = None,
                     specialty: Optional[str] = None) -> int:
        """添加工程师"""
        pass

    @abstractmethod
    def get_engineer_by_id(self, engineer_id: int) -> Optional[Dict[str, Any]]:
        """根据 ID 获取工程师"""
        pass

    @abstractmethod
    def get_engineer_by_name(self, name: str) -> Optional[Dict[str, Any]]:
        """根据姓名获取工程师"""
        pass

    @abstractmethod
    def get_all_engineers(self) -> List[Dict[str, Any]]:
        """获取所有工程师"""
        pass

    @abstractmethod
    def get_engineers_by_level(self, level: str) -> List[Dict[str, Any]]:
        """根据等级获取工程师"""
        pass

    @abstractmethod
    def get_all_specialties(self) -> List[str]:
        """获取所有工程师的专长列表"""
        pass


class BaseTicketRepository(ABC):
    """工单数据访问抽象接口"""

    @abstractmethod
    def create_ticket(self, customer_id: int, title: str, description: str,
                      category: str, priority: str, status: str = "pending") -> int:
        """创建工单，返回工单 ID"""
        pass

    @abstractmethod
    def get_ticket_by_id(self, ticket_id: int) -> Optional[Dict[str, Any]]:
        """根据 ID 获取工单"""
        pass

    @abstractmethod
    def get_ticket_by_no(self, ticket_no: str) -> Optional[Dict[str, Any]]:
        """根据工单号获取工单"""
        pass

    @abstractmethod
    def assign_ticket(self, ticket_id: int, engineer_id: int) -> bool:
        """将工单分派给工程师"""
        pass

    @abstractmethod
    def update_ticket_status(self, ticket_id: int, status: str) -> bool:
        """更新工单状态"""
        pass

    @abstractmethod
    def get_tickets_by_status(self, status: str) -> List[Dict[str, Any]]:
        """根据状态获取工单列表"""
        pass

    @abstractmethod
    def get_tickets_by_customer(self, customer_id: int) -> List[Dict[str, Any]]:
        """获取指定客户的工单列表"""
        pass

    @abstractmethod
    def get_all_tickets(self) -> List[Dict[str, Any]]:
        """获取所有工单"""
        pass


class BaseCustomerRepository(ABC):
    """客户数据访问抽象接口"""

    @abstractmethod
    def add_customer(self, name: str, company: Optional[str] = None,
                     plan: str = "standard") -> int:
        """添加客户，返回客户 ID"""
        pass

    @abstractmethod
    def get_customer_by_id(self, customer_id: int) -> Optional[Dict[str, Any]]:
        """根据 ID 获取客户"""
        pass

    @abstractmethod
    def get_or_create_default_customer(self) -> Dict[str, Any]:
        """获取或创建默认客户"""
        pass

    @abstractmethod
    def get_all_customers(self) -> List[Dict[str, Any]]:
        """获取所有客户"""
        pass


class BaseKnowledgeRepository(ABC):
    """知识库数据访问抽象接口"""

    @abstractmethod
    def add_document(self, content: str, category: str,
                     keywords: Optional[List[str]] = None,
                     embedding: Optional[List[float]] = None) -> int:
        pass

    @abstractmethod
    def get_document(self, doc_id: int) -> Optional[Dict[str, Any]]:
        pass

    @abstractmethod
    def get_all_documents(self, include_inactive: bool = False) -> List[Dict[str, Any]]:
        pass

    @abstractmethod
    def update_document(self, doc_id: int, content: Optional[str] = None,
                        category: Optional[str] = None,
                        keywords: Optional[List[str]] = None,
                        embedding: Optional[List[float]] = None) -> bool:
        pass

    @abstractmethod
    def delete_document(self, doc_id: int, soft_delete: bool = True) -> bool:
        pass

    @abstractmethod
    def search_documents_by_category(self, category: str) -> List[Dict[str, Any]]:
        pass

    @abstractmethod
    def get_all_categories(self) -> List[str]:
        pass

    @abstractmethod
    def get_documents_count(self) -> int:
        pass


class BaseCustomerActivityRepository(ABC):
    """客户行为数据访问抽象接口"""

    @abstractmethod
    def record_activity(self, customer_id: str, action_type: str,
                        action_data: Optional[Dict[str, Any]] = None,
                        ticket_id: Optional[int] = None,
                        session_id: Optional[str] = None) -> int:
        pass

    @abstractmethod
    def get_activities(self, customer_id: str, action_type: Optional[str] = None,
                       days_back: Optional[int] = None) -> List[Dict[str, Any]]:
        pass

    @abstractmethod
    def get_customer_statistics(self, customer_id: str, days_back: int = 30) -> Dict[str, Any]:
        pass
