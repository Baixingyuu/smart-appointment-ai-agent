"""已结历史工单语料（派单 v4 的第三源）。

这批东西的作用是：让"同类问题当初谁在处理"成为可检索的证据。名册 24 人、服务 14 个，
如果历史只有探针里那条假单，第三源永远召不出东西，"先召回再选"就退化成两源。

写作规则（比内容更重要，改这批数据前先读）：

1. **归属只从归属表里取**：每条的 `assignee_id` 必须是 `service_id` 那一行的 owner 或
   backup。这条不靠我自觉 —— `HistoryRecord.kind` 是从 SERVICES 反查出来的，填了第三人
   会在导入期就抛异常。理由：历史一旦被模型引用，它就是软答案；我若照着自己的判断写
   "这单给了 116"，等于把自己的猜测洗成答案。owner 顶班是常态，所以 20 条里**只留 3 条
   backup**，且每条在 `note` 里写明为什么不是 owner。
2. **不与评测集重合**：45 条 `eval/datasets/assignment_v2.json` 的正文与这里任何一条都
   不能互相覆盖过 `COVERAGE_GATE`，这条闸在 `tests/test_history.py` 里跑，不用模型。
   重合就意味着"召回历史"等于"直接读到金标"。
3. **工单号从 5001 起**：真跑的工单号从 1 开始，两边撞号会让 `AssignmentLog.cited_tickets`
   的读数分不清引的是哪个世界。
4. **全部是已结单**：`resolved=True` 才进得了读取句柄（框架的构造期 metadata_filter）。
   未结单不进这批，只在探针里现造，用来证明隔离仍然成立。

`current_load` 是快照、不随历史变化，所以 backup 顶班的理由是当年的一句话记录，
不是今天能复算出来的事实 —— 提示词里也是这么写的。
"""
from __future__ import annotations

from dataclasses import dataclass

from .catalog import OwnershipKind, services_by_id
from .domain import Category, Priority


@dataclass(frozen=True)
class HistoryRecord:
    id: int
    title: str
    description: str
    service_id: int
    assignee_id: int
    category: Category
    priority: Priority
    note: str

    @property
    def searchable_text(self) -> str:
        return f"{self.title}。{self.description}"

    @property
    def kind(self) -> OwnershipKind:
        """从归属表反查，不是我手填的标记 —— 填了第三人会在这里炸。"""
        svc = services_by_id()[self.service_id]
        if self.assignee_id == svc.owner_id:
            return OwnershipKind.OWNER
        if self.assignee_id == svc.backup_owner_id:
            return OwnershipKind.BACKUP
        raise ValueError(
            f"历史工单 #{self.id} 的归属 {self.assignee_id} 既不是服务 {svc.id} 的 "
            f"owner({svc.owner_id})，也不是它的 backup({svc.backup_owner_id})"
        )


HISTORY: tuple[HistoryRecord, ...] = (
    HistoryRecord(
        5001, "订单创建批量失败，根因是线程池上限被改小",
        "客户侧新建订单一半以上拿不到回执，网关先看是连接被直接拒掉。回滚那次服务发布后立刻恢复。"
        "复盘发现新配置把执行线程从 200 压到 20，压测场景没覆盖大促流量。已补发布前的配置对比校验。",
        2001, 101, Category.INCIDENT, Priority.P1,
        "下单主链路的变更本来就归 101，处置与复盘都经他的手。",
    ),
    HistoryRecord(
        5002, "订单接口在大促当天雪崩，写库排队拖垮上游",
        "高峰期新建订单的响应从 80ms 涨到 8s，随后上游整片超时。当时把非核心的订单写入做了降级、"
        "并把重推队列延后消费才稳住。根因是订单落库的批量任务与在线写抢同一把锁。",
        2001, 107, Category.INCIDENT, Priority.P0,
        "backup 顶班：owner 101 手上另一起 P0 没结，跨栈救火由 107 接。",
    ),
    HistoryRecord(
        5003, "月汇总看板跑不完，长查询把资源占满",
        "财务每月初必跑的那张汇总表连续几天在凌晨超时。抓出来是一条没走索引的全表扫描，"
        "补了联合索引并挪到只读实例。同期临时取数的口径核对也一并交了。",
        2002, 102, Category.INCIDENT, Priority.P2,
        "报表侧的慢查询是 102 的固定口。",
    ),
    HistoryRecord(
        5004, "导出明细表偶发丢列，模板版本没对齐",
        "运营反馈某张周报导出后少两列，重跑几次有时正常。定位到模板发布后旧版本缓存没刷，"
        "把模板版本号带进缓存键后不再复现。改动落在看板服务本身。",
        2002, 102, Category.INCIDENT, Priority.P3,
        "模板的日常维护在 122 手里，但归属按 SERVICES 的表记 —— 画像不覆盖责任口。",
    ),
    HistoryRecord(
        5005, "批量导入的新账号进不去成员管理页",
        "HR 一次性导入的两百多个新账号里，有一部分登录后进不去成员管理，权限树是空的。"
        "原因是导入脚本绕过了默认角色绑定，补跑了一次绑角色任务。",
        2003, 106, Category.INCIDENT, Priority.P2,
        "账号与权限的日常口在 106。",
    ),
    HistoryRecord(
        5006, "发票红冲以后重开金额对不上",
        "客户把上月开错的发票红冲，重开时系统带出来的金额仍是红冲前的值。"
        "查是账单快照没随红冲刷新，手工修正了三张并加了快照刷新钩子。",
        2004, 106, Category.INCIDENT, Priority.P2,
        "账务核对与发票规则都走 106。",
    ),
    HistoryRecord(
        5007, "办公区到分支的专线隔几天抖一次，抖动集中在晚高峰",
        "多个部门反映远程桌面卡顿，链路监控看到专线有周期性丢包。运营商机线抽检确认是接头老化，"
        "割接换了光缆之后两周无告警。期间把跨机房任务错开了时段。",
        2005, 103, Category.INCIDENT, Priority.P2,
        "专线与机房链路由 103 值日。",
    ),
    HistoryRecord(
        5008, "主从切换演练后从库延迟追不上来",
        "季度演练切完，从库的复制位差一直收敛不了，读接口偶发超时。"
        "拆开看是演练期间堆积的大事务回放慢，改成分片回放并调了并行度。演练手册同步更新。",
        2009, 102, Category.CHANGE, Priority.P2,
        "集群变更由 102 主导。",
    ),
    HistoryRecord(
        5009, "连接数告警：应用侧连接泄漏没释放",
        "生产库连接数持续顶到上限，业务侧报取不到连接。DBA 抓出某服务在异常分支里漏了关闭，"
        "临时把池子上限抬一档救急，随后由应用方发版修掉泄漏点。",
        2009, 101, Category.INCIDENT, Priority.P1,
        "backup 顶班：owner 102 当时在休假，池子参数由接口组的 101 按预案先放开。",
    ),
    HistoryRecord(
        5010, "外部上报后台一处越权访问，已完成加固",
        "第三方报了一个靠改参数读到他人工单的问题，复现成立。补了服务端归属校验，"
        "并把同类接口整排过一遍。另出一份整改说明交合规留档。",
        2010, 104, Category.INCIDENT, Priority.P0,
        "漏洞响应与加固实施只有 104 做。",
    ),
    HistoryRecord(
        5011, "订单落到数仓的表比在线库少一天",
        "对账时发现数仓缺了某天的分区，而同步任务当天显示成功。追下来是任务先于上游落盘启动，"
        "改成等上游就绪信号触发，缺的那天回补完成。",
        2011, 101, Category.INCIDENT, Priority.P2,
        "同步管道的调度口在 101。",
    ),
    HistoryRecord(
        5012, "门店扫码枪批量掉线，驱动版本是元凶",
        "十几家门店陆续报同一批扫码设备识别不到，换设备无效。集中排查发现系统自动更新推了不兼容驱动，"
        "回退后统一加了更新白名单。属于终端设备的批量处置。",
        2012, 103, Category.INCIDENT, Priority.P2,
        "硬件批量故障的判断在 103 —— 这个服务没有 backup，见 SERVICES。",
    ),
    HistoryRecord(
        5013, "合作方轮换了密钥，签名校验大面积不通过",
        "接入方更换签名密钥后，我们这边缓存的旧公钥没失效，日志里全是校验失败。"
        "清缓存并订了密钥轮转通知，双方联调通过后结单。",
        2013, 101, Category.INCIDENT, Priority.P1,
        "鉴权与签名链路由 101 主值。",
    ),
    HistoryRecord(
        5014, "控制台个别页面翻页后筛选项被重置",
        "客户反映列表页翻到第二页时已选条件丢失。是路由状态没写进查询串导致组件重挂载，"
        "改了状态保持策略并补上回归用例。",
        2007, 105, Category.INCIDENT, Priority.P3,
        "控制台疑难杂症归口 105。",
    ),
    HistoryRecord(
        5015, "客户机房条件不满足，私有化安装卡在预检",
        "驻场安装时预检报出磁盘与网卡不符合要求，客户侧需要重新采购。把资源清单、出网端口与替代方案"
        "整理成一页对客文档，安装排到下一批。属于现场条件与咨询。",
        2006, 104, Category.CONSULTATION, Priority.P3,
        "backup 顶班：owner 103 在另一个客户现场，环境侧的安全预检由 104 出面对客。",
    ),
    HistoryRecord(
        5016, "桌面端某版本启动后白屏，本地缓存损坏",
        "少数同事的客户端更新后停在白屏，重装能好但下次又犯。定位到升级中断留下半截缓存文件，"
        "补了启动自检与自动重建。Windows 与 macOS 都覆盖。",
        2008, 105, Category.INCIDENT, Priority.P2,
        "桌面客户端的疑难同样归口 105。",
    ),
    HistoryRecord(
        5017, "客服工作台新增批量转派入口",
        "服务台要求把逐条转派改成按队列批量操作。工单后台新增两个权限点，上线时配了灰度名单，"
        "一周后全量。跨系统联调由工具台侧牵线。",
        2014, 107, Category.CHANGE, Priority.P3,
        "内部工具台是 107 的主战场（该服务无 backup）。",
    ),
    HistoryRecord(
        5018, "内部工单后台读不到知识库文章，权限组配漏",
        "新成立的两组同事登录后看不到知识条目，其他功能正常。是权限组同步任务少跑了一轮，"
        "补跑后生效，同时把同步频率改成每日两次。",
        2014, 107, Category.INCIDENT, Priority.P3,
        "跨系统兜底单落 107。",
    ),
    HistoryRecord(
        5019, "经营看板月初集中访问偶发超时",
        "每月 1 号上午看板打开慢，个别请求直接超时。是缓存策略在月初集中失效，"
        "改成错峰过期并预留预热任务。当月未再复现。",
        2002, 102, Category.INCIDENT, Priority.P2,
        "看板性能仍走 102，与 5003 是同一口的两件事。",
    ),
    HistoryRecord(
        5020, "订阅降级后仍按老档位出账",
        "客户把套餐降档，次月账单却按高价出。追到计费规则读的是下单时的快照而非生效套餐，"
        "改了取数来源并退了差价，原发票作废重开。",
        2004, 106, Category.INCIDENT, Priority.P2,
        "计费与账单异常的日常在 106。",
    ),
)

#: backup 顶班的条数 —— 语料自查用，多了就说明"当初谁在处理"开始漂脱归属表。
BACKUP_CASE_IDS: tuple[int, ...] = tuple(r.id for r in HISTORY if r.kind is OwnershipKind.BACKUP)
