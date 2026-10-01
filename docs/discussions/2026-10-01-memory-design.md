# 2026-10-01 · 长期记忆首版闭环（ContextSource 接线 + SQLite 默认源）

> 起因：三块现状——会话摘要（runtime/window）只存内存、重启即失；对话历史（JSONL）只写不读；长期记忆停留在 04 §4 契约一行。接记忆插件前必须先补 runtime 的读写接线。本记录定稿**首版闭环**，并收编 [2026-08-27 embedding/RAG 讨论](2026-08-27-embedding-rag.md)的相关待定项（D1/D2/D5/D6/D7/D10）。
>
> 首版闭环 = **SQLite 持久化 → 落盘确认后后台提炼 → 有限召回（词面+线索） → 重启可用**；现有摘要压缩（window 间隙压缩）原样保留，两层各司其职。
>
> **同日晚追加拍板**：经 Hindsight 实况调研（§11），引擎选型定为**接入 Hindsight，自建 SQLite 源整体废弃**——§6 转为被否方案存档（防重议），分期重排见 §9。

## 1. 目标与非目标

**目标**：重启后 ARiA 记得住长期事实与共同经历；每轮组装能召回相关背景；模型能主动查（pull）；提炼在后台，不碰对话路径。

**非目标（明确不做，防蔓延）**：

- 非线性 Planner / 窗口命令（G2 草稿，M3 前重论）——v1 召回结果是「注入」不是「提名进打分竞争」；
- 语义浮现层2（每轮 push embedding，2026-08-27 §12：评测门槛后才开）；
- 会话摘要持久化（`WindowCompressedData` 只报规模不含摘要文本，维持现状——重启丢摘要由长期记忆兜底，格式带版本号将来好迁移）；
- 名册/认主/私聊过滤（05 E4，挂插件层协商）；
- JSONL 历史重放/恢复（v1 只写不读，维持 03 §5）。

## 2. 契约与类型落点

```
pkg/mem            Episode / Unit / Query（纯类型：插件可见，与 pkg/message 同级）
runtime/memory     ContextSource 接口 + Collect 超时包装 + Pump（写路泵）
plugins/memory/hindsight   默认源（hindsight-clients/go，自托管服务；规格见 §11）
plugins/memory     memory_recall 工具（吃 ContextSource，与 Collect 同一检索入口）
```

core 零改动。落点遵循既有先例：jsonl 不 import runtime/persist 靠结构化类型成立，hindsight 适配器同样只 import `pkg/mem`（HTTP 客户端封在插件内）。

```go
// pkg/mem
type Unit struct {
    Kind       string    // profile | fact | event（词汇定死，扩展走文档评审——tag 同款纪律）
    Subject    string    // 归属：说话人 id / "agent" / ""（共同事实）
    Content    string
    Importance float64
    At         time.Time
    Meta       map[string]string   // source run/session 等
}
type Query struct {
    Text     string
    Scope    ctxx.Scope    // Namespace 进 WHERE 硬隔离；UserID 是提示不是过滤器
    Limit    int
    MaxChars int
}
type Episode struct {
    ID     string             // RunID，幂等锚点
    Scope  ctxx.Scope         // UserID = 触发说话人（≠ 全部事实的主人）
    Input  message.Message    // 真实触发输入——组装上下文绝不入内（§4）
    Turns  []message.Message  // injected + assistant + tool（含被拒），按事件序
    At     time.Time
    Meta   map[string]string  // end_reason 等
}

// runtime/memory
type ContextSource interface {
    Name() string
    Collect(ctx context.Context, q mem.Query) ([]mem.Unit, error) // 快路径，毫秒级
    Observe(ctx context.Context, ep mem.Episode) error            // 慢路径，幂等
}
```

**工具命名拍板**：`memory_recall`（非 `memory.recall`）——core `validToolName` 只认 `[a-zA-Z0-9_-]`、禁点号（部分网关严格校验），`artifact_open` 是同款先例。草稿里的 `history.load` 将来同理叫 `history_load`。

## 3. 读路：窗口组装时 Collect

`Window.Assemble` 签名改为 `Assemble(ctx, input)`（runtime 内部改动，调用点只有 `Session.Input`），新增装配期 `SetSources([]ContextSource)`（与 `SetSystem` 同模式）。插入位置按 03 §5 分层定稿：

```
[system] → [<memory> 压缩摘要] → 近轮 → [<context source="memory"> ← 新] → 新输入
```

纪律：

- **超时降级**：runtime 统一包装（默认 100ms 量级），超时/出错记日志、本轮无检索——不信任插件自觉，检索失败不影响对话；
- **容量**：Query 带 Limit/MaxChars，window 侧按总字符截断（多源叠加不爆）；
- **空结果不注入**：无匹配不产生 `<context>` 块——闲聊不插往事；
- **噪声三件套**（继承 2026-08-27 §12）：top-k ≤5、冷却（源侧记 last_injected，注入后 N 轮内不再提名；profile 常驻除外）、**假阳性比漏掉更糟**；
- 每个 run 只在 `Input` 组装时 Collect 一次，run 内多轮工具循环不重复检索。

与 2026-08-27 三级浮现的对位：**profile 常驻 = 层0 保守版**（小画像而非全量 200 条常驻索引——音箱上下文预算有限，全量索引是配置可升的档位）；**线索命中注入 = 层1**（触发线索列，§6）；**向量 = 层2/pull**（§6，评测门槛）。G2 非线性 Planner 落地后，召回从「注入」升级为「提名进打分竞争」，结构不变。

（同日晚：引擎选型后，本段分层的**实现**归 Hindsight 服务侧；Collect 的语义与降级/噪声纪律不变，作用于适配器。）

## 4. 写路：落盘确认 → 后台 Observe

接线事实（2026-10-01 核对代码）：一个 run 的 durable 事件流里 `agent_end` 是最后一条，且全部经 `Recorder.Consume` 单 goroutine 顺序写；`window_compressed` 走 `SetOnCompress` 直写旁路（不参与 Episode，无影响）。

方案：**`persist.Recorder` 加 `OnAppend func(loop.Event)` 钩子**——每次 Append 成功后在落盘 goroutine 上回调（契约注明「必须快、不得阻塞」）。agent 把它接到 `runtime/memory.Pump`：

1. **Pump.Feed(ev) 在落盘 goroutine 上积累事件**：`AgentStart` 取 `InitialInput` **末条**为真实输入；`UserMessageInjected` / `MessageEnd`(assistant) / `ToolExecEnd`（含 Denied 与 IsError）按序追加；`agent_end` **落盘成功后**组成 Episode 入队。（这套「哪些事件进历史」的取数已实现在 `loop.HistoryMessage(ev)`——窗口结算已改共用它，Pump 不得自带第二份解释。）
2. **单 worker 后台消费**（有界队列，默认 64）：依序对各源 `Observe`（Detached ctx + 超时 + 有限重试），失败响亮记日志不致命——记忆是 best-effort，durable 承诺仍归 L1。队列满丢弃并 warn，内容仍在 JSONL 可再衍生。
3. **落盘失败 / Close 超时** → 对应 Episode 永不触发 Observe：没写进历史的不提炼，与 durable 语义一致。
4. **关停**：`Session.Close` 在 persist drain 之后加 Pump 有界排空（复用 grace 风格）。
5. **无 Store 不跑记忆**：`memory.enabled && record.path==""` → 启动报错（fail-loud）。

**防重复入记忆（结构保证）**：Episode 输入 = `InitialInput` 末条 + injected + 产出。`AgentStart.InitialInput` 是组装后全量（system + `<memory>` + 近轮 + 新输入），**整包提炼会把旧摘要与召回内容反复当新事实**——只取末条从结构上排除。

**备选与否决**：settle() 复用 inputBuf/turnBuf + 按 RunID 等落盘回执——少一份重复积累，但引入跨 goroutine 的 runID 匹配状态；确认与构造同在落盘消费者一处更简，否。

## 5. 身份归属

- engine 建 Session 时补设 `Namespace`（配置 `[memory].namespace`，空则 `session.id`）与 `AgentID`；`mergeScope` 只被 host ctx 的 UserID/AgentID 覆盖，Namespace 不会被 Sink 冲掉；
- `Episode.Scope.UserID` = 触发说话人（run ctx merge 已有：Sink 按 `[名字]` 传 UserID），仅作提炼提示；
- **事实级归属在提炼时定**：蒸馏提示词读 `[名字]` 前缀（与 `SpeakerInstruction` 同一约定），每条候选事实标 Subject——「A 说 B 喜欢猫」落 `subject=B`，不整轮归给触发者；
- 召回 v1 **不过滤** subject（家庭共享设备，B 的事实对 A 也该可见，行内带归属标注）；私聊过滤留名册（05 E4）。

## 6. SQLite 源内部（已否方案存档——引擎选型见 §11）

表：`units(id, ns, kind, subject, content, cues, importance, status, superseded_by, embedding BLOB, embed_model, source_session, source_run, created_at, updated_at, last_injected)` + `observed_runs(run_id PK)`。embedding 列第一天预留（F2 版本化精神：字段先占位，启用不迁移）。

**Observe（写路管线，全部后台）**：

1. 查 `observed_runs` 幂等门（RunID）；
2. 渲染 Episode：工具调用先于正文（`ProviderCompressor` 同款教训：调用参数是 assistant 实际说过/做过的话）、带结果成败——**提示词明写「工具失败的动作不算既成事实」**；
3. **单次 LLM 提炼**：产出候选事实（content + subject + kind + importance + **触发线索 cues 2~4 个**，受控词表纪律承 2026-08-27 D7），JSON 约束输出；
4. **append-only 写入**：只做精确去重（同 ns+subject+content），**不做写入期 UPDATE/DELETE 合并**——Mem0 经典管线的「提炼后判 ADD/UPDATE/DELETE」已否（错删、丢上下文；业界已转向 append + 时间轴）；矛盾交给检索期：同主题候选按时间新者胜，被取代者降权/标注「（已过时）」；
5. 单事务写 units + 标记 run。崩溃在事务前 = 重跑重提炼（幂等，浪费一点 token），事务原子性保证不半写。

（原规划「第二步合并 LLM」随 append-only 一并砍掉——每 episode 从两次 LLM 调用降为一次，记录在案防重议。）

**Collect（读路）**：

- `kind=profile` 恒返回（字符上限内）——Memobase 式小画像常驻；
- 其余按 **词面 LIKE（content）∥ 线索列 LIKE（cues）** 双路提名，打分 = 相关度 × importance × 时间衰减，topk + 总字符截断 + 冷却过滤；
- **v1 不上 FTS5**：unicode61 对中文不分词、trigram 查不了双字词（「杭州」）——事实表几百行规模 LIKE 全扫毫秒级且零分词依赖（对 04 §4 原「FTS5 检索」表述的修订，理由回写）；
- **向量通道 = 升级位（P4+，条件 = embedding 端点可用）**：写侧离线 embed（256 维 float32 blob，MRL 截断，content-hash 缓存）→ 读侧 **Go 暴力余弦**（千行级 <1ms；sqlite-vec 是 C 扩展、纯 Go 不能载——2026-08-27 D2 定论）→ 词法∥向量 **RRF 融合**（D6）；**pull-only 起步**（D1）：向量只在 `memory_recall` 工具内用，push 路 Collect 永远零 embedding；Embedder 走 2026-08-27 §2 契约草案（Query/Document 角色显式传）。

**重启**：SQLite 天然持久，Collect 跨重启可用；会话摘要不持久化（§1 非目标）。

## 7. 宿主装配与配置

engine（`internal/aria-host`）：memory 开 → 建 sqlite 源（注入同一 provider 供蒸馏）→ `agent.Config.ContextSources` + 工具表追加 `memory_recall`（挂法照 `artifact_open` 的冲突检查先例）→ Close 时源随引擎收尾。core 不认识记忆库，宿主只接线。

```toml
[memory]                     # 草图；实现落地时随 06 §3 补全注释与热改边界
enabled = true
path = "var/memory.db"
namespace = ""               # 空 = session.id
[memory.collect]
topk = 5
max_chars = 1200
timeout_ms = 100
cooldown_turns = 3
[memory.distill]
model = ""                   # 空 = 跟随 provider.model
max_tokens = 2000
```

（同日晚：引擎换 Hindsight 后，`path` 改为 engine 端点、`[memory.distill]` 整段作废——提炼参数在 Hindsight 侧配置；`[memory.collect]` 保留，作用于适配器。定稿草图随 P3 实现落 06。）

## 8. 与既有讨论的衔接

| 2026-08-27 待定项 | 本记录处置 |
|---|---|
| D1 检索路线 | 采纳其倾向并定稿：memory 侧词法先行、向量 pull-only 起步（写侧离线） |
| D2 sqlite-vec | 定稿暴力余弦（已在 04 §4 回写） |
| D5 两级/三级召回 | 层0（profile 常驻）+ 层1（cues）随 v1；memory_recall = pull 兜底 |
| D6 RRF | 采纳为向量通道融合形态（升级位） |
| D7 tag 纪律 | cues 列 = 触发线索，受控词表 + 2~4 个/条 + 高频降权 |
| D10 浮现三级 | 层2（push embedding）维持评测门槛后；「浮现≠注入」的完整形态等 G2 |

未处置：D3（RAG 文档侧 trigram）、D4（Embedder 契约正式化——P4 前定）、D8/D9。

引擎换 Hindsight 后（§11），检索内部机制（词法/向量/RRF 的落点）由服务侧承担；本表为已否自建路线的历史记录。

业界对位（简短）：分层（原文/事实/画像）、读写契约分离、落盘确认、幂等锚点与通用记忆系统骨架同构；两处修正的依据——语义缺口（「那次旅行」词面不含「杭州」）与消歧路线（Mem0 新版 append+时间轴）。

## 9. 分期

| 期 | 内容 | 验收 |
|---|---|---|
| P1 | pkg/mem + runtime/memory 契约 + 窗口源槽（读路） | fake 源表驱动：位置/顺序/超时降级/容量截断，-race 干净 |
| P2 | Recorder.OnAppend + Pump + engine 补 Namespace | 断言 Observe 严格晚于 agent_end 的 Append；落盘失败不触发；关停排空 |
| P3 | **plugins/memory/hindsight 适配器** + EPYC compose 部署（外置 PG + pgroonga、本地 bge-m3 / bge-reranker-v2-m3、LLM = deepseek 或 zai） | §10 五道题（对真引擎）+ recall 延迟实测（LAN）；幂等台账验证 |
| P4 | memory_recall 工具 + 重启验证 + 真机联调（C2 音箱） | 五道题真模型复验；push Collect 按 M1/M3 节奏放开 |

## 10. 验证（首批 golden 案例）

1. 「我不喝咖啡了」之后不再推荐咖啡 → 检索期时效判断（同主题新者胜）；
2. A 说「B 喜欢猫」，不记成 A 喜欢猫 → subject 归属提炼；
3. 「上周去杭州」能答「那次旅行去了哪里」 → cues 线索跨词面召回（词面双路兜底）；
4. ARiA 说「灯已经关了」但工具失败 → 不形成错误记忆（IsError 渲染 + 提示词规则）；
5. 重启后记得、闲聊不乱插往事 → DB 重开 Collect + 空结果不注入。

①②④的 LLM 判断用脚本化 fake provider 固定输出做确定性测试；真模型效果放 P4 真机。测试纪律：表驱动、runtime 侧无网络无磁盘、plugin 侧 httptest 假引擎，`-race` 干净。

## 11. 引擎选型：Hindsight（同日晚拍板）

**决策**：接入自托管 [vectorize-io/hindsight](https://github.com/vectorize-io/hindsight) 作为唯一长期记忆引擎；§6 自建 SQLite 方案**废弃**。runtime 接线（§2–§5）不变——ContextSource 契约保持引擎中立是仓库纪律（runtime 不 import 具体插件、消费方窄接口），与留后路无关。

**调研核实**（2026-10-01，官方仓库与文档）：

- MIT，~44k stars、迭代很快（2026-04 v0.5 → 05 v0.7），arXiv 论文 + 第三方复现基准；API = retain / recall / reflect；
- 部署：docker compose（API :8888 / 管理 UI :9999，自带 Prometheus/Grafana）；**单容器内嵌 pg0 不适合我们**（见下条）；另有 Helm / pip / 托管云；
- **中文三件套（默认全不对，必须显式配置）**：BM25 默认 native tsvector 中文无效（issue #1077）→ **pgroonga**（TokenBigram + NFKC150，**需外置 PG**，单容器模式用不了）；embedding 默认 bge-small-en-v1.5 英文 only → 本地 **BAAI/bge-m3**；reranker 默认英文 → **bge-reranker-v2-m3**。语言处理在 LLM 提示词级（自述 not a hard guarantee），实体保留原文，`LLM_OUTPUT_LANGUAGE` 可强制中文；
- provider：LLM 原生支持 **deepseek / zai** / 任意 OpenAI 兼容端点 / litellm / ollama；embedding 与 rerank 有本地默认（免 key）；文档明说 DeepSeek 无 embeddings 端点——正好全走本地；
- Go 客户端：官方 OpenAPI 生成（`hindsight-clients/go`），retain(bank, items) / recall(bank, query)→Results[].Text；
- 隔离：bank = 硬边界（↔ Namespace）；**人物关系是原生能力**（social graph 网络）——「A 说 B 喜欢猫」的归属正是它建模的东西。

**适配器规格（plugins/memory/hindsight）**：

- `Observe(ep)` → `retain(bank=ns, content=Episode 渲染文本, timestamp=ep.At)`——渲染器沿用 §4 原则（工具调用先行、IsError 标注、`[名字]` 前缀保留）；**timestamp 必传**：Hindsight 的 recency 衰减（线性一年、±10%）与 temporal 检索臂（时间指涉查询）都按记忆**发生时间**算，缺日期 = 永远 0.5 中性 + 时间臂失效；
  - 排序机制备忘（2026-10-01 查证）：四路 → RRF(k=60) → 预筛 top300 → cross-encoder → recency×temporal×proof 三 boost（各 ±10%/±10%/±5%，不可配）→ token 装填。**相关性主导、时间是轻推**；「过时」主要靠 consolidation supersede，不靠衰减。嫌衰减弱可调 `RECALL_STRATEGY_BOOSTS` 加权 temporal 臂，或适配器按返回时间戳重排（返回是否带日期，P3 实测确认）；
- `Collect(q)` → `recall(bank, q.Text)` → Results 映射 `[]mem.Unit`；
- `memory_recall` → 同 recall 入口，limit 放宽；
- **幂等**：retain 不承诺幂等；适配器侧留 run 台账（已观察 RunID 的本地小记录，形态实现时定）——Pump 至少一次投递 × 台账去重；
- `reflect` 暂不接（未来白捡的推理入口）。

**部署落点**：EPYC VM（vidar-nacl-node）compose：hindsight + 外置 PG（pgroonga 镜像）+ 本地 bge-m3 / bge-reranker-v2-m3（CPU）；与 asr-gateway / miku-tts 同机。

**风险与前置**：

- recall 延迟未实测（CPU embedding + 四路检索 + cross-encoder 重排是变数）；旋钮：ANN 扫描上限、BM25 selective terms、reranker 换小模型或 SiliconFlow API；Collect 超时放宽到 300ms 量级、降级语义不变；
- 项目迭代快：**锁版本部署，升级当变更对待**；
- 提炼中文质量最终由 §10 五道题对真引擎裁决；
- **前置阻塞：DeepSeek key 失效（尾号 41eb）待换**（或改配 zai）——retain 提炼没有 LLM 跑不了，P3 起不了步。

**纪律继承**：pull 先开（memory_recall），push Collect 过五道题 + 延迟实测后再放（M1）。

## 待定决策清单

| # | 决策 | 倾向 | 影响文档 |
|---|---|---|---|
| M1 | push Collect（每轮自动召回注入）开启时机 | pull（memory_recall）先开；push 过五道题 + 延迟实测后再放 | 03 §5、04 §4 |
| M3 | Collect 超时 / top-k / 冷却定值 | P4 真机调参后回写默认值（push 超时基准 300ms 量级起） | 06 配置 |
| M4 | `memory_write` / `memory_forget` 主动写工具（模型自纠记忆） | P4 后按需——接口已预留（工具直连源，不经新契约） | 04 §3 |
| M5 | `[memory]` 配置段落入 06（engine 端点 / bank / 开关） | 随 P3/P4 实现落 | 06 §3 |

M6 已拍板（接入 Hindsight，§11）；M2（Embedder 契约正式化）随引擎选型**撤销**——embedding 由 Hindsight 内部承担，2026-08-27 D4 一并挂起。

## 已合入

- 03 §5：零件位③（背景资料槽）从「预留」改定稿；实现落点表补 `runtime/memory` 行；§2 写路补充落盘确认表述；
- 04 §3：memory 源工具命名 `memory_recall`（禁点号）；
- 04 §4：默认实现重写——append-only 消歧、词面+线索检索（FTS5 表述修订）、向量通道升级位（暴力余弦非 sqlite-vec）、Observe 落盘确认；
- 05：C2 收窄（方向已定）、C3 补 golden 起点、G2 注记线性接线定稿；
- docs/README：未实现清单补记忆源一项；
- 2026-08-27 记录：D1/D2/D5/D6/D7/D10 标注已合入。
- **同日晚修订（引擎选型，§11）**：自建 SQLite 源废弃（§6 转被否存档）；04 §4 默认实现改为 Hindsight 适配器、04 §6 依赖表以 hindsight-clients/go 替换 modernc.org/sqlite；05 C2 收口、G2 注记同步；2026-08-27 已合入条目补引擎选型说明；M6 拍板、M2 撤销。
