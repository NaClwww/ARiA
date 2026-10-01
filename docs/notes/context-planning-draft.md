# 草稿 · ContextWindow 与非线性规划（未定稿）

> 2026-08-27 从 03-context-build 降级为草稿：细节超前于共识深度，**M3 前重新讨论定稿**（05 清单 G2）。
> 已定共识：ContextWindow 是一等可编程状态；非线性发生在选择而非呈现；ContextSource 契约（Collect/Observe）；**v1 每轮组装模型**（压缩记忆+当前感知+新输入 + 间隙异步压缩，03 §5）。以下为非线性 Planner 的草稿细节，届时在其上扩展。

## ContextWindow（状态设计草稿）

**三角分离**：ContextWindow（状态：Unit 图+策略+版本）→ Planner（策略：打分选择）→ Presenter（投影：稳定区域顺序的 []Message）。窗口 ≠ transcript。

**并发契约**：每 session 一个 Window 单写（Session Actor）；变更 = 类型化命令进 mailbox，轮边界生效；Unit 图不可变持久结构 + COW 新版本原子发布，Load() 永不阻塞。

**Handle（变更 API 草稿）**：

```go
h := window.For(Role)   // RoleBusiness / RoleRuntime / RoleModel(无)
h.Inject(UnitSpec{Kind, Region, Content, Importance, TTL, GroupID})
h.Pin/Unpin/Evict/EvictGroup/Replace/PatchPolicy/Why(id)
```

- Group 事务包（一包原子注入/整体撤下）、TTL 自动过期、声明重要度而非位置；
- 权限：业务只能动自己注入的；模型无 Handle（只能经 pull 工具），防自我膨胀；
- 每命令发 durable `WindowCommandApplied` 事件，重放可重建窗口状态。

## Planner（非线性选择草稿）

线性滑窗四缺陷：importance≠recency、工具结果失衡、无视结构、AV 数据量崩溃。

三个非线性维度：树状历史（当前路径物化+旁路分支摘要在场）、需求驱动分页（任何轮原文可回页）、类型分层压缩（AV 原始流永不进 L0）。

**Unit 模型**：kind 先验等级（system/taskState 保留级、fact/summary 高价值、dialogue 保底、toolResult/artifact 快驱逐、retrieved 竞争级）+ Provenance 溯源链。铁律：摘要只删窗口席位，不删原文。

**内存层级**：L0 工作窗口 / L1 会话库（全量原文，树状 parent 指针）/ L2 长期记忆（待重新设计）/ L3 外部语料与工件。

**算法（每轮纯函数）**：保留集（system+taskState+Pin）→ 保底集（最近 K 轮，K 由预算反推）→ 剩余预算打分背包：`score = w_rel·relevance + w_imp·importance + w_rec·e^{-λ·Δturn} + w_ref·被引用加权`；类型策略截断；固定区域顺序物化 `[system | facts | summaries | dialogue | input]`；溢出信号投递压缩任务（沿话题边界）。**选择非线性，呈现线性稳定。**

**Pull 路**：给模型两个普通工具——`history.load(turnRange|unitID)`（沿 Provenance 回页）、`artifact.open(ref,part)`。Push 给基线，Pull 兜底。

**可观测**：context.why() 三层（命令史→决策史→当前构成）；decisions 事件带分数分解。

**评测**：golden 集（长对话+第 N 轮断言关键事实在场或一次 pull 找回）+ 策略消融，进 CI。

**分期**：M3 线性基线（K 轮+类型驱逐）→ M4 评分竞争+溯源回页 → M5 向量+树状分支+AV 分层。
