"""
工程师匹配评测（离线，无需 LLM / 数据库）

评测工程师匹配的准确率：
- 基线：随机匹配（期望命中率约 1/10 = 10%）
- 本项目：专长相似度匹配（离线 Jaccard 相似度替代 embedding，Top-1 命中率）

运行：python eval/run_matching_eval.py
"""

from metrics import top_k_accuracy
import random


# 工程师专长（与项目默认工程师数据一致）
ENGINEERS = [
    {"name": "张伟", "level": "expert", "skills": {"后端", "数据库", "API", "高并发", "性能"}},
    {"name": "王强", "level": "senior", "skills": {"云原生", "容器", "K8s", "部署"}},
    {"name": "李娜", "level": "senior", "skills": {"前端", "移动端", "SDK", "UI"}},
    {"name": "赵敏", "level": "intermediate", "skills": {"数据", "报表", "BI", "指标"}},
    {"name": "刘洋", "level": "expert", "skills": {"网络", "证书", "SSO", "安全"}},
    {"name": "孙丽", "level": "intermediate", "skills": {"计费", "账号", "权限", "账单"}},
    {"name": "周杰", "level": "senior", "skills": {"消息", "推送", "Webhook", "队列"}},
    {"name": "吴婷", "level": "intermediate", "skills": {"AI", "模型", "Prompt", "向量"}},
    {"name": "郑斌", "level": "senior", "skills": {"集成", "API", "Webhook", "协作"}},
    {"name": "何静", "level": "intermediate", "skills": {"客户成功", "Onboarding", "答疑"}},
]

PRIORITY_LEVEL_MAP = {
    "P0": {"expert"},
    "P1": {"expert", "senior"},
    "P2": {"senior", "intermediate"},
    "P3": {"intermediate", "junior"},
}

# 评测集：30 个工单 + 人工标注的最合适工程师
EVAL_SET = [
    {"desc": "API 接口返回 500，数据库连接超时", "skills": {"后端", "API", "数据库"}, "priority": "P1", "expected": "张伟"},
    {"desc": "高并发场景下服务响应变慢", "skills": {"后端", "高并发", "性能"}, "priority": "P1", "expected": "张伟"},
    {"desc": "K8s 集群 Pod 一直重启", "skills": {"云原生", "K8s", "容器"}, "priority": "P1", "expected": "王强"},
    {"desc": "容器镜像部署失败", "skills": {"容器", "部署", "云原生"}, "priority": "P2", "expected": "王强"},
    {"desc": "移动端 SDK 集成报错", "skills": {"移动端", "SDK", "前端"}, "priority": "P2", "expected": "李娜"},
    {"desc": "前端页面渲染白屏", "skills": {"前端", "UI"}, "priority": "P2", "expected": "李娜"},
    {"desc": "报表数据对不上，指标口径有问题", "skills": {"数据", "报表", "指标"}, "priority": "P3", "expected": "赵敏"},
    {"desc": "BI 看板不刷新", "skills": {"BI", "数据"}, "priority": "P3", "expected": "赵敏"},
    {"desc": "SSO 单点登录失败", "skills": {"SSO", "安全", "网络"}, "priority": "P0", "expected": "刘洋"},
    {"desc": "SSL 证书配置报错", "skills": {"证书", "网络"}, "priority": "P1", "expected": "刘洋"},
    {"desc": "账单金额计算错误", "skills": {"计费", "账单"}, "priority": "P2", "expected": "孙丽"},
    {"desc": "账号权限配置异常", "skills": {"账号", "权限"}, "priority": "P2", "expected": "孙丽"},
    {"desc": "消息推送收不到", "skills": {"消息", "推送"}, "priority": "P2", "expected": "周杰"},
    {"desc": "Webhook 回调没触发", "skills": {"Webhook", "消息"}, "priority": "P2", "expected": "周杰"},
    {"desc": "模型 API 调用返回异常", "skills": {"AI", "模型", "API"}, "priority": "P3", "expected": "吴婷"},
    {"desc": "Prompt 调优效果不好", "skills": {"Prompt", "AI"}, "priority": "P3", "expected": "吴婷"},
    {"desc": "第三方平台集成失败", "skills": {"集成", "API", "Webhook"}, "priority": "P2", "expected": "郑斌"},
    {"desc": "企业协作平台对接问题", "skills": {"集成", "协作"}, "priority": "P2", "expected": "郑斌"},
    {"desc": "新手如何上手产品", "skills": {"客户成功", "Onboarding"}, "priority": "P3", "expected": "何静"},
    {"desc": "产品使用有疑问", "skills": {"答疑", "客户成功"}, "priority": "P3", "expected": "何静"},
    {"desc": "数据库查询性能优化", "skills": {"数据库", "性能"}, "priority": "P1", "expected": "张伟"},
    {"desc": "网络连接超时排查", "skills": {"网络"}, "priority": "P2", "expected": "刘洋"},
    {"desc": "前端 SDK 版本兼容问题", "skills": {"前端", "SDK"}, "priority": "P2", "expected": "李娜"},
    {"desc": "K8s 集群扩容咨询", "skills": {"K8s", "云原生"}, "priority": "P3", "expected": "王强"},
    {"desc": "数据同步延迟", "skills": {"数据"}, "priority": "P3", "expected": "赵敏"},
    {"desc": "推送通道切换", "skills": {"推送", "消息"}, "priority": "P3", "expected": "周杰"},
    {"desc": "向量检索结果不准", "skills": {"向量", "AI"}, "priority": "P3", "expected": "吴婷"},
    {"desc": "订阅续费操作咨询", "skills": {"计费", "账单"}, "priority": "P3", "expected": "孙丽"},
    {"desc": "API 限流问题", "skills": {"API", "后端"}, "priority": "P2", "expected": "张伟"},
    {"desc": "证书续期流程", "skills": {"证书", "安全"}, "priority": "P3", "expected": "刘洋"},
]


def jaccard(a: set, b: set) -> float:
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def specialty_match(skills: set, priority: str) -> str:
    """专长匹配：等级过滤 + 专长相似度排序，返回 Top-1 工程师名"""
    allowed = PRIORITY_LEVEL_MAP.get(priority, set())
    candidates = [e for e in ENGINEERS if e["level"] in allowed]
    if not candidates:
        return None
    best = max(candidates, key=lambda e: jaccard(skills, e["skills"]))
    return best["name"]


def random_match(priority: str) -> str:
    """随机匹配（基线）"""
    allowed = PRIORITY_LEVEL_MAP.get(priority, set())
    candidates = [e for e in ENGINEERS if e["level"] in allowed]
    if not candidates:
        return None
    return random.choice(candidates)["name"]


def main():
    expected = [item["expected"] for item in EVAL_SET]

    # 本项目：专长匹配
    matched = [specialty_match(item["skills"], item["priority"]) for item in EVAL_SET]
    our_acc = top_k_accuracy(matched, expected)

    # 基线：随机匹配（多次取平均）
    random_accs = []
    for _ in range(100):
        random.seed()
        rand_matched = [random_match(item["priority"]) for item in EVAL_SET]
        random_accs.append(top_k_accuracy(rand_matched, expected))
    baseline_acc = sum(random_accs) / len(random_accs)

    print("=" * 60)
    print("工程师匹配评测（30 个标注工单）")
    print("=" * 60)
    print(f"随机匹配（基线）：  Top-1 命中率 {baseline_acc * 100:.1f}%")
    print(f"专长匹配（本项目）：Top-1 命中率 {our_acc * 100:.1f}%")
    print(f"提升：{(our_acc - baseline_acc) / baseline_acc * 100:.1f}%")
    print("=" * 60)


if __name__ == "__main__":
    main()
