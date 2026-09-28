# 草稿 · runtime 并发调度（未定稿）

> 2026-08-27 从 03-context-build 降级为草稿：设计超前于共识深度。
> **2026-09-11 更新**：M2 不再被调度阻塞——v1 定稿为一 Session 同时只跑一轮、新输入走 core.Queue（互斥+队列语义），schedule/ 子包不建（03 §5、05 G1）。本草案仅在出现真实并发场景（多实例准入、后台任务、限流）时回来定稿，原则方向大概率保留。

## 原则（三条）

- **R1 内存状态不走锁，走 actor**：会话状态、持久化写入各归单 goroutine（mailbox），串行与有序天然获得——core 飞轮哲学在 runtime 的延续。
- **R2 信号量只守外部资源**：provider 并发/TPM、MCP 连接。拿信号量守内存结构 = 退化为锁，禁止。
- **R3 排序靠队列语义声明，不靠协调**：不同任务的顺序要求由各自入口显式声明。

## 并发拓扑

```
嵌入方(多goroutine): 宿主程序/集成测试/未来语音、视频daemon
     │ Submit(RunSpec)                        ┌─ 后台任务: ObserveSource/Compact/Ingest/MCPRefresh
     ▼                                        │   → Worker Pool（三级 class）
RunScheduler ─多维准入─► Session Actor 池      │
     │                     │飞轮 Run          │
     │                     ▼                  │
     │                 Provider 调用 ─► LimitedProvider(限流) ─► 外部 API
     │                     │
     │                事件总线(durable)
     │                     ▼
     │                Persist Actor ─► SQLite/JSONL（单写者）
```

## Session Actor（会话串行化）

每 session 一个常驻 goroutine：session base ctx（core run ctx 的派生父级）+ 当前 Loop + 触发输入路由（空闲→起 Run；Run 中同会话新输入→Queue）。**同一 session 至多一个 Run**（语义正确性：两个 run 交错写同一 messages 即数据损坏）——**这一条已在 v1 定稿**（03 §5），只是 v1 用互斥+core.Queue 直接实现、不建常驻 Actor。空闲 LRU 回收，resume 无状态丢失。

## RunScheduler（准入）

- 多维信号量 `global → provider → user` 固定获取顺序（死锁防线），全部带 ctx；v1 单用户只开 global+provider；
- 排队按 class 分道 + 道内 FIFO；队列满 → `ErrAdmissionQueueFull`，不无限积压；
- 队列深度/等待时长出指标，上限凭指标调。

## Persist Actor（事件溯源落盘）

- 单写者；订阅 durable 事件流（channel 天然保序）→ 会话内 FIFO 写入，顺序保证来自通道不来自锁；
- 批量刷盘（攒 N 条或 50ms 一个事务，WAL）；关机排水有界 deadline。

## 后台任务系统

任务：`ObserveSource(source, session, turn)`、`Compact(session)`、`Ingest(ns, docs)`、`MCPRefresh(server)`。

- 三级 class（Interactive/Background/Batch）加权轮转（如 8:1:1）+ 硬性预留 k 个 worker 给后台——双向不饿死；
- 重试：指数退避+抖动；幂等键 `(kind, sessionID, turnID)` + 任务表去重（崩溃后从事件日志重放）；死信入表可重投，绝不静默吞。

## Ingress 策略（触发源声明队列语义）

> 注：此处 Ingress 指「准入/排队」语义，与 03 §6 的 ingress（输入形成状态机，成轮/归主/判终）不是同一层——前者把已成轮的输入送进 Run，后者把原始感知加工成输入。命名冲突 M2 实现时收口。

| 触发源 | 语义 |
|---|---|
| 嵌入方库 API | 立即准入（排队即背压） |
| 语音转写 | 严格 FIFO（乱序=答非所问） |
| 视频流分析 | coalesce：`key=session+kind` 只保留最新 pending，队列压到 O(kind 数) |
| 定时 | cron，Batch class |

## Provider 限流层

每 provider：并发信号量 + RPM 令牌桶 + TPM；容量静态切分 Interactive 80% / Background+Batch 20%。位置：provider 适配器外包 `LimitedProvider`，core 无感知。

## 关机协议（四阶段）

1. 封口（入口停新请求，Scheduler 停准入）→ 2. 收飞轮（收尾 deadline，超时 Interrupt）→ 3. 排水（事件→Persist 清空；后台取消可重放）→ 4. 释放（连接关闭、指标快照；丢弃数=0 才是干净关机）。

## 观测（草稿）

各队列深度/速率/滞留、信号量等待分布、活跃飞轮数、persist 批大小与延迟、任务重试/死信、限流等待。slog + expvar。
