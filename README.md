# 智能工单助手（Support Ticketing AI Agent）

智能工单助手是一个面向 SaaS/企业服务场景的技术支持工单调度系统。项目基于 FastAPI、LangChain、FAISS、SQLite 和多 Agent 协作架构，实现了意图识别、RAG 知识问答、工程师智能匹配、工单全生命周期管理和客户健康度分析等能力。

这个项目的核心目标不是只做一个工单表单，而是把技术支持中心每天需要处理的高频工作自动化：理解用户是想咨询还是报障，判断问题类型与优先级，按“等级过滤 + 专长匹配”二级策略匹配最合适的工程师，创建并分派工单，并在事后分析客户健康度、预警续费与流失风险。

## 项目背景

在企业级 SaaS 服务中，技术支持中心需要同时处理大量复杂事务：产品咨询、故障报修、工单派发、优先级判定、工程师能力匹配和客户续费维护。随着客户规模和工单量增加，传统的人工客服模式容易出现响应慢、派单不准和客户流失等问题。

因此，本项目用多 Agent 的方式重构这一流程：让系统像一位智能技术支持主管，主动理解客户需求，把咨询、报障、派单、客户洞察等任务分发给对应的专业 Agent 处理。

## 核心能力

- **智能意图识别**：自动区分用户是在咨询产品功能、提交工单，还是无关请求，并路由到对应 Agent。
- **多 Agent 协作**：任务分类 Agent、咨询 Agent、工单 Agent、客户洞察 Agent 分工处理，职责单一、易扩展。
- **RAG 知识问答**：基于产品文档知识库（FAISS 向量检索）生成自然语言回答，支持流式输出。
- **智能工单派发**：按优先级过滤工程师等级，再按问题描述与工程师专长的 Embedding 相似度匹配派单（二级匹配）。
- **工单全生命周期**：待分派 → 已分派 → 处理中 → 待确认 → 已解决 → 已关闭，状态流转完整。
- **客户健康度分析**：基于近 30 天客户行为（工单数、咨询数、活跃度）评估健康度，输出续费/流失预警。
- **Embedding 缓存优化**：知识库向量缓存，减少重复向量计算。
- **分层架构**：严格五层架构，下层不反向调用上层，避免循环依赖。

## 系统架构

项目采用严格的五层架构，核心原则：**下层不能反向调用上层**。

```text
Web & Application Layer
    ↓  app.py, web/：页面、路由入口、系统启动
API Layer
    ↓  api/：外部接口、请求编排、响应封装
Agents Layer
    ↓  agents/：AI Agent、意图路由、对话流程控制
Services Layer
    ↓  services/：业务逻辑、推荐算法、向量处理
DB Layer
    ↓  db/：数据模型、数据库连接、Repository
```

### 允许的调用方向

- Web 层调用 API 层
- API 层调用 Agents 层或 Services 层
- Agents 层调用 Services 层
- Services 层调用 DB 层

### 禁止的调用方式

- 下层反向调用上层
- Web 层绕过 API 直接访问 Services 或 DB
- Agents 层绕过 Services 直接访问 DB
- Services 层调用 Agents、API 或 Web

## Agent 设计

### Task Classification Agent（任务分类）

系统主调度器，负责分析用户输入、判断任务类型（咨询 / 工单 / 其他），并分发给对应 Agent。

```text
用户输入 → 意图分析 → Agent 路由 → 响应协调
```

### Consultation Agent（咨询）

负责产品知识问答，使用 RAG 流程从产品文档知识库检索相关内容，再结合大模型生成回答。

```text
任务分类 → 知识检索 → FAISS 相似度搜索 → 流式回答
```

### Ticketing Agent（工单）

负责工单全流程：解析问题描述、判断类型与优先级、匹配工程师、创建并分派工单。

```text
任务分类 → 解析工单信息 → 优先级判定 → 工程师匹配（等级过滤 + 专长匹配）→ 工单创建
```

### Customer Insight Agent（客户洞察）

偏向后台智能分析，根据客户交互记录和工单历史评估健康度，输出续费/流失预警。

```text
行为记录 → 健康度评分 → 流失风险判定 → 续费/挽留建议
```

## 核心设计思想

### 1. 用任务分类降低系统复杂度

系统不让一个 Agent 处理所有事情，而是先判断意图再分发，让咨询、工单、洞察等逻辑保持独立，也更容易扩展新 Agent。

### 2. 用 RAG 解决专业知识回答

产品功能、套餐等知识适合通过知识库维护。RAG 让回答基于可控知识来源，而不是完全依赖大模型自由发挥。

### 3. 用「等级过滤 + 专长匹配」二级匹配做派单

工单派发不只看技术栈是否匹配：先按优先级过滤工程师等级（P0 仅专家级），再按问题描述与工程师专长的 Embedding 相似度排序，取最相关者派单。该策略刻意**不做“当前负载均衡”**：一是演示场景的并发量不足以体现负载调度的价值，二是负载均衡更适合放在队列/调度层（如工单池轮询、工程师工作台抢单）而非首轮匹配，避免把“能力匹配准确性”与“负载公平性”两个目标耦合在同一排序中。

### 4. 用分层架构保证可维护性

Agent 负责智能流程，Service 负责业务逻辑，Repository 负责数据访问，每层只关心自己的职责。

## 技术栈

- **后端框架**：FastAPI、Uvicorn
- **AI 框架**：LangChain
- **大模型接入**：兼容 OpenAI 格式的模型提供商（Qwen、DeepSeek、Zhipu、OpenAI、Azure OpenAI）
- **向量检索**：FAISS
- **数据库**：SQLite、SQLAlchemy
- **RAG 能力**：Embedding、向量索引、知识库检索、提示词构建
- **流式响应**：Python AsyncGenerator
- **前端页面**：Jinja2 模板、静态 CSS
- **配置管理**：python-dotenv

## 项目结构

```text
support-ticketing-ai-agent/
├── agents/                          # 多 Agent 智能层
│   ├── task_classification_agent.py # 任务分类与主路由
│   ├── consultant_agent.py          # RAG 咨询 Agent
│   ├── ticketing_agent.py           # 工单 Agent
│   ├── customer_insight_agent.py    # 客户洞察 Agent
│   ├── task_classification/         # 意图识别、状态管理、路由逻辑
│   ├── consultant/                  # 知识检索、提示词、回答生成
│   ├── ticketing/                   # 工单解析、工程师匹配、消息构建
│   └── customer_insight/            # 健康度分析
├── api/                             # API 编排层
│   ├── ticket.py                    # 工单接口
│   ├── consultation.py              # 咨询接口
│   ├── task.py                      # 任务分类接口
│   ├── chat_handler.py              # 流式聊天处理
│   ├── engineer.py                  # 工程师接口
│   ├── customer.py                  # 客户与健康度接口
│   └── knowledge.py                 # 知识库管理接口
├── services/                        # 业务逻辑层
│   ├── ticket_service.py            # 工单业务逻辑（创建、分派、状态流转）
│   ├── knowledge_service.py         # 知识库管理
│   ├── engineer_service.py          # 工程师管理与查询
│   ├── customer_service.py          # 客户管理与健康度
│   └── text_embedding.py            # Embedding 与向量处理
├── db/                              # 数据持久化层
│   ├── models.py                    # SQLAlchemy 模型
│   ├── db_router.py                 # 数据库路由
│   ├── base/                        # 数据库基础接口
│   └── repositories/                # Repository 数据访问封装
├── config/                          # 配置模块
│   ├── constants.py                 # 常量、枚举、优先级-等级映射
│   ├── database.py                  # 数据库配置
│   ├── model_provider.py            # 模型与 Embedding Provider 工厂
│   ├── settings.py                  # 应用配置
│   └── time_config.py               # 时间配置
├── web/                             # Web 页面层
│   ├── routes.py                    # 页面路由
│   ├── templates/                   # HTML 模板
│   └── static/                      # 静态资源
├── tests/                           # 测试用例
├── app.py                           # 应用入口
├── requirements.txt                 # Python 依赖
├── .env.example                     # 环境变量模板
└── README.md                        # 项目说明
```

## 快速开始

### 1. 创建虚拟环境

```bash
python -m venv .venv
```

Windows PowerShell：

```powershell
.\.venv\Scripts\Activate.ps1
```

macOS / Linux：

```bash
source .venv/bin/activate
```

### 2. 安装依赖

```bash
pip install -r requirements.txt
```

### 3. 配置环境变量

```bash
cp .env.example .env
```

在 `.env` 中填写模型配置：

```env
MODEL_PROVIDER=qwen
LLM_API_KEY=your_llm_api_key_here
LLM_BASE_URL=your_openai_compatible_chat_base_url_here
LLM_MODEL=your_chat_model_name_here

EMBEDDING_PROVIDER=qwen
EMBEDDING_API_KEY=your_embedding_api_key_here
EMBEDDING_BASE_URL=your_openai_compatible_embedding_base_url_here
EMBEDDING_MODEL=your_embedding_model_name_here

DATABASE_URL=sqlite:///./data/support_ticketing.db
```

### 4. 启动服务

```bash
python -m uvicorn app:app --host 127.0.0.1 --port 8000 --reload
```

启动后访问：

- Web 页面：http://127.0.0.1:8000
- API 文档：http://127.0.0.1:8000/docs

## 测试

```bash
pytest
```

## 主要页面

- 首页聊天与工单入口：`web/templates/index.html`
- 工单列表：`web/templates/tickets.html`
- 工程师列表：`web/templates/engineers.html`
- 客户健康度：`web/templates/customers.html`
- 知识库管理：`web/templates/knowledge_management.html`

## 后续规划

- 增加 Agent 自我反思机制，评估工单分派质量。
- 引入多轮推理链，提升复杂故障的根因定位能力。
- 增加客户登录、权限控制与多租户数据隔离。
- 支持工单满意度回访与知识库自动沉淀。
- 支持 Docker 部署、云数据库与标准日志监控。

## 项目价值

这个项目把多 Agent、RAG、工单调度和客户洞察放在同一个真实业务场景中验证。它既是一个企业技术支持中心原型，也可以作为学习 AI Agent 工程化、分层架构、RAG 系统和业务自动化的综合实践项目。
