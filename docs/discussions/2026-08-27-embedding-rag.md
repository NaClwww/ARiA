# 2026-08-27 · Embedding / RAG / 记忆检索专题讨论

> 起因：梳理正式文档时发现 embedding 在 5 处被引用（MemoryItem.Embedding、M5 向量召回、蒸馏去重、sqlite-vec）但**子系统本身从未设计**。本记录为讨论实录，全部内容**未合入**正式文档（01-04 未改动）。

---

## 1. embedding 模型原理与接入

**原理**：`文本 → tokenizer → transformer(decoder-only，与聊天模型同架构) → 池化(mean/末token) → 固定维度向量`。核心在训练而非架构：对比学习（InfoNCE、in-batch negatives、难负例），「正例对拉近、负例推远」，度量被烙进向量空间。

- **非对称前缀**：很多模型区分 query/document 身份（E5 的 `query:`/`passage:`、Qwen3-Embedding 任务指令），用错前缀召回质量明显下降 → **接口必须显式传角色，不能靠调用方拼字符串**；
- **Matryoshka (MRL)**：训练时前 k 维单独可用，1536 维可放心截到 256 维（OpenAI text-embedding-3、Qwen3-Embedding 支持）；
- **reranker ≠ embedding**：是 cross-encoder，(query, doc) 拼接输入直接出分；精度高但不能建索引，只用于召回后重排。

**接入两条路**：

| 路线 | 做法 | 特点 |
|---|---|---|
| API | OpenAI 兼容 `/v1/embeddings`（OpenAI/智谱/阿里/Jina 一套打尽） | 零部署、按 token 计费、100-300ms |
| 本地 | Ollama `/api/embed`（bge-m3 / Qwen3-Embedding / nomic-embed-text）、ONNX | 免费、毫秒级、数据不出机；Ollama 已在 provider 名单，等于白拿 |

## 2. Embedder / TokenCounter 契约草案（ARiA 缺口的补法）

```go
// plugins 层契约，runtime 中介
type Embedder interface {
    Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)
    // req: Texts []string（批量）、Role: Query|Document、Dims hint
    // res: 向量 + 模型名/维度（落库要存）
}
// 实现：ollama-embed、openai-compatible-embed

// pkg 小契约：本地只需「数 token」估计（精确值来自 API usage 闭环）
type TokenCounter interface{ Count(text string) int }
// 默认零依赖启发式（英文 chars/4，中文按字数系数）；可选 tiktoken 纯 Go 移植
// 不引 sentencepiece/HF Rust 绑定（CGO 违反无 CGO 决定）
```

四个决策点：

1. **模型/维度一致性**：换模型=维度变=旧向量作废。索引元数据记 `model+dims`，不一致触发重建索引后台任务（Batch class，幂等键沿用）；
2. **缓存**：`content_hash → vector`，同内容绝不重算（与 §10 的 hash 寻址同一个键，贯穿存储与计算两层）；
3. **⚠️ sqlite-vec 与纯 Go 不兼容**（本次讨论发现的正式文档隐患）：sqlite-vec 是 C 扩展，`modernc.org/sqlite`（纯 Go）**不能加载 C 扩展**。选项：(a) 暴力余弦扫（1万条×256维=毫秒级，推荐 M5 起步）(b) Go 原生 HNSW 库 (c) 换 CGO 的 mattn + 真 sqlite-vec（违反无 CGO，排除）。倾向 (a)→(b) 渐进，**04-plugins 中「三期 sqlite-vec」的表述需修改**；
4. **前缀纪律**：Query/Document 角色显式传。

## 3. tokenizer、长文本与 chunking

- tokenizer 与 embedding 是**流水线先后两站**（不是并列方案）；embedding = 整段进、一个向量出（池化）；
- 长文不能整 embed：① max context 限制 ② 池化平均化——一篇讲十主题的长文向量是「语义平均值」，什么都像什么都不像；
- **chunking 标准做法**：语义边界（markdown 标题→段落）→ 固定窗口 256~512 token → 10~20% overlap；每个 chunk 独立 embed 入库，命中返回 chunk；
- **ARiA 数据分两种粒度**：MemoryItem / Planner Unit 天然短（单条直接 embed，**不需要 chunk**）；只有 RAG 长文档导入需要 chunk（IngestSource 插件带切分策略）。chunk 向量进索引、原文整篇进 L3（Provenance 指回）。

## 4. 词法 RAG 路线（「用 tokenizer 做 RAG」）

- 工程真名：**词法检索**（BM25/FTS）——tokenize → 倒排索引 → BM25 打分。与向量检索是并行路线：词法强在精确符号（专名/错误码/函数名），向量强在改写/同义/跨语言；
- **中文大坑：SQLite FTS5 默认 unicode61 tokenizer 不分词**（连续汉字成一整个词项，「上限」查不到「预算上限」）。纯 Go 出路：**(a) FTS5 trigram tokenizer（三字滑窗，推荐 v1，零新依赖）** (b) Go 分词库预分词（多依赖多调参） (c) jieba CGO（违反无 CGO，排除）；
- 选项：**A = 全系统零 embedding**（RAG 用 trigram FTS；去重/相关性打分用词法相似度如 Jaccard）vs **B = 混合**（M3 词法起步、M5 按 Embedder 契约补向量）。倾向 **先 A 后按需演进 B**（契约保留、实现缓写）。

## 5. 「简单 attention 找相似记忆」→ LLM in-context attention 方案

- **自建 attention 是循环论证**：attention 的语义能力来自训练出的 Q/K 投影；无训练的 attention（词项向量直接 Q·K+softmax）数学上退化为加权词重叠 ≈ BM25 换皮；
- 统一图景：**词法检索 = 不用训练的 attention；embedding = 把语义压缩进向量再算；LLM 读上下文 = 免费、训练好的 attention**；
- 方案（零 embedding 路线的关键拼图）——**两级召回**：
  - 第 1 级：**常驻索引**——长期记忆一行一条放 facts 区（`[#tag] 一句话摘要`），LLM 自己的 attention 完成语义挑选（= Claude Code 的 MEMORY.md 机制）。规模边界：几十~两百条（200 条×20 token≈4k；再大有 lost-in-the-middle）；
  - 第 2 级：`memory.recall` 工具（FTS5 trigram）——索引外的、episodic 大库，模型自己决定查；
- 约束：API 不暴露内部 attention 权重 → 无法做显式「打分返回 top-k」服务，只能是隐式选择（模型读索引回答/调工具）。

## 6. tag 的处理（含多 tag）

**拆开两个目标**：(a) tag 参与语义计算（进向量）(b) tag 作为过滤/排序信号（不进向量）。工程正解是 (b)。

| 方案 | 评价 |
|---|---|
| 文本拼接进 embedding | 查询侧无 tag → 不对称，激活不了；池化稀释。主要作用是同 tag 文档聚堆 |
| 元数据过滤（SQL 列） | **工程正解**；Namespace/Kind 同款模式 |
| 双通道融合（RRF） | tag 精确命中做加权通道，工业标准 |
| 拼接加权向量 `[text‖tag]` | 索引不能用、调参烦，不推荐 |

- **零 embedding 路线下 tag 三吃**：`[#tag] 摘要` 写进索引行 → attention 可见 + FTS 高 IDF token + SQL 列可过滤。一个 tag 同时吃三种机制红利；
- **多 tag 让向量方案全恶化**：tag 向量平均=大杂烩稀释；每 tag 一条索引项=存储放大；前缀变长=池化进一步稀释。离散过滤却是天堂（AND `tags @>`、OR `tags &&`）；
- **tag-IDF**：高频 tag（#日常）判别力为零应降权，稀有 tag（#预算）命中重仓——BM25 的灵魂移植到 tag；
- **前提是受控词表**：蒸馏时从受控词表选（防「费用/开销/预算」漂移）；tag 归一化这种离线小任务可以单独用一个 embedding 跑，不违背在线零 embedding；
- 写入侧质量决定天花板：每条记忆 2~4 个高判别力 tag。

**tag 向量唯一赚钱场景**：tag↔tag 语义模糊匹配（查询「费用」命中标着「预算」的记录）。便宜实现：**受控词表一次性离线 embed 成查找表**（tag×tag 相似矩阵缓存），在线零推理——电商 query→类目路由同款。

## 7. 混合检索：tag 词法 + 内容 embedding 双通道

- **两种模式**：① 并行双通道+融合（tag 是加分项）② **漏斗式：tag 硬过滤 → 候选集内向量排序**（tag 是准入条件——记忆检索多数是这个语义；个人库规模向量暴力扫，prefilter 无性能问题）；
- **RRF**：`score(d)=Σ_通道 1/(k+rank_i(d))`，k=60。尺度免疫（BM25 无界分 vs cosine [-1,1]）、零调参、**加通道不改公式**（双词法→三通道平滑升级的关键）；
- **业界案例（已搜索验证）**：
  - Azure AI Search：[官方定量实验](https://techcommunity.microsoft.com/blog/azure-ai-foundry-blog/azure-ai-search-outperforming-vector-search-with-hybrid-retrieval-and-reranking/3929167) hybrid+rerank 胜纯向量，定位生产默认模式；
  - Elasticsearch：[RRF retriever](https://www.elastic.co/docs/reference/elasticsearch/rest-apis/retrievers/rrf-retriever) 官方一等 API（standard+kNN 子检索器合并）；OpenSearch 同款；
  - Qdrant/Weaviate/Milvus：hybrid query + RRF/WeightedRanker；
  - [Spice.ai](https://spice.ai/blog/real-time-hybrid-search-using-rrf)：SQL 内 keyword+vector+**metadata 三通道** RRF——与 ARiA 设想几乎逐点对应；
  - SQL 系：[MariaDB 官方 RRF 文档](https://mariadb.com/docs/server/reference/sql-structure/vectors/optimizing-hybrid-search-query-with-reciprocal-rank-fusion-rrf)、Postgres pgvector+BM25+RRF（Tiger Data/pgEdge）——SQLite 手写同构；
  - 生产案例：法律行业 RAG（BM25+向量+RRF 已上线，纠结 rerank 叠加）。

## 8. summarize-then-embed（小模型总结 → 摘要做 embedding）

- **名称**：summarize-then-embed / 粗到细检索（coarse-to-fine）；知名实例：**RAPTOR**（递归摘要树）、**Anthropic Contextual Retrieval**（chunk 前缀上下文化）、经典 IR 的 summary-based indexing；
- 好处：**对齐查询分布**（查询短、原文长，摘要把信息压到同粒度，cosine 变准）、去噪、省空间；
- 代价：选择性信息损失（摘要没写的检索不到；缓解=多视角摘要+原始词法通道兜底）、小模型幻觉（管线幂等可重跑）；
- **与 ARiA 的对位**：Distiller 产出 fact/summary + Provenance 链（摘要→episode→artifact）就是此模式——本讨论为其补上了向量通道的实现细节。

## 9. workflow 记忆（RAG 应用设计）

场景：给可重复运行的 workflow 积累经验记忆。

- **chunking 结论**：记忆主形态=蒸馏短条目，**不需要 chunk**；chunking 只是归档长原始产物（日志/转录）时的旁路；
- **四类条目**：`outcome`（输入类+参数→结果）、`pitfall`（失败教训+绕法）、`recipe`（成功配置模板）、`preference`（用户人工修正，**价值最高**）。结构化字段 `workflow_id/step/场景tag/时间/置信度` 是精确过滤键；
- **写入**：运行结束异步蒸馏（幂等）；人工修正即时同步写；同键新 outcome **supersede** 旧的；
- **检索点长在流程节点上**（与普通 RAG 最大不同）：workflow 启动（装载经验卡）、**每个决策/LLM 节点前**（当前输入摘要做语义查询+step 过滤——「和这单最像的历史」）、失败重试（查 pitfall）；
- **注入**：命中 3~5 条「经验卡片」放 context 区（几行，非大段），provenance 指回原始 run；
- **实现**：漏斗式双通道（结构化键 SQL 硬过滤→内容排序）；v1 零 embedding 最小版 = SQLite + FTS5 + 小模型查询改写；
- **两个特有坑**：① 流程改版→记忆集体过时（条目带 workflow 版本，变更时全量降置信/失效）② 失败实验污染（置信度从低起步，**被引用且复现成功才升级**）；
- 与 ARiA 关系：= `Scope.Namespace` 绑 workflow_id、Kind 换 workflow 域四类的应用实例，管线全部复用。

## 10. hash 寻址存储（免 chunking 的取回层）

用户提出「不做 chunking，条目做 hash、指向记录文件具体位置」——成立，且有名有姓：**content-addressable storage + bitcask 式日志结构索引**（git 对象模型、Riak bitcask 同款）。

```
记录文件（append-only，长度前缀）：[len][record]…        ← 只追加，永不原地改
内存哈希表：hash(content) → (file, offset, len)
检索路径：搜索索引(FTS/向量，建在摘要上) → 命中 ID=hash → 查表 → seek → O(1) 整条取回
```

- **四个白拿**：取回与记录长度无关（2KB/2MB 一样一次 seek——不 chunk 也能取长文的机制保证）；去重幂等（同内容同 hash；**hash 同时是 §2 的 embedding 缓存键**）；不可变+supersede 兼容（修正=追加新记录+改指向，= 事件溯源的物理形态）；append-only 崩溃安全；
- **三个坑**：① 内容变一个字=新 hash，旧记录变垃圾 → hash 是身份不是版本，配 `superseded_by` + 定期 compaction（bitcask merge）② **hash 只解决取回，不解决搜索**——搜索索引仍建在摘要上，两层职责分离 ③ 部分读取：整条读回再切（MB 级可行），别过度设计内部偏移；
- **ARiA 对位**：L3 artifact store 的存储引擎；或「SQLite 存结构化元数据 + hash→offset 文件存大内容对象」的分离方案。

## 11. 给 memory 加 embedding 检索（「快速检索」之辨）

- **核心澄清**：个人记忆规模（几百~几万条）下速度从来不是瓶颈（FTS5 / 暴力余弦都是毫秒级）；embedding 买的是**语义召回质量**——解决「换种说法就找不到」（记忆条目短、查询口语化、词面重叠低，正是词法通道的主要失败模式，例：存「月度上限 50 元」、问「这个月还能花多少」）。延迟账要诚实：查询侧必须先 embed 查询本身（API 100-300ms / 本地 Ollama 几 ms），严格说在线反而略慢，换的是召回率。
- **为什么 memory 子系统是 embedding 回报最高、成本最低的地方**：回报最高（短条目 + 口语查询，语义匹配收益最大）；成本最低（条目天然短，**完全避开 chunking 的全部麻烦**（§3）；写侧离线进行，在线零写成本）。
- **最小增量方案**（不动两级召回结构）：
  ```
  写侧（离线）：蒸馏/写入后台任务 → content-hash 缓存查重 → embed
               → 256 维（MRL 截断）float32 blob 存 SQLite 行内
  读侧（在线）：memory.recall 工具内部升级——
               词法 top-20 ∥ 向量 top-20 → RRF 融合 → top-5
               （工具签名、第 1 级常驻索引、两级召回结构全不变）
  搜索实现：   Go 原生循环暴力余弦（1 万条 × 256 维 < 1ms），不需要 sqlite-vec（衔接 D2）
  查询缓存：   查询向量同样走 hash 缓存，重复问法零成本
  ```
- **关键克制**：起步 **pull-only**——只在模型调 `memory.recall` 时才 embed 查询，不做每轮自动 recall（push，每轮多一次 embedding 调用）；golden 集评测（改写查询的召回对比：纯词法 vs 混合）证明收益后再开 push。
- **回退路径**：向量通道只是 recall 工具内多一路候选，评测不达预期摘掉即可（RRF 公式不动）。
- **结论**：D1 细化为「分子系统决策」——memory 侧先上向量（pull-only 起步），RAG 文档侧维持词法（A）。

## 12. 记忆浮现（surfacing）——记忆自动想起，而非 agent 主动寻找

> 2026-08-28 追加。认知科学对应：**扩散激活**（spreading activation）与**编码特异性原理**（Tulving：写入时的线索与提取时的线索匹配，记忆才被想起）。

**三级结构（成本递增，可只做前两层）**：

| 层 | 机制 | 成本 | 说明 |
|---|---|---|---|
| 层0 常驻索引 | facts 区一行一条，LLM attention 即浮现机制 | 零 | 已有设计；限制 ≤200 条 |
| 层1 线索触发 | 写入时蒸馏器生成**触发线索**（实体/关键词/场景 tag）；读取时新输入词项命中线索 → 提名 | 零 embedding、零每轮成本 | 编码特异性的工程化：写入侧线索质量 ≈ 读取侧匹配质量；推荐的首个增量 |
| 层2 语义浮现 | 当前输入 embed，与记忆向量相似度超阈值 → 提名 | 每轮一次 embedding | 噪声风险最高，须过评测 |

**三个工程决策**：

1. **浮现 ≠ 注入**：浮现层只「提名」，记忆作为 retrieved units 进 Planner 打分背包竞争（relevance × importance × referenceBoost），弱提名自然被挤出——Window 机制天然吸收一层噪声；
2. **噪声三件套**：阈值 + 冷却/疲劳（同一记忆不每轮浮现，已注入的带冷却期）+ 每轮 top-k ≤3~5。**假阳性浮现比漏掉更糟**（不相关记忆污染上下文直接扭曲回答）；层 2 必须过 golden 集评测（「第 N 轮相关记忆自动出现」+ 假阳性率）才启用；
3. **层1/层2 不互斥**：线索=明确相关、语义=暗关联，提名合并进同一竞争。

**业界佐证**：ChatGPT Memory = 每轮浮现（产品可行性与噪声问题的双重证明）；斯坦福 Generative Agents 的记忆检索公式 `α·recency + β·importance + γ·relevance` 与 Planner 打分逐项同构（独立验证该评分形状适用于「什么该浮现」）。

**结论**：§5 两级召回升级为**三级**——常驻索引（层0）+ 浮现提名（层1/2）+ recall 工具（pull 兜底）。层 1 与 §11 pull-only 起步不冲突（同样零每轮成本）。落地顺序：层0（M3 自带）→ 层1 → 层2（评测门槛后）→ 新增 **D10**。

---

## 待定决策清单（合入正式文档前需拍板）

| # | 决策 | 倾向 | 影响文档 |
|---|---|---|---|
| D1 | v1 检索路线：A 零 embedding vs B 直接混合 | **分子系统决策（§11 细化）**：memory 侧先上向量（pull-only 起步，写侧离线 embed + RRF 融合）；RAG 文档侧维持 A（trigram FTS） | 03 §11-12、04 §5 |
| D2 | sqlite-vec 表述修正：改「暴力扫(256维)→HNSW 渐进」 | 是（sqlite-vec 与纯 Go 不兼容） | 04 §5.2 |
| D3 | FTS5 中文 tokenizer：trigram（v1） | 是 | 04 §5.2 |
| D4 | Embedder/TokenCounter 契约写入 | 草案在 §2 | 01-pkg、04 |
| D5 | 常驻索引 + memory.recall 两级召回（§5）合入记忆编排 | 是 | 03 §12 |
| D6 | 双通道+RRF 漏斗式检索（§7）作为 Recall 实现形态 | 是 | 04 §5.2 |
| D7 | tag 受控词表 + tag-IDF + 2~4 个/条 | 是 | 03 §12、04 §5 |
| D8 | hash 寻址存储作为 L3 artifact 引擎 | 待议（与 SQLite 分工） | 03 §11、04 |
| D9 | workflow 记忆作为独立原型先行验证 | 待议 | 新记录 |
| D10 | 记忆浮现三级结构（层0 常驻索引 / 层1 线索触发 / 层2 语义浮现），提名统一进 Planner 竞争 | 层0 随 M3；层1 优先（零 embedding）；层2 过 golden 评测才开 | 03 §11-12 |

## 已合入

（无——本次全部为讨论态，正式文档 01-04 未改动）
