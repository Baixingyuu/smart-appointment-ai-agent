"""
时间配置模块

统一管理系统时间（北京时间）。
"""

from datetime import datetime, timezone, timedelta
from typing import Optional


class TimeConfig:
    """时间配置类"""

    BEIJING_TZ = timezone(timedelta(hours=8))

    @classmethod
    def now(cls) -> datetime:
        """获取当前北京时间"""
        return datetime.now(timezone.utc).astimezone(cls.BEIJING_TZ)

    @classmethod
    def today(cls) -> datetime:
        """获取今天日期（北京时间）"""
        return cls.now().replace(hour=0, minute=0, second=0, microsecond=0)

    @classmethod
    def current_date_str(cls, format_str: str = "%Y年%m月%d日") -> str:
        """获取当前日期字符串（北京时间）"""
        return cls.now().strftime(format_str)

    @classmethod
    def current_datetime_str(cls, format_str: str = "%Y-%m-%d %H:%M") -> str:
        """获取当前日期时间字符串（北京时间）"""
        return cls.now().strftime(format_str)

    @classmethod
    def parse_datetime(cls, date_str: str, format_str: str = "%Y-%m-%d %H:%M") -> Optional[datetime]:
        """解析日期时间字符串为北京时间的 datetime 对象"""
        try:
            dt = datetime.strptime(date_str, format_str)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=cls.BEIJING_TZ)
            return dt
        except ValueError:
            return None

    @classmethod
    def format_datetime(cls, dt: datetime, format_str: str = "%Y-%m-%d %H:%M") -> str:
        """格式化 datetime 对象为字符串"""
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=cls.BEIJING_TZ)
        elif dt.tzinfo != cls.BEIJING_TZ:
            dt = dt.astimezone(cls.BEIJING_TZ)
        return dt.strftime(format_str)


# 创建全局实例
time_config = TimeConfig()
