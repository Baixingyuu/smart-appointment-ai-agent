from typing import List, Dict, Any, Optional
from ..base.interfaces import BaseEngineerRepository
from ..base.session_manager import SessionManager
from ..models import Engineer


class EngineerRepository(BaseEngineerRepository):
    """
    工程师数据访问对象

    职责：
    1. 工程师信息的 CRUD
    2. 按等级 / 专长查询
    """

    def __init__(self, session_manager: SessionManager):
        self.session_manager = session_manager

    def add_engineer(self, name: str, level: Optional[str] = None,
                     specialty: Optional[str] = None) -> int:
        with self.session_manager.session_scope() as session:
            engineer = Engineer(
                name=name,
                level=level,
                specialty=specialty,
            )
            session.add(engineer)
            session.flush()
            return engineer.id

    def get_engineer_by_id(self, engineer_id: int) -> Optional[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            engineer = session.query(Engineer).filter(Engineer.id == engineer_id).first()
            return self._to_dict(engineer) if engineer else None

    def get_engineer_by_name(self, name: str) -> Optional[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            engineer = session.query(Engineer).filter(Engineer.name == name).first()
            return self._to_dict(engineer) if engineer else None

    def get_all_engineers(self) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            engineers = session.query(Engineer).all()
            return [self._to_dict(e) for e in engineers]

    def get_engineers_by_level(self, level: str) -> List[Dict[str, Any]]:
        with self.session_manager.session_scope() as session:
            engineers = session.query(Engineer).filter(Engineer.level == level).all()
            return [self._to_dict(e) for e in engineers]

    def get_all_specialties(self) -> List[str]:
        with self.session_manager.session_scope() as session:
            specialties = session.query(Engineer.specialty).distinct().all()
            return [s[0] for s in specialties if s[0] is not None]

    def _to_dict(self, engineer: Engineer) -> Dict[str, Any]:
        return {
            'id': engineer.id,
            'name': engineer.name,
            'level': engineer.level,
            'specialty': engineer.specialty,
        }
