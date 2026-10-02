# 长期记忆：候选方案

4 个候选方案按 ARiA 侧实现规模从小到大排列，均未定稿；事实依据与来源见 [survey.md](survey.md)。

## 方案 A：画像常驻（文件式记忆）

- 机制：每位成员一节、家庭共同一节、ARiA 自身一节的结构化记忆文本，每轮全量注入上下文，不做检索。
- 写入形态：
  - A1 后台改写：每轮结束后在后台调用 1 次 LLM，对记忆文本执行增加、修改、删除。对应 LangMem 的后台写入、ChatGPT 的 Dreaming。
  - A2 模型工具编辑：模型在对话中调用记忆工具编辑文件。对应 Claude API memory tool、Letta MemFS；写入发生在对话路径上，增加该轮延迟（LangMem 所述热路径）。
- 同类做法：ChatGPT saved memories、Claude `<userMemories>`、Claude Code 的 MEMORY.md、Letta `system/` 文件。
- 优点：无检索引擎与 embedding；读路无额外延迟；时效取代通过改写条目完成。
- 限制：容量受上下文预算约束，2026-08-27 记录 §5 按 200 条、约 4k token 估算；具体经历只能保留少量；改写式更新在无版本记录时存在误删风险。
- ARiA 侧工作：1 个 ContextSource 实现 + 文件存储。

## 方案 B：Go 进程内检索库（自建）

- 机制：方案 A 的画像，加一张事件与事实条目表（只追加，带时间与触发线索词）；每轮检索 top-k 注入，另提供 pull 工具。
- 纯 Go 选型（survey §6）：
  - ncruces/go-sqlite3：无 CGO；FTS5 注册自定义分词器，接入 gse 中文分词；vec1 向量扩展未到 1.0。
  - bleve：CJK bigram 分析器为纯 Go；向量检索依赖 FAISS，不满足无 CGO。
  - chromem-go：精确余弦，10 万条单次查询约 40 ms。
  - 向量通道需要本地 embedding 服务（例如 bge-m3）。
- 依据：GroupMemBench 的多方对话评测中，BM25 基线与多数记忆系统持平或更高，词法检索作为首个通道有实测支持。
- 优点：无外部服务；数据格式、去重键、隔离方式由 ARiA 定义。
- 限制：提炼、时效取代、说话人归属需自行实现；ARiA 侧工作量大于方案 A 与方案 C。
- 对应：已删除设计记录 §6 的 SQLite 方案。

## 方案 C：外部记忆引擎

- 机制：写路把每轮对话原文提交引擎，读路调用引擎检索；ARiA 侧为适配器。
- 与比较维度冲突较少的候选：

| 引擎 | 符合项 | 冲突或待实测项 |
|---|---|---|
| Hindsight | 官方 Go 客户端；中文可配（pgroonga、bge-m3、bge-reranker-v2-m3）；`operation_id` 去重；读路不调用 LLM | 需外置 PG；#4691 繁简同名实体拆分；客户端 API 版本低于服务端；CPU 延迟未实测 |
| MemOS | 中文提示词与 jieba 为缺省；fast 模式读写均不调用 LLM；条目带归档版本链 | Neo4j + Qdrant（偏好记忆另需 Milvus）；无 Go 客户端；消息无说话人字段 |
| EverOS | 单进程部署，SQLite + LanceDB；`sender_id`／`sender_name` 说话人字段；jieba | 无 Go 客户端；#378 中文输入生成英文记忆未关闭；无幂等字段 |
| Graphiti | 双时间轴失效模型 | 提示词仅英文；#1728 失效误判；Neo4j／FalkorDB；无 Go 客户端 |

- 冲突较多的引擎与原因见 survey §2：Mem0 开源版中文 BM25 不生效，Memori 提取请求发往托管服务，Supermemory 引擎无源码，Letta 服务端已归档，Memobase 自 2026-01-11 起无提交。
- 共同限制：需额外部署 Python 服务；多数引擎每次写入的 LLM 调用次数未公开；窗口压缩摘要不属于引擎职责。

## 方案 D：分期组合

- 第 1 期：方案 A。
- 第 2 期：画像容量不足、或出现无法召回具体经历的实际案例时，接入检索层，在方案 B 与方案 C 之间选择，先只作为 pull 工具提供。
- 理由：代码中长期记忆尚未实现，需求未确定；第 1 期以较小工作量取得「重启后仍记得」的版本，再用真实对话数据确定第 2 期。

## 对比

| 维度 | A 画像常驻 | B 进程内检索库 | C 外部引擎 | D 分期组合 |
|---|---|---|---|---|
| 每轮读取延迟 | 无额外延迟 | 毫秒级（数百至数千条） | 未实测 | 第 1 期无额外延迟 |
| 外部依赖 | 无，只用现有 LLM | 无；向量通道需 embedding 服务 | Python 服务 + 数据库 + 模型容器 | 第 1 期无 |
| 容量 | 百级 | 千至万级 | 万级以上 | 随期数扩展 |
| 时效取代 | 改写条目 | 新条目按时间优先 | 由引擎实现，方式各异 | 第 1 期同 A |
| 说话人归属 | 提示词约束 | 自行实现 | 由引擎实现，中文效果未实测 | 第 1 期同 A |
| ARiA 侧工作量 | 小 | 大 | 中：适配器 + 部署 | 第 1 期小 |

## 已确定需求（2026-10-01）

原「待定问题」1～8 已逐项确定，其中第 6 条（每轮后台提炼的 LLM 调用上限）已删除，其余编号保持不变。第 4 列为该决定对上文候选方案的约束，依据见 survey.md。

| # | 问题 | 决定 | 对候选方案的约束 |
|---|---|---|---|
| 1 | 引擎形态 | 外部服务，ARiA 经 HTTP 接入 | 方案 C 符合；进程内实现的方案 A、B 与本条不符 |
| 2 | 未识别说话人（访客、声纹 unmatched） | 只记共同事件：只记录涉及家庭成员或 ARiA 的事件，不记录访客个人事实 | 提交引擎前需按说话人区分输入；候选引擎的说话人字段：EverOS 有 `sender_id`／`sender_name`，Hindsight 经 `context` 字段与实体参数传递，MemOS 消息无说话人字段 |
| 3 | 「查看记忆」「忘掉某件事」语音指令 | 第 2 期按需 | 首版无此项约束 |
| 4 | 成员之间不得互相召回的内容 | 不存在，全员共享 | 每个家庭使用一个命名空间（Hindsight 的 bank 等），召回不按成员过滤 |
| 5 | 单个家庭的记忆条目数量级 | 万级及以上 | 方案 A 的容量（百级）不满足，需要检索层 |
| 7 | 摄像头画面中的事件 | v1 进入记忆 | 视觉事件需转为文本描述后提交引擎；候选引擎的图像输入能力不在本次调研范围内 |
| 8 | ARiA 自身的承诺（例如「答应明天提醒」） | 作为记忆，并在到期时触发 | 到期触发需要 ARiA 侧的定时机制；候选引擎的到期触发能力不在本次调研范围内 |

## 写入与召回流程（2026-10-02 讨论结论）

本节记录写入时机、暂存与提交、会话之间的衔接、记忆服务接口与合并的执行方。记忆服务在测试期采用 Hindsight 适配层（见「测试期后端」）；ARiA 侧的实现状态见本节「ARiA 侧实现状态」。

### 已定

| # | 项 | 结论 |
|---|---|---|
| 1 | 写入点 | 每个压缩批次：上下文告急时的压缩批次，以及会话切换时的剩余原文 |
| 2 | 要点提取 | 压缩调用一次同时输出摘要与要点：输入为旧摘要与批次原文，模型与压缩相同；要点为空即本批不写入。会话切换时的批次只输出要点，不生成摘要 |
| 3 | 写入方式 | 要点逐批上传暂存，会话结束后提交；提交时统一合并，处理会话内与跨会话的重复、修正与状态变化 |
| 4 | 暂存 | 由记忆服务端提供 |
| 5 | 提交触发 | 由 ARiA 触发 |
| 6 | 幂等 | 暂存以每批新生成的 TraceID 去重（当前 run ctx 不带 TraceID，须按批次生成）；提交以 SessionID 去重 |
| 7 | 新会话 | 不带入上一会话的摘要与原文：会话切换时清空窗口（`window.Reset`，已实现） |
| 8 | 召回 | 会话开始时 Start 返回用户背景与上一会话的最近要点（2026-10-02 修订，取代「返回内容由服务端处理」）：用户背景 = 家庭成员名单与各自身份、每位成员的长期偏好与重要事实、ARiA 未到期的承诺，每位成员 10 条、合计 1500 字；最近要点 = 上一会话的最后 10 条，文本带「上一会话：」前缀；结果整个会话内不变。第 n 个会话可召回未提交的暂存（一般为第 n−1 个会话）。对话中的具体内容由模型调用 memory_recall 工具检索（第 11 项），不保留每轮自动召回 |
| 9 | 关闭缺口 | 搁置：会话切换前关闭进程时，尚未成批的原文不进入记忆 |
| 10 | 合并执行方 | 服务端：End 的语义为服务端按自身策略提交与合并（见「记忆服务接口」）；合并的时机、模型与规则属于服务端设计。ARiA 侧只负责提取要点与调用 4 个接口 |
| 11 | Recall 工具 | `memory_recall`（`runtime/memory.RecallTool`）：参数 query（必填）、limit（可选，缺省 10、上限 20）；超时 3 s（`memory.Client.Recall`）；namespace 与 session_id 取 ctx 的 Scope；结果经 `memory.Render` 渲染，再经 `toolkit.Truncate` 截断；配置了记忆服务时宿主装入工具表并在人设后追加 `memory.RecallToolInstruction` |

### ARiA 侧实现状态（2026-10-02）

| # | 工作项 | 状态 |
|---|---|---|
| 1 | 会话切换：无操作超时、新 SessionID、清空窗口（`window.Reset`） | 已实现 |
| 2 | 记忆服务接口与失败处理：`runtime/memory` 的 `Service`（接口）与 `Client`（重试 3 次、Start 超时 5 s、Recall 超时 3 s） | 已实现 |
| 3 | Stage：`window.PointExtractor` 与 `Window.SetOnPoints` 上报要点，`agent` 生成 batch_id（`ctxx.NewTrace().TraceID`）与 seq 并投递 | 已实现 |
| 4 | End / Start：会话切换时依次投递，`NewSession` 时投递 Start；Start 的结果经 `memory.Render` 渲染、`window.RecallMessage` 写入窗口的召回位置（`Window.SetRecalled`）；会话首轮等待 Start 返回，上限 `session.recall_wait_ms`（`agent.Config.RecallWait`） | 已实现 |
| 5 | namespace：配置项 `session.namespace`（缺省 `default`），宿主写入 `Scope.Namespace` | 已实现 |
| 6 | 压缩调用同时输出要点：`window.ProviderCompressor` 实现 `window.PointExtractor`（`CompressWithPoints`）；窗口注册了要点上报（`Window.SetOnPoints`，配置了记忆服务时由 `agent` 注册）时改用该调用，keeplast 不提取 | 已实现 |
| 7 | 会话切换时对清除的原文提取要点（压缩实现为 `PointExtractor` 时）：`Window.Reset` 返回会话内摘要、原文与最后的批次序号（`window.Cleared`），记忆服务的后台 goroutine 调用 `Window.ExtractPoints`（`ProviderCompressor.ExtractPoints`，以 `Reset` 返回的会话内摘要为上下文，使用该会话最近一轮 run ctx 的值），以下一个序号 Stage，之后 End、Start | 已实现 |
| 8 | Recall 工具 `memory_recall`（`runtime/memory.RecallTool`，已定第 11 项，取代每轮 Recall） | 已实现 |
| 9 | 记忆服务实现：`plugins/memory/hindsight`（测试期后端，见下文）；配置段 `[memory]`（engine / base_url / dir / members），`assemble.Memory` 装配为 `agent.Config.Memory`，同时装入 memory_recall 工具；`engine` 为空时不调用记忆服务、不提取要点 | 已实现（未对接真实 Hindsight 实测） |

记忆服务调用由每个 Session 的一个后台 goroutine 按投递顺序执行（队列容量 64，队列满时丢弃该次调用并输出 Error 日志），每次调用的 ctx 带该调用所属会话的 Scope。会话切换前已完成批次的要点先于 End 投递；`Close` 时队列中尚未执行的调用丢弃（关闭缺口搁置）。

### 时序

```
每个压缩批次（上下文告急）
  压缩调用 → {摘要, 要点}；摘要留在窗口，要点上传暂存
会话切换（无操作超时）
  剩余原文 → 只提取要点 → 上传暂存 → 通知会话结束（服务端提交、合并）
  窗口清空，SessionID 更换
新会话开始
  Start：提交遗留会话 → 返回用户背景与上一会话最近要点 → 注入新会话（首轮等待，上限 session.recall_wait_ms）
对话中
  模型按需调用 memory_recall 工具 → 在正式记忆与暂存中检索 → 结果作为工具结果返回
```

### 记忆服务接口（2026-10-02 确认）

记忆服务接口独立于 ContextSource：只有记忆服务实现以下 4 个接口，RAG 源只实现 Collect。每次调用都带 namespace（需求 4：每个家庭一个命名空间；宿主目前未设置 `Scope.Namespace`，接入时补上）。

| 接口 | 调用时机（ARiA 侧位置） | 参数 | 返回 | 幂等 | 失败处理 |
|---|---|---|---|---|---|
| Stage | 每个压缩批次的要点提取完成后，要点为空时不调用（压缩路径） | namespace, session_id, batch_id（每批新生成的 TraceID）, seq（本会话内批次序号，从 1 起）, points | 无 | batch_id：服务端据此拒绝重放 | 重试 3 次，间隔 1 s / 2 s / 4 s；仍失败则输出 Error 日志，本批要点丢弃（原文仍在 jsonl） |
| End | 会话切换：最后一批 Stage 之后（agent.onIdle） | namespace, session_id, ended_at（该会话最近一轮的结算时刻） | 无 | session_id | 重试 3 次；仍失败则输出 Error 日志，由下一次 Start 补齐 |
| Start | 新会话开始：会话切换时与进程启动时，异步执行（agent.onIdle、agent.NewSession） | namespace, session_id, started_at | items（用户背景 + 上一会话最近要点，已定第 8 项） | session_id | 超时取 `session.recall_wait_ms`（缺省 5000 ms，0 时为 5 s）；超时或失败则新会话不带召回结果，输出 Warn 日志 |
| Recall | memory_recall 工具（模型调用，已定第 11 项） | namespace, session_id, query（模型给出）, limit（缺省 10、上限 20） | items | 无 | 超时 3 s；超时或失败则工具返回 IsError 结果，输出 Warn 日志 |

服务端职责：

- Stage：按 batch_id 暂存。
- End：标记会话结束，按自身策略提交：合并本会话暂存与相关已有记忆、写入正式记忆、清除暂存。
- Start：把此前未结束的会话视为已结束并提交（进程异常退出后的遗留会话由此处理）；返回用户背景（成员身份、长期偏好与重要事实、未到期的承诺）与上一会话的最近要点（已定第 8 项）。
- Recall：在正式记忆与暂存中检索；按时间排序，同一要点不同时以暂存与正式记忆两种形式返回。

召回条目（items）的字段：text、source（committed 正式记忆 / staged 暂存）、session_id、at。Start 的结果放入新会话窗口的记忆位置（system 之后），整个会话内不变；会话首轮等待 Start 返回后再组装，上限为配置项 `session.recall_wait_ms`（缺省 5000 ms，同时作为 Start 的超时；0 = 首轮不等待）；到达上限仍未返回时首轮不带召回，结果到达后自下一轮起出现在组装结果中（2026-10-02 主人裁定：首轮须带召回，以避免依赖个人记忆的首句指令缺少依据）。memory_recall 工具的结果同样经 `memory.Render` 渲染。

### 测试期后端：Hindsight 适配层（2026-10-02）

主人 2026-10-02 决定：第一期用现有引擎测试记忆流程，之后切换到自有记忆服务。所选引擎为 Hindsight（方案 C 候选）。查证结果（来源：hindsight.vectorize.io 开发者文档、仓库 README 与 hindsight-clients/go，2026-10-02）：

- 接口：retain `POST /v1/default/banks/{bank}/memories`（items[].content / context / timestamp / document_id / metadata / tags / update_mode；async 时 operation_id 幂等）；recall `POST …/memories/recall`（query / budget / max_tokens / prefer_observations / tags / tags_match / query_timestamp；结果 text / occurred_start / mentioned_at / document_id / metadata / tags）；bank `PUT /v1/default/banks/{bank}`（请求体字段全部可选）。
- 不具备暂存与会话级提交：retain 完成后即可检索。同一 document_id 重复 retain 缺省 replace（删除旧文档及其记忆后重新提取）。
- 部署：单容器 `ghcr.io/vectorize-io/hindsight:latest`，API 端口 8888、UI 端口 9999，内嵌 PostgreSQL（pg0）；LLM 经 `HINDSIGHT_API_LLM_PROVIDER` / `HINDSIGHT_API_LLM_BASE_URL` / `HINDSIGHT_API_LLM_MODEL` / `HINDSIGHT_API_LLM_API_KEY` 指向 OpenAI 兼容端点。中文：embedding 须设 `HINDSIGHT_API_EMBEDDINGS_LOCAL_MODEL=BAAI/bge-m3`、reranker `HINDSIGHT_API_RERANKER_LOCAL_MODEL=BAAI/bge-reranker-v2-m3`（容器内本地推理，官方建议 8 GB 内存）；BM25 需外置 PostgreSQL + pgroonga，内嵌 pg0 不支持。
- Go 客户端 `github.com/vectorize-io/hindsight/hindsight-clients/go`：OpenAPI 生成，README 标注 API 0.4.11（服务端 0.10.x），无独立版本标签。本实现不引入，用 net/http 实现用到的 3 个端点。

测试期取舍：内嵌 pg0 + bge-m3 + bge-reranker-v2-m3，不启用 BM25（只有向量与重排通道）。启动命令（DeepSeek 作提取模型）：

```bash
docker run -d --name hindsight --restart unless-stopped -p 8888:8888 -p 9999:9999 \
  -e HINDSIGHT_API_LLM_PROVIDER=openai \
  -e HINDSIGHT_API_LLM_BASE_URL=https://api.deepseek.com \
  -e HINDSIGHT_API_LLM_MODEL=deepseek-flash \
  -e HINDSIGHT_API_LLM_API_KEY=$DEEPSEEK_API_KEY \
  -e HINDSIGHT_API_EMBEDDINGS_LOCAL_MODEL=BAAI/bge-m3 \
  -e HINDSIGHT_API_RERANKER_LOCAL_MODEL=BAAI/bge-reranker-v2-m3 \
  -v hindsight-data:/home/hindsight/.pg0 \
  ghcr.io/vectorize-io/hindsight:latest
```

适配层（`plugins/memory/hindsight`，实现 `memory.Service`）：测试期由适配层充当记忆服务端，暂存在 ARiA 进程内（已定第 4 项「暂存由记忆服务端提供」在测试期由此满足）。

| 接口 | 适配层行为 |
|---|---|
| Stage | 追加写入 `<dir>/<namespace>/<session_id>.jsonl`（每行一个批次：batch_id、seq、暂存时刻、要点），按 batch_id 去重；空要点不写 |
| End | 读取该会话全部批次，每批一个 retain item（content = 每行 `[类别] 文本（说话人：…）`，document_id = `<session_id>#<seq>`，update_mode = replace，timestamp = 暂存时刻，metadata = {session_id, namespace, seq}，context = 「ARiA 家庭对话要点」）；承诺另成 item（document_id = `<session_id>#<seq>#c<i>`，tags = commitment，timestamp = 到期时间），一次同步 retain；成功后文件改名 `.committed`，同一 namespace 只保留最近 1 个 `.committed`；没有暂存文件时不调用引擎 |
| Start | 后台 goroutine 按 End 流程提交目录中其余 `.jsonl`（进程异常退出的遗留；同步 retain 含 LLM 提取，时长超过 Start 的超时，因此不阻塞 Start，ctx 取消不影响提交；失败只记 Error 日志，留待下一次 Start；提交前其要点仍可经 Recall 的暂存匹配召回）；bank_id = namespace，首次使用时 PUT 建立；对配置的成员名单逐人 recall（query「<名字>的身份、偏好与重要事实」，budget low，prefer_observations），每人最多 10 条；再 recall tags = commitment，保留到期时间在 [now, now + 30 天] 内或无时间的条目；以上合计 1500 字封顶；最后追加当前会话以外最近一个会话（已提交或未提交的文件均计入，以最后一批的暂存时刻为准）的最后 10 条要点（文本前缀「上一会话：」，未提交的 Source = staged） |
| Recall | recall（budget mid，max_tokens = limit × 120，范围 512～4096）；本地全部 `.jsonl` 中与查询词匹配的要点（CJK 二字组合与小写原词的子串匹配）作为 staged 条目并入：引擎结果最多 limit − reserve 条、暂存最多 reserve 条（reserve = min(匹配数, limit/2)），合并后按时间升序，无时间的在后 |

结果映射：Item.Text = text；SessionID = metadata.session_id（缺省取 document_id 的 `#` 前部分）；At = occurred_start，缺省 mentioned_at；Source = committed。

配置（aria.toml `[memory]`）：`engine`（空 = 不接入；hindsight）、`base_url`（缺省 http://127.0.0.1:8888）、`dir`（缺省 memory-staging）、`members`（成员名字列表）。

测试期限制：未对接真实 Hindsight 实测（开发机无 Docker），HTTP 交互只经 httptest 伪服务端验证；kind 与 due 只以文本与 tags 保留；承诺在批次 item 与承诺 item 中各出现一次，去重依赖 Hindsight 的 observation 整合；暂存匹配为子串匹配，无语义检索；PUT 建立 bank 的请求体为 {"name": namespace}（Hindsight 快速开始中 retain 可直接使用新 bank_id，PUT 作为显式建立）。

### 要点的格式（2026-10-02 确认）

| 项 | 结论 |
|---|---|
| 输入 | 分两段：「既往摘要」（会话内压缩：上一版摘要；会话切换：会话内的压缩摘要）只用于理解上下文；「本批对话」为要点的唯一来源，以避免重复提取已暂存的内容。会话内压缩的新摘要涵盖两段 |
| 字段 | `text`（独立成句、写明主语）、`kind`（fact 事实 / preference 偏好 / event 事件 / commitment ARiA 的承诺）、`speakers`（涉及的说话人名字，取自 `[名字]` 标注）、`due`（到期时间，RFC 3339，只用于 commitment） |
| 记录规则 | 只记录涉及家庭成员或 ARiA 的事实、偏好、事件与 ARiA 的承诺；不记录访客（未识别说话人）的个人事实（需求 2）；不记录寒暄与无信息量的往来；每批最多 10 条 |
| 输出格式 | 会话内压缩：`{"summary": "...", "points": [...]}`；会话切换时只提取：`{"points": [...]}`。提示词附当前时间（消息无时间戳），供模型换算相对时间 |
| 摘要长度 | 800 字以内（由提示词限定） |
| 输出不合法 | 从第一个「{」起解码一个 JSON 对象，其后的文本忽略；解码失败即本次调用失败。会话内压缩按压缩失败回退：本批原文并入记忆、继续参与组装，下一批压缩时移回「本批对话」重新提取，输出 Warn 日志 `window: compress failed, keeping raw messages`；会话切换时的提取失败后按 1 s / 2 s / 4 s 重试 3 次（期间 End、Start 等待），仍失败则不暂存，输出 Error 日志 `agent: session-end point extraction failed, points dropped`。要点逐条校验：字段类型不符与空文本的条目跳过、未知类别置空、不合法的 due 置零、超过 10 条截去 |
| 启用条件 | 只在配置了记忆服务（`agent.Config.Memory`）时提取要点；未配置时压缩只输出摘要 |

实现：`runtime/memory.Point`（Text / Kind / Speakers / Due）、`runtime/window` 的 `ProviderCompressor`（`CompressWithPoints` / `ExtractPoints`）、`DefaultPointsRules()`、`parseExtraction`。


### 未定

| # | 项 | 说明 |
|---|---|---|
| 1 | 需求 7 | 摄像头画面只在组装时追加、不进入历史，压缩批次中没有视觉内容 |
| 2 | 需求 8 | 承诺的 `due` 已随要点输出；到期触发需要 ARiA 侧的定时机制，未设计。Hindsight 侧承诺以 tags = commitment、timestamp = 到期时间写入，可按时间查询 |
| 3 | 自有记忆服务 | 第一期用 Hindsight 适配层测试流程（2026-10-02 主人决定），之后切换到自有服务；自有服务须提供上述 4 个接口与服务端合并（暂存、提交、用户背景、最近要点） |

需求 2 由要点提取的提示词规则执行，效果未经实测。
