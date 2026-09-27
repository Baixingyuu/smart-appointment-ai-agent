"""业务事实：员工名册、服务字典、知识语料。

已结历史工单不在这里，在 `history.py`（它是证据源，不是目录的派生物）。
名册 24 人：新增的 15 人**都不持有服务归属** —— 持有归属的只有 8 人（owner 侧 101–107，backup 侧多一个 108）。

派单只剩两层，所以这里只需要"运营定的事实"，不需要任何打分权重：
服务归属（owner/backup）是喂给模型的上下文，不是金标来源，也不是特征。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum

ESCALATE_HUMAN = "ESCALATE_HUMAN"


class Level(StrEnum):
    JUNIOR = "junior"
    MID = "mid"
    SENIOR = "senior"


class OwnershipKind(StrEnum):
    OWNER = "owner"
    BACKUP = "backup"


@dataclass(frozen=True)
class Team:
    id: int
    name: str


@dataclass(frozen=True)
class Employee:
    id: int
    name: str
    team_id: int
    level: Level
    active: bool
    max_concurrent: int
    current_load: int
    recency: float
    profile: str

    @property
    def has_capacity(self) -> bool:
        return self.current_load < self.max_concurrent

    @property
    def searchable_text(self) -> str:
        """能力画像才是召回信号，姓名不是 —— 所以正文用画像，团队/级别作后缀。"""
        return f"{self.profile}（{team_name(self.team_id)}／{self.level.value}）"


@dataclass(frozen=True)
class ServiceOwnership:
    service_id: int
    kind: OwnershipKind


@dataclass(frozen=True)
class Service:
    id: int
    name: str
    team_id: int
    owner_id: int
    backup_owner_id: int | None
    description: str
    aliases: tuple[str, ...] = ()

    @property
    def searchable_text(self) -> str:
        return f"{self.name}（{'/'.join(self.aliases)}）：{self.description}"


@dataclass(frozen=True)
class KnowledgeChunk:
    id: str
    doc_id: str
    title: str
    content: str
    keywords: tuple[str, ...] = field(default_factory=tuple)

    @property
    def searchable_text(self) -> str:
        return f"{self.title} {self.content}"


TEAMS: tuple[Team, ...] = (
    Team(1, "接口组"),
    Team(2, "数据组"),
    Team(3, "权限计费组"),
    Team(4, "运维组"),
    Team(5, "前端组"),
    Team(6, "安全组"),
    Team(7, "综合组"),
    Team(8, "实习组"),
)

SERVICES: tuple[Service, ...] = (
    Service(
        2001, "核心下单接口", 1, 101, 107,
        "对外提供订单创建、修改、取消的同步接口；日均调用 20 万次，P0 服务。",
        ("下单接口", "订单接口", "创建订单", "下单服务"),
    ),
    Service(
        2002, "报表与 BI 查询", 2, 102, 107,
        "运营与财务侧的报表生成服务，含月度汇总看板与临时查询导出。",
        ("报表", "BI", "看板", "月报", "报表查询"),
    ),
    Service(
        2003, "用户账号中心", 3, 106, 104,
        "账号注册、登录、锁定解锁、密码重置。工单常见词包括登不上、账号被锁。",
        ("账号", "登录", "注册", "用户中心", "成员管理"),
    ),
    Service(
        2004, "计费与账单", 3, 106, 107,
        "订阅计费、账单生成、发票开具。异常常与账号权限、支付通道联动。",
        ("计费", "账单", "扣费", "发票", "订阅"),
    ),
    Service(
        2005, "网络与机房链路", 4, 103, 107,
        "内部机房与专线、DNS、跨机房链路。任何大面积打不开都可能落到这里。",
        ("网络", "链路", "机房", "专线", "带宽"),
    ),
    Service(
        2006, "私有化部署环境", 4, 103, 104,
        "客户私有化环境的部署、升级、参数配置。咨询类工单集中在这里。",
        ("私有化", "部署", "现场部署", "驻场"),
    ),
    Service(
        2007, "Web 控制台", 5, 105, 108,
        "面向客户与内部运营的管理控制台前端，含权限路由与页面渲染。",
        ("控制台", "后台", "管理台", "Web 页面"),
    ),
    Service(
        2008, "桌面客户端", 5, 105, 108,
        "Windows / macOS 桌面客户端，含自动升级与本地缓存。",
        ("桌面", "客户端", "Windows 端", "Mac 端"),
    ),
    Service(
        2009, "数据库集群", 2, 102, 101,
        "生产 MySQL 集群、连接池、主从同步。慢查询与连接数打满是常见症状。",
        ("数据库", "MySQL", "主从", "分库分表", "连接池"),
    ),
    Service(
        2010, "安全加固与合规", 6, 104, 103,
        "系统层安全加固、漏洞响应、合规审计。P0 通常直接触发。",
        ("安全", "加固", "合规", "漏洞", "渗透"),
    ),
    Service(
        2011, "数据同步管道", 1, 101, 102,
        "上下游数据同步任务，含定时与流式；常见症状是数据延迟、对不上账。",
        ("同步", "管道", "任务", "延迟", "数据不同步"),
    ),
    Service(
        2012, "硬件与设备", 4, 103, None,
        "服务器、终端、外设（POS / 打印机）等硬件故障。",
        ("硬件", "设备", "服务器", "打印机", "POS"),
    ),
    Service(
        2013, "接口鉴权与令牌", 1, 101, 104,
        "对外接口的鉴权、签名与令牌刷新；工单常见词 401、token 过期。",
        ("鉴权", "令牌", "token", "签名", "401"),
    ),
    Service(
        2014, "员工工具台", 7, 107, None,
        "面向内部员工与客服的操作台。跨系统兜底，多数其他类问题最终归到这里。",
        ("内部工具", "员工工具", "工单后台", "客服台"),
    ),
)

@dataclass(frozen=True)
class RosterRecord:
    id: int
    name: str
    team_id: int
    level: Level
    active: bool
    current_load: int
    max_concurrent: int
    recency: float
    profile: str


#: 名册 24 人 —— v4 的"先召回再选"要有意义，候选数必须只是名册的一个小片子。
#: 9 人时实测候选 7/9（等于把名册全列出来），召回退化成摆设。
#: 新增的 15 人**都不持有服务归属**：他们是相邻产能与干扰项，owner/backup 表一个字没改，
#: 所以层 1 的 45 条金标与指派金标的判定次序都不受影响（会受影响的只有难度，见 RUBRIC）。
ROSTER: tuple[RosterRecord, ...] = (
    # --- 原班 9 人：id/姓名/团队/级别/在职/负载/活跃度与原快照一致；
    #     104、109 两条画像为给新增 15 人让开口径而改写 -----------------------
    RosterRecord(101, "张伟（接口组）", 1, Level.SENIOR, True, 2, 5, 0.85,
        "接口组骨干，负责对外核心接口，尤其熟悉下单链路和令牌鉴权；"
        "数据库层能兜底但非首选。历史处理过高并发写导致的下单雪崩类故障。"),
    RosterRecord(102, "王强（数据库组）", 2, Level.SENIOR, True, 3, 4, 0.80,
        "数据组负责人，数据库调优与慢查询排查经验最丰富；报表与 BI 侧的问题第一时间找他。"),
    RosterRecord(103, "陈磊（运维组）", 4, Level.MID, True, 4, 6, 0.75,
        "运维组主力，负责网络、机房链路与私有化现场部署；硬件类工单基本都走他。"),
    RosterRecord(104, "袁泉（安全组）", 6, Level.SENIOR, True, 1, 3, 0.70,
        "安全组唯一做漏洞响应的人，负责加固与合规；账号鉴权异常时会被拉进来一起看。"),
    RosterRecord(105, "钱枫（前端组）", 5, Level.MID, True, 2, 4, 0.78,
        "前端组骨干，控制台与桌面客户端的疑难杂症归口；权限路由与渲染性能问题最擅长。"),
    RosterRecord(106, "周涛（权限计费组）", 3, Level.MID, True, 3, 4, 0.72,
        "权限计费组唯一员工，账号锁定与账单异常是他的日常；跨系统的账号打通问题会拉上安全组。"),
    RosterRecord(107, "黄磊（全栈通才）", 7, Level.SENIOR, True, 5, 8, 0.60,
        "全栈通才，跨栈救火队员；主战场是内部工具台，也是多个核心服务的备份。"),
    RosterRecord(108, "李娜（实习坐席）", 8, Level.JUNIOR, True, 6, 10, 0.95,
        "实习坐席，前端类工单 backup。响应快、任务多，但独立处理复杂故障的能力有限。"),
    RosterRecord(109, "郑爽（已离职）", 1, Level.SENIOR, False, 0, 5, 0.90,
        "原接口组资深工程师，负责下单接口与鉴权令牌链路，离职前是这条链路的主值。"
        "记录保留用于测试 active=False 的过滤路径。"),
    # --- 接口组 ---------------------------------------------------------------
    RosterRecord(110, "廖静（接口组）", 1, Level.MID, True, 1, 4, 0.82,
        "接口组做合作方接入的，签名联调与沙箱环境问题归她；下单主链路的变更要 101 复核后才合。"),
    RosterRecord(111, "康宁（接口组）", 1, Level.JUNIOR, True, 2, 3, 0.88,
        "接口组新人，跟着 101 跑下单链路的告警值守，能复现和定位到具体接口，处置方案要人带。"),
    # --- 数据组 ---------------------------------------------------------------
    RosterRecord(112, "邵斌（数据组）", 2, Level.MID, True, 2, 4, 0.76,
        "DBA，连接池参数、慢查询抓取与主从切换的执行都是他做；报表口径和业务指标他不碰。"),
    RosterRecord(113, "高敏（数据组）", 2, Level.JUNIOR, True, 3, 5, 0.84,
        "数据分析岗，写临时查询和取数脚本，能看出数据对不上，但没有生产库的变更权限。"),
    RosterRecord(122, "崔明（数据组）", 2, Level.MID, True, 2, 4, 0.71,
        "BI 报表开发，看板与月度汇总的模板在他手里；库性能问题要转给数据组主值。"),
    # --- 权限计费组 -----------------------------------------------------------
    RosterRecord(114, "唐磊（权限计费组）", 3, Level.MID, True, 1, 4, 0.69,
        "计费对账二线，发票规则与账务核对熟；账号锁定的处置口不在他这里。"),
    # --- 运维组 ---------------------------------------------------------------
    RosterRecord(115, "杜衡（运维组）", 4, Level.SENIOR, True, 3, 5, 0.74,
        "机房与专线的值班主任，专线割接和跨机房链路要和 103 一起看；应用层问题他不接。"),
    RosterRecord(116, "宋楠（运维组）", 4, Level.JUNIOR, True, 4, 6, 0.86,
        "桌面与硬件现场支持，跑门店换打印机和 POS 终端；结单前要 103 复核，独立判断硬件批量故障的能力有限。"),
    RosterRecord(123, "谭雪（运维组）", 4, Level.MID, True, 2, 4, 0.73,
        "私有化现场部署驻场，按手册升级与配参数；环境本身的网络与硬件条件归 103 定。"),
    # --- 前端组 ---------------------------------------------------------------
    RosterRecord(117, "程雪（前端组）", 5, Level.MID, True, 1, 4, 0.81,
        "控制台前端开发，白屏与路由渲染问题她见得最多；权限模型和后端接口报错要转出去。"),
    RosterRecord(118, "冯佳（前端组）", 5, Level.JUNIOR, True, 3, 5, 0.79,
        "桌面客户端测试转支持，能复现闪退并抓日志；升级链路与签名问题不是她定的。"),
    # --- 安全组 ---------------------------------------------------------------
    RosterRecord(119, "石敢（安全组）", 6, Level.MID, True, 1, 3, 0.66,
        "安全组的合规与审计岗，出审计材料、整理漏洞台账；漏洞响应和加固实施仍走 104。"),
    # --- 综合组 ---------------------------------------------------------------
    RosterRecord(120, "贾一雯（综合组）", 7, Level.JUNIOR, True, 5, 8, 0.92,
        "服务台一线，负责录入、转派与跟进，能判断该找哪个组，但不承担技术处置。"),
    RosterRecord(121, "白帆（实习组）", 8, Level.JUNIOR, True, 4, 8, 0.90,
        "实习坐席，只做咨询类的资料整理与回访；故障类和变更类不能给他。"),
    RosterRecord(124, "韩梅（已离职）", 2, Level.SENIOR, False, 0, 5, 0.87,
        "原数据组数据库集群负责人，主从与分库分表是她带的头。记录保留用于测试离职陷阱。"),
)


def employees() -> tuple[Employee, ...]:
    return tuple(
        Employee(
            id=r.id,
            name=r.name,
            team_id=r.team_id,
            level=r.level,
            active=r.active,
            max_concurrent=r.max_concurrent,
            current_load=r.current_load,
            recency=r.recency,
            profile=r.profile,
        )
        for r in ROSTER
    )


def services_by_id() -> dict[int, Service]:
    return {s.id: s for s in SERVICES}


def team_name(team_id: int) -> str:
    return next(t.name for t in TEAMS if t.id == team_id)


def ownership_of(emp_id: int) -> tuple[ServiceOwnership, ...]:
    """反向索引：员工负责哪些服务。派生自 SERVICES，避免两处手写漂移。"""
    out: list[ServiceOwnership] = []
    for svc in SERVICES:
        if svc.owner_id == emp_id:
            out.append(ServiceOwnership(svc.id, OwnershipKind.OWNER))
        if svc.backup_owner_id == emp_id:
            out.append(ServiceOwnership(svc.id, OwnershipKind.BACKUP))
    return tuple(out)


KNOWLEDGE: tuple[KnowledgeChunk, ...] = (
    KnowledgeChunk(
        "kb-interface-auth", "doc-interface", "接口鉴权失败排查",
        "接口返回 401 表示鉴权令牌过期或签名不正确。请先检查请求头 Authorization "
        "是否携带有效令牌，再确认服务器系统时间是否准确——时间偏差过大会导致签名校验失败。"
        "若确认令牌有效仍报错，请核对签名算法与密钥是否与平台一致。",
        ("接口", "401", "鉴权", "令牌", "Authorization", "签名"),
    ),
    KnowledgeChunk(
        "kb-interface-timeout", "doc-interface", "接口超时排查",
        "接口超时的常见原因是下游依赖响应缓慢或连接池耗尽。建议先查看调用链耗时分布，"
        "定位是网络、数据库还是第三方服务造成的延迟，再确认连接池配置与慢查询情况。",
        ("接口", "超时", "连接池", "慢查询", "调用链"),
    ),
    KnowledgeChunk(
        "kb-account-lock", "doc-account", "账号被锁定处理",
        "连续多次输入错误密码会触发账号锁定，通常锁定十五分钟。管理员可在成员管理中"
        "重置密码并解锁账号。若账号绑定的邮箱不可用，需要由组织管理员代为重置。",
        ("账号", "锁定", "密码", "解锁", "成员管理"),
    ),
    KnowledgeChunk(
        "kb-account-reset", "doc-account", "重置密码方法",
        "在登录页点击忘记密码，输入绑定邮箱获取验证码即可重置。重置成功后"
        "所有设备都需要重新登录。若未收到验证邮件，请检查垃圾邮件并确认邮箱地址无误。",
        ("重置密码", "忘记密码", "验证码", "登录"),
    ),
    KnowledgeChunk(
        "kb-deploy-hardware", "doc-deploy", "私有化部署硬件要求",
        "私有化部署最低需要四核八 GB 内存，建议十六 GB 以上；需要可访问外部模型服务的"
        "网络出口，或在内网部署独立的模型服务。磁盘建议预留一百 GB 以上用于知识与日志。",
        ("私有化", "部署", "硬件", "内存", "磁盘"),
    ),
    KnowledgeChunk(
        "kb-deploy-network", "doc-deploy", "私有化部署网络配置",
        "私有化部署需要开放应用端口与数据库端口；若使用外部模型服务还需开放出网访问。"
        "内网部署模型服务时，请确认域名解析与证书配置正确。",
        ("私有化", "部署", "网络", "端口", "出网"),
    ),
    KnowledgeChunk(
        "kb-db-pool", "doc-database", "数据库连接池耗尽",
        "连接池耗尽的典型表现是请求大量超时且数据库连接数打满。常见原因是慢查询"
        "长期占用连接，或连接未正确释放。建议先排查慢查询，再评估连接池上限是否需要调整。",
        ("数据库", "连接池", "耗尽", "慢查询"),
    ),
)
