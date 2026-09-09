# 05 · 未决问题清单（讨论进行中）

> 已定决策见 [README](README.md) 速览与 01-04。本清单是**尚未讨论完**的部分：按主题分组，标注影响阶段与建议倾向。约定：每项讨论定案后回写对应层文档，并从这里删去——本文件趋空 = 设计收敛。

## A. 核心模型

**A1 · ~~thinking/reasoning 块~~ `[已定稿 2026-09-09]`** —— 已随消息模型定稿：01 §2 含 `ThoughtBlock` 与完整 canonical transcript 约定；剩余「thought 是否入窗」并入 G2 重论。本条收口，不再卡 M1。

**A2 · system prompt 与人设 `[M3]`** —— system 内容从哪来、多 persona 切换、模板变量（日期/用户名/工具说明）由谁渲染。
建议：system = Window 的 system 区 Unit；人设/领域包 = Group 注入；模板渲染在 Presenter。

**A3 · 技能/指令包机制 `[M3/M4]`** —— pi 有 skills + AGENTS.md 渐进披露机制，ARiA 要不要独立的技能包概念。
建议：先不建独立机制——技能文档 = L3 artifact（markdown）+ `memory.recall` 检索 + Group 注入；不够用再升级。

**A4 · model registry `[M1 简版]`** —— 模型元数据（context 长度、价格、是否支持 tool call/thinking/多模态）的维护处。Budget 计价与 Planner 的预算反推（K 轮保底）都依赖它。
建议：pkg 静态表 + 配置覆盖；M1 只需 context 长度 + 价格两字段。

## B. 安全与权限

**B1 · ToolGuard 策略模型 `[M2]`** —— 白名单/危险工具确认/自动放行的配置表达；嵌入方的交互式审批（等人类点确认）如何接入。
建议：Guard = 策略链（规则表 → 可选交互确认），v1 只实现规则表。

**B2 · shell/文件工具边界 `[M2]`** —— cwd 限制、路径白名单、超时、输出截断上限、环境变量过滤。

**B3 · 凭据存储与 MCP 信任 `[M2/M4]`** —— per-user key/token 存放（环境变量/文件/keyring）；MCP server 的权限声明与信任模型。

## C. 记忆与规划细节

**C1 · 压缩的话题边界检测 `[M4]`** —— LLM 判定 vs 启发式（轮次间隔/工具调用模式）vs 混合。
建议：混合——启发式粗切 + LLM 修边（省钱且稳定）。

**C2 · Distiller 冲突解决 `[M4]`** —— supersede 的具体规则、置信度衰减曲线、何种冲突保留双方+时间戳。

**C3 · 评测集建设 `[M3]`** —— golden 集的来源与格式、CI 阈值、防回归劣化的门禁强度。

## D. 多 agent（远期）

**D1 · AgentID 一等化时机** —— 子运行（core/02 §9 已预留演进位）与多 loop 编排要不要进设计、何时进。当前定位是单 agent 通用助手。

## E. AV 接入

**E1 · 输出侧 TTS `[现在就要定方向]`** —— 影响事件模型要不要预留输出音频流槽位。改动成本最低的窗口就是 M1 定 message/event 模型时，拖到 M5 再加会伤到事件契约。
**E2 · 视频/转写 provider 选型 `[M5]`** —— 关键帧策略、帧率、转写流式协议。
**E3 · 实时性预算 `[M5]`** —— 语音场景首 token 延迟上限、打断恢复时限。

## F. 工程与运维

**F1 · 工程约定 `[M1 前]`** —— module path、golangci-lint 配置、CI、commit 规范、测试文件放置（同包 `_test.go`）。
**F2 · 存储格式版本化 `[M1]`** —— 事件/Unit schema 的版本号字段与迁移函数是否第一天带上。
建议：带上，成本是一个字段，收益是 M3 后不用写一次性迁移。
**F3 · trace 方案 `[M2]`** —— 自研 trace_id 传播 vs OTel 兼容。
建议：先自研轻量传播，预留 OTel 导出位。
**F4 · 成本报表 `[M3]`** —— usage 计量落库表结构与查询接口（Budget 已有，报表未设计）。
**F5 · 命名 `[ anytime ]`** —— ARiA 的含义/全称（影响 README 第一行与 module path）。:)

## G. 从 03 降级的设计（待重论）

**G1 · runtime 并发调度 `[M2 前]`** —— actor 单写者 / 多维准入 / 后台任务 / ingress / 限流 / 关机的完整草案在 [notes/concurrency-draft.md](notes/concurrency-draft.md)。原则方向（内存状态走 actor、信号量只守外部资源、排序靠队列语义声明）大概率保留，机制细节重论定稿。

**G2 · ContextWindow 与非线性 Planner 细节 `[M3 前]`** —— 草案 [notes/context-planning-draft.md](notes/context-planning-draft.md)（Unit 分级、打分公式、L0-L3、pull 工具、context.why、评测、thought 块入窗默认策略）。已定共识仅三句：窗口是一等可编程状态、选择非线性呈现线性稳定、ContextSource 契约（03 §2）。

**G3 · hook 挂点两问 `[M2 前]`** —— ① 挂点 2/4（LLM 响应变换、工具结果变换）用装饰器表达还是升格 core 一等槽（倾向先装饰器，B→C 升级不破坏契约）；② tool hook 具体场景盘点：是否有挂点全景表（03 §3）之外的需求。runtime 生命周期事件词汇补全（session/task 事件）也在此项。

## 建议的讨论顺序

卡 M1 的先定：**A4 → F2 → E1（只定方向）→ F1/F5（工程启动）**（A1 已随消息模型定稿收口）；
M2 前需收敛：**G1（并发调度重论）、G3（hook 两问）、B 组（安全权限）**；
M3 前需收敛：**G2（Planner 细节重论）、C 组（记忆细节）**。
