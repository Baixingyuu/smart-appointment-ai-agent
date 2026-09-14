"""
工程师服务

职责：
1. 管理工程师数据与默认初始化
2. 提供工程师查询（按姓名/等级/专长）
"""

from typing import List, Dict, Any
from db.db_router import DatabaseRouter
import logging

logger = logging.getLogger(__name__)


class EngineerService:
    """工程师服务类"""

    def __init__(self, db_path: str = 'sqlite:///data/support_ticketing.db'):
        self.db = DatabaseRouter(db_path)

        # 默认工程师数据（10 人，专长覆盖不同技术栈 / 产品模块）
        self.default_engineers = [
            {"name": "张伟", "level": "expert",
             "specialty": "后端架构与数据库，擅长高并发定位、性能优化、API 故障排查"},
            {"name": "王强", "level": "senior",
             "specialty": "云原生与容器，擅长 K8s 集群、容器化部署、镜像与编排问题"},
            {"name": "李娜", "level": "senior",
             "specialty": "前端与移动端，擅长 UI 渲染问题、移动 SDK 集成、H5 兼容性"},
            {"name": "赵敏", "level": "intermediate",
             "specialty": "数据分析与报表，擅长数据同步、指标口径、BI 看板"},
            {"name": "刘洋", "level": "expert",
             "specialty": "网络与安全，擅长网络故障、证书配置、SSO 单点登录"},
            {"name": "孙丽", "level": "intermediate",
             "specialty": "计费与账号，擅长账单对账、权限管理、订阅续费"},
            {"name": "周杰", "level": "senior",
             "specialty": "消息与推送，擅长消息队列、推送通道、Webhook 回调"},
            {"name": "吴婷", "level": "intermediate",
             "specialty": "AI 与模型接入，擅长 Prompt 调优、模型 API 接入、向量检索"},
            {"name": "郑斌", "level": "senior",
             "specialty": "第三方集成，擅长开放 API、Webhook、企业协作平台集成"},
            {"name": "何静", "level": "intermediate",
             "specialty": "客户成功，擅长 Onboarding、最佳实践、使用答疑"},
        ]

    def initialize_default_engineers(self) -> bool:
        """初始化默认工程师数据"""
        try:
            existing = self.db.engineers.get_all_engineers()
            if existing:
                logger.info(f"数据库已有 {len(existing)} 位工程师，跳过初始化")
                return True

            for data in self.default_engineers:
                try:
                    self.db.engineers.add_engineer(
                        name=data['name'],
                        level=data['level'],
                        specialty=data['specialty'],
                    )
                except Exception as e:
                    logger.error(f"添加工程师 {data['name']} 失败: {e}")
                    return False

            logger.info(f"工程师初始化完成，共添加 {len(self.default_engineers)} 位")
            return True
        except Exception as e:
            logger.error(f"工程师初始化失败: {e}")
            return False

    def get_all_engineers(self) -> List[Dict[str, Any]]:
        """获取所有工程师"""
        return self.db.engineers.get_all_engineers()

    def get_engineer_by_id(self, engineer_id: int) -> Dict[str, Any]:
        return self.db.engineers.get_engineer_by_id(engineer_id)

    def get_engineer_by_name(self, name: str) -> Dict[str, Any]:
        return self.db.engineers.get_engineer_by_name(name)

    def get_engineers_by_level(self, level: str) -> List[Dict[str, Any]]:
        return self.db.engineers.get_engineers_by_level(level)

    def get_all_specialties(self) -> List[str]:
        return self.db.engineers.get_all_specialties()

    def get_engineers_count(self) -> int:
        return len(self.db.engineers.get_all_engineers())
