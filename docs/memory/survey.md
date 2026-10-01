# 长期记忆：现有方案调研（2026-10-01）

检索方式：Stars、版本与提交时间取自 GitHub REST API；代码事实取自各仓库默认分支；文档页面于 2026-10-01 读取。评测分数除另行注明外均为厂商自测，可比性见 §4。标「未核实」的条目为调研时未能从一手来源确认。

## 1. 结论摘要

- §2 所列引擎均未公布自托管 CPU 环境下可复现的检索延迟；已公布的数值均为厂商或云端测量。
- 多数引擎的原始写入接口不接受客户端幂等键；例外为 Hindsight 异步 retain 的 `operation_id`、Supermemory 的 `customId`（按 ID 更新）、ReMe 的 `session_id`（每会话每日一张卡片）。
- 提供官方 Go 客户端的只有 Hindsight（OpenAPI 生成）与 Memobase（2026-01-11 起无提交）；Zep 的 `zep-go` 只支持 Zep Cloud。
- 全部公开评测为英文，未见普通话或家庭多说话人的长期记忆评测；LoCoMo 答案键经第三方审计有 6.4% 错误。
- 多方对话评测 GroupMemBench 中，BM25 基线与多数记忆系统持平或更高。
- 家庭语音助手（Alexa+、Gemini for Home、小爱、小度、天猫精灵）均未公开记忆与声纹的绑定方式及未识别说话人的记忆处理（§7）。

## 2. 记忆引擎

| 系统 | 许可证与活跃度 | 部署依赖 | Go 客户端 | 中文 | 写路 | 读路 |
|---|---|---|---|---|---|---|
| Hindsight | MIT；v0.10.2（2026-09-29），每周发布 | PostgreSQL + pgvector（单镜像内嵌 pg0，或外置 PG）；内存最低 4 GB，建议 8 GB | 官方，OpenAPI 生成；客户端 README 标注的 API 版本（0.4.11）低于服务端（0.10.2） | 缺省 embedding 与 reranker 仅英文，可配 bge-m3、bge-reranker-v2-m3；中文 BM25 需 pgroonga 或 pg_search，两者均要求外置 PG；#4691 繁简同名拆成两个实体，关联 PR 未合并 | LLM 提取事实、实体、关系与时间；异步 observation 合并再次调用 LLM，次数未公开；异步 retain 支持 `operation_id` 去重 | 语义、BM25、图、时间四路 + RRF(k=60) + cross-encoder；读路不调用 LLM；FAQ：不重排 50–100 ms，重排 200–500 ms，硬件未注明 |
| MemOS | Apache-2.0；v2.0.34（2026-09-23） | FastAPI + Neo4j + Qdrant；偏好记忆开启时需 Milvus | 无，有 OpenAPI | 中文提示词、jieba、缺省 qwen 系列 | fast 模式不调用 LLM；fine 模式每窗口 1 次 LLM，另有可选附加调用；条目带归档版本链 | fast 模式：jieba + 向量 + 图 + BM25 + bge 重排，不调用 LLM；论文自测云端检索均值 440.5 ms、P99 777.1 ms（10 QPS，硬件未注明） |
| EverOS（原 EverMemOS） | Apache-2.0；v1.4.1（2026-09-24） | 单 Python 进程；Markdown 为原始数据，SQLite + LanceDB 为索引 | 无，有 OpenAPI | jieba；#378 中文输入生成英文记忆，未关闭 | LLM 判定话题边界，每段 1 次提取；异步引擎再提取事实与画像；无幂等字段 | BM25、向量、RRF 混合、agentic 四种；延迟未公布；消息带 `sender_id`、`sender_name` |
| Graphiti | Apache-2.0；v0.30.2（2026-09-08）；Zep CE 已于 2025-04-02 停止维护 | Neo4j 或 FalkorDB | 无 | 提示词仅英文（#1141）；MinHash 去重剔除非 ASCII 字符（PR #1357 未合并） | 每段多次 LLM 调用，次数未公开；双时间轴 `valid_at`／`invalid_at`；#1728 报告失效判定误判 | 向量 + BM25 + 图遍历 + RRF；读路不调用 LLM |
| Mem0（开源） | Apache-2.0；v2.2.1（2026-09-25） | 库缺省 Qdrant + SQLite；服务端 FastAPI + pgvector | 无官方 | BM25 与实体提取固定使用 spaCy 英文模型，中文时不生效（#4884 未关闭） | 每次 1 次 LLM，只做 ADD，MD5 精确去重，无幂等键；图记忆已移出开源版 | 语义 + BM25 + 实体融合，重排可选 |
| Supermemory | 仓库 MIT；引擎只以二进制发布，源码不在仓库 | 单二进制，无需数据库 | 无，有 OpenAPI | 缺省本地 embedding 仅英文，可选 bge-m3 | `customId` 按 ID 更新 | 自托管延迟未核实 |
| Memobase | Apache-2.0；2026-01-11 起无提交 | FastAPI + pgvector + Redis | 官方 Go SDK | 有 zh 提示词 | 缓冲达 1024 token 或 1 h 触发，每次固定 3 次 LLM；画像 + 事件时间线；消息可带 `alias`（说话人名） | 画像读取 <100 ms，带检索 500–1000 ms（README） |
| Cognee | Apache-2.0；v1.6.2（2026-09-29） | SQLite + LanceDB + Kuzu | 无 | 词法检索按 `\w+` 切分，无中文分词 | 分块后 LLM 抽取实体与关系 | 仅检索类查询不调用 LLM |
| ReMe | Apache-2.0；v0.4.1.12（2026-09-14） | 本地 Markdown 工作区 | 无 | 可选 jieba | ReAct agent 每会话每日写 1 张卡片 | BM25 + 可选向量 + RRF；无按用户隔离 |
| MIRIX | Apache-2.0；最后提交 2026-08-20 | pgvector + Redis | 无 | BM25 使用 english tsvector，不切分中文 | 6 个记忆 agent，每次多次 LLM | 带话题提取的检索接口调用 LLM |
| Memori | Apache-2.0；v3.3.6 | SDK + 自有数据库 | 无 | 词法分词 `[a-z0-9]+`，中文无词项 | 提取请求发往托管服务 api.memorilabs.ai，对话数据离开本机 | 本地 FAISS + 词法 |
| memU | Apache-2.0；v2 改为 agent 编辑的 Markdown wiki | 无 REST 服务 | 无 | 依赖 embedding 模型 | 由宿主 agent 写入 | 仅向量 |
| Letta | letta-ai/letta 已于 2026-08-16 归档，后继为 letta-code（TypeScript） | MemFS：git 管理的 Markdown 文件 | 无 | 未核实 | agent 通过工具编辑记忆文件 | `system/` 下文件每轮注入，其余按需读取 |
| MemoryOS（BAI-LAB） | Apache-2.0；低活跃 | 库 + MCP；依赖 faiss-gpu | 无 | 提示词无中文 | 短期 → 中期摘要 → 画像，LLM 次数未核实 | 仅向量 |
| TeleMem | Apache-2.0；v1.10.0（2026-08-15） | 本地 Qwen + FAISS | 无 | 按角色分离记忆画像；自测中文多角色评测 ZH-4O 86.33% | Mem0 兼容 API | 未核实 |
| MemoryBear | Apache-2.0；v0.4.7（2026-09-24） | PG + Neo4j + Redis + Elasticsearch + Celery | 无 | 有 README_CN | 未核实 | 未核实 |

来源：

- Hindsight：https://github.com/vectorize-io/hindsight ；https://hindsight.vectorize.io/developer/multilingual ；https://hindsight.vectorize.io/faq ；https://hindsight.vectorize.io/developer/retrieval
- MemOS：https://github.com/MemTensor/MemOS ；https://arxiv.org/abs/2507.03724
- EverOS：https://github.com/EverMind-AI/EverOS
- Graphiti：https://github.com/getzep/graphiti ；https://arxiv.org/abs/2501.13956 ；https://blog.getzep.com/announcing-a-new-direction-for-zeps-open-source-strategy/
- Mem0：https://github.com/mem0ai/mem0 ；https://docs.mem0.ai/migration/oss-v2-to-v3
- Supermemory：https://github.com/supermemoryai/supermemory
- Memobase：https://github.com/memodb-io/memobase
- Cognee：https://github.com/topoteretes/cognee
- ReMe：https://github.com/agentscope-ai/ReMe
- MIRIX：https://github.com/Mirix-AI/MIRIX
- Memori：https://github.com/MemoriLabs/Memori
- memU：https://github.com/NevaMind-AI/memU
- Letta：https://github.com/letta-ai/letta ；https://docs.letta.com/concepts/memfs
- MemoryOS：https://github.com/BAI-LAB/MemoryOS
- TeleMem：https://github.com/TeleAI-UAGI/telemem
- MemoryBear：https://github.com/SuanmoSuanyangTechnology/MemoryBear

## 3. 产品与模式

| 对象 | 机制 | 来源 |
|---|---|---|
| ChatGPT | saved memories 与 reference chat history 两个开关。逆向分析（非官方）：不对历史对话做 RAG，向上下文注入长期事实与约 15 条近期对话摘要。2026-06 的 Dreaming 在后台合成记忆状态并改写时间敏感事实（官方页面未能访问，依据二手来源） | https://manthanguptaa.in/posts/chatgpt_memory/ ；https://openai.com/index/chatgpt-memory-dreaming/ |
| Claude API memory tool | 模型请求 `/memories` 目录下的文件操作，由客户端执行；模型自行浏览文件，无 embedding 检索；Go SDK 无辅助实现 | https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool |
| Claude 消费端 | `<userMemories>` 块常驻注入，历史对话经 `conversation_search` 等工具按需检索（逆向分析） | https://manthanguptaa.in/posts/claude_memory/ |
| Gemini | 机制未公开 | https://support.google.com/gemini/answer/16598469 |
| Alexa+ | 记住用户陈述的事实；Voice ID 按 profile 建立声学模型；记忆是否按声纹分人未核实 | https://www.aboutamazon.com/news/devices/new-alexa-top-features |
| Google Home | Voice Match 最多 6 个 profile；未见按人记忆的公开设计 | https://support.google.com/googlehome/answer/7320960 |
| LangMem | semantic（collection／profile）、episodic、procedural 三类；写入分热路径（agent 工具，增加该轮延迟）与后台（对话结束后处理） | https://langchain-ai.github.io/langmem/concepts/conceptual_guide/ |
| Generative Agents | 检索打分 = recency + importance + relevance | https://arxiv.org/abs/2304.03442 |
| A-MEM | 笔记带关键词与标签，新笔记触发 LLM 生成链接 | https://arxiv.org/abs/2502.12110 |
| MemGPT | 主上下文与外部存储分页，经函数调用换入换出 | https://arxiv.org/abs/2310.08560 |

## 4. 评测与可比性

- LoCoMo：Penfield 审计 1540 题中 99 题答案键错误（6.4%），上限约 93.6%；gpt-4o-mini 评判接受 62.81% 的故意错误答案。https://penfieldlabs.substack.com/p/we-audited-locomo-64-of-the-answer
- 同条件复测（MemTensor OmniMemEval，由竞争方执行；回答模型 gpt-4.1-mini，评判 gpt-4o-mini）：

| 系统 | LoCoMo | LongMemEval |
|---|---|---|
| MemOS | 88.83 | 89.20 |
| Cognee | 83.48 | 51.80 |
| EverOS | 82.75 | 80.40 |
| Hindsight | 81.99 | 72.20 |
| Mem0 | 77.68 | 56.00 |
| Letta | 77.12 | 77.67 |
| Supermemory | 73.53 | 66.07 |
| Zep/Graphiti | 63.83 | 79.80 |

  来源：https://github.com/MemTensor/OmniMemEval/blob/main/docs/user_memory/results.md
- 厂商自报（LoCoMo／LongMemEval-S）：Hindsight 92.0／94.6，Mem0 92.5／94.4（Platform，非开源版），Zep 94.7／90.2（Zep Cloud），EverOS 94.42（多轮检索）／94.00。自报值与上表复测值的差距说明分数受评测框架影响显著。
- 争议：Zep 与 Mem0 的 LoCoMo 设置争议；Letta 以纯文件系统 agent 取得 LoCoMo 74.0；Supermemory 以 8 个提示变体任一正确计分；Maximem 复测 Mem0 得 57.5 与 73.8。https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/ ；https://www.letta.com/blog/benchmarking-ai-agent-memory
- 中文数据集：DuLeMon（https://arxiv.org/abs/2203.05797 ）、PerLTQA（摘要未说明语言，是否为中文未核实；https://arxiv.org/abs/2402.16288 ）、MADial-Bench（双语，https://arxiv.org/abs/2409.15240 ）；TeleMem 自测的 ZH-4O 为中文多角色长对话评测。

## 5. 多说话人与共享记忆研究（2026）

| 研究 | 结论 | 来源 |
|---|---|---|
| GroupMemBench | 多方对话中按说话人追踪信念；最优记忆系统 46.0%（知识更新 27.1%）；BM25 基线与多数记忆系统持平或更高 | https://arxiv.org/abs/2605.14498 |
| EverMemBench | 多方多群对话；即使给定标准证据，多方归属下多跳准确率为 26% | https://arxiv.org/abs/2602.01313 |
| GateMem | 多主体共享记忆，含家庭场景；无方法同时满足效用、访问控制与遗忘；检索类与外部记忆方法存在跨主体信息泄漏 | https://arxiv.org/abs/2606.18829 |
| AIM + MUMBench | 每条记忆判定为私有或公开；可见性分类 96.0% | https://arxiv.org/abs/2609.12320 |
| SpeakerMem-R1 | 说话人标注原文 + 派生状态双轨记忆，以强化学习训练 | https://arxiv.org/abs/2609.26780 |

## 6. Go 生态

| 类别 | 项目 | 事实 |
|---|---|---|
| 记忆服务 SDK | Hindsight | 官方，monorepo 内 `hindsight-clients/go`，OpenAPI 生成 |
| | Zep | `getzep/zep-go` v3.30.0，只支持 Zep Cloud |
| | Memobase | 官方 `memobase-go`，项目 2026-01-11 起无提交 |
| | Mem0 | 无官方 Go SDK |
| 框架 | langchaingo | memory 包只有短期对话历史 |
| | Eino（CloudWeGo） | 无长期记忆组件 |
| | Genkit Go | 无记忆模块 |
| 纯 Go 检索 | ncruces/go-sqlite3 | 无 CGO（Wasm 转 Go）；FTS5 可注册自定义分词器；附带 vec1 向量扩展（未到 1.0） |
| | bleve | CJK bigram 分析器为纯 Go；向量检索依赖 FAISS，不满足无 CGO |
| | chromem-go | 精确余弦，10 万条单次查询约 40 ms |
| | coder/hnsw | 纯 Go HNSW，无发布版本 |
| 中文分词 | gse | 纯 Go，v1.0.2 |
| | gojieba | 依赖 CGO |

## 7. 伴侣与家庭语音助手

来源标记：〔文档〕官方页面已读取；〔文档摘录〕官方页面，直接读取失败，内容取自搜索摘录；〔媒体〕新闻报道；〔第三方〕第三方博客或指南；〔未核实〕未能确认。

### 7.1 国内助手

| 产品 | 已公开的记忆功能 | 来源 |
|---|---|---|
| 豆包 | 2025-10 前后上线「显式记忆」测试版；据报道范围不含图片与文件，可开关、查看、删除〔媒体〕；缺省关闭、只提取姓名与职业等结构化事实〔未核实〕；未见音箱或按声纹记忆的说明 | https://www.aibase.com/zh/news/21891 |
| Kimi | 独立训练的模型判定记忆内容；不存健康信息、密码、地址（用户明确要求时除外）；支持「记住…」「更正：」「忘掉…」指令；上限 50 条、每条 500 字；不用于训练，可关闭或清空〔文档〕 | https://www.kimi.com/en/help/features/memory-space |
| 通义千问 | 2025-10-16 发布 Qwen Chat Memory，记录偏好、兴趣与过往交互，可查看、编辑、删除〔媒体〕；「记住…」写入用户记忆表并注入 system prompt〔未核实〕 | https://toolnavs.com/en/article/469-tongyi-qianwen-launches-quotqwen-chat-memoryquot-to-support-long-term-personalized-conversation-experience ；https://www.woshipm.com/operate/6281298.html |
| DeepSeek 应用 | 未见官方用户记忆功能〔第三方〕；2026-01 的 Engram 为模型架构内的条件记忆，不属于用户记忆 | https://blog.memoryplugin.com/does-deepseek-have-memory/ ；https://news.cgtn.com/news/2026-01-13/DeepSeek-unveils-new-AI-architecture-to-slash-memory-requirements-1JUjzzdmDf2/p.html |
| 小米小爱同学 | HyperOS 3「小爱记忆」（2026-04-20）以三指滑动记住屏幕内容〔媒体〕；未见音箱对话记忆或按声纹记忆的公开说明；社区项目 mi-gpt 在小爱音箱上实现长短期记忆，不区分说话人，已于 2026-04-04 归档 | https://www.ithome.com/0/941/292.htm ；https://github.com/idootop/mi-gpt |
| 华为小艺 | 「小艺记忆」（应用 11.5.2.300 起改名「小艺帮记」）存重要日期、证件与卡片、地址、事件与订单、人物信息与偏好、计划与目标；对话、图片、屏幕识别、应用同步四种添加方式；事件记忆上限 1000 条，其余类型各 100 条〔文档摘录〕；未说明多用户或声纹绑定 | https://consumer.huawei.com/cn/support/content/zh-cn16010214/ |
| 天猫精灵 | 宣传「持续学习家庭成员习惯」，机制与用户控制未见公开〔未核实〕 | https://post.smzdm.com/p/axkvnlzw/ |
| 小度 | 2025-11-13 发布「超能小度」，提及「个性记忆」〔媒体〕；已公开的具体功能为语音备忘与摄像头 24 小时回看找物；是否按家庭成员区分未核实 | https://finance.sina.com.cn/roll/2025-11-14/doc-infxkhuy2990814.shtml ；https://www.leikeji.com/article/72862 |

### 7.2 伴侣应用

| 产品 | 机制 | 来源 |
|---|---|---|
| Character.AI | Chat Memories 固定信息上限 400 字符，全体用户可用；Pinned memories 与自动记忆限订阅用户〔文档〕；2026-05 的三层结构：用户编写的 Story Memory、对话中自动捕获的 Facts、Memory Usage 占用显示，每个对话最多 15 个 Pin〔第三方〕 | https://blog.character.ai/helping-characters-remember-what-matters-most/ ；https://www.roborhythms.com/character-ai-adds-chat-memories/ |
| Replika | 分层记忆：Memory 标签页可见条目 + 基于对话模式的深层记忆；用户对正确回忆点赞以强化；可手动添加〔文档摘录〕 | https://help.replika.com/hc/en-us/articles/37208679176077-How-does-Replika-s-memory-work |
| Nomi | 短期、中期、长期记忆之上设 Identity Core，由角色自行判定身份核心内容；Mind Map 2.0 展示记忆中人物、地点、话题的关联〔文档摘录〕 | https://nomi.ai/updates/introducing-the-nomi-identity-core-fostering-dynamic-and-authentic-identities/ ；https://nomi.ai/updates/mind-map-2-0-bringing-nomi-memory-into-view/ |
| Kindroid | Journal 条目按关键短语触发（不区分大小写），上限 500 条，每条消息最多召回个人 3 条 + 全局 3 条；Cascaded Memory 为覆盖数百至数千条消息的中期层〔文档摘录〕 | https://kindroid.ai/docs/article/memory/ |
| MiniMax 星野／Talkie | 应用记忆架构未公开 | https://ai-bot.cn/minimax-m2-her/ |
| Pi | 登录用户跨平台记住 100 轮（2023-05 报道，数值取自搜索摘录）〔媒体〕 | https://gwern.net/doc/www/www.forbes.com/638e8c03d683fe876080f1f7d664ad4387b6bd55.html |

### 7.3 家庭多用户语音助手

| 产品 | 说话人识别 | 记忆与未识别说话人 | 来源 |
|---|---|---|---|
| Alexa+ | Voice ID 按说话人给出个性化响应；Echo Show 的 Visual ID 识别在场成员并投递针对该成员的提醒〔文档〕；家庭最多 2 名成人与 4 名儿童 profile，各有 Voice ID〔文档摘录〕 | 可记住家庭菜谱、重要日期、事实、饮食偏好，控制项位于 Alexa Privacy 面板〔文档〕；记忆按声纹 profile 还是按家庭存储，官方页面与 CNN 2026-01-13 报道均未说明；2019 开发者文档：声音未识别或用户退出时不下发 personId，技能改用账户级 userId〔文档〕 | https://www.aboutamazon.com/news/devices/new-alexa-generative-artificial-intelligence ；https://www.aboutamazon.com/news/devices/new-alexa-plus-amazon-devices ；https://developer.amazon.com/en-US/blogs/alexa/alexa-skills-kit/2019/10/recognize-voices-and-personalize-your-skills ；https://ktvz.com/money/cnn-business-consumer/2026/01/13/amazons-big-plan-to-beat-chatgpt-give-alexa-a-better-memory/ |
| Gemini for Home | Voice Match 个性化日历、通勤、音乐〔文档〕 | 访客与未匹配声音使用第一位 Voice Match 用户的缺省音乐服务；回答过滤缺省为「对访客过滤」，已匹配的儿童始终过滤；管理员可把视频历史检索限定为已匹配用户；支持「记住…」式偏好〔文档〕；Gemini for Home 不读取用户此前对 Google Assistant 所说内容〔媒体〕 | https://support.google.com/googlehome/answer/7320960 ；https://support.google.com/googlehome/answer/16618650 ；https://www.thurrott.com/?p=328971 |
| Apple HomePod／Siri | HomePod 最多识别 6 人，Personal Requests 需声音识别〔文档〕 | 未识别者可使用音乐、计时、天气，音乐经主用户账户播放且不改变主用户偏好；2026-06 的 Siri AI 经设备端 Spotlight 索引取个人上下文，未提及 HomePod 与多用户，中国区不提供〔文档〕 | https://support.apple.com/guide/homepod/apd1841a8f81/homepod ；https://www.apple.com/newsroom/2026/06/apple-introduces-siri-ai-a-profoundly-more-capable-and-personal-assistant/ |

### 7.4 研究

| 主题 | 研究 | 结论 | 来源 |
|---|---|---|---|
| 伴侣长期记忆 | MemoryBank／SiliconFriend（AAAI 2024） | 按经过时间与重要度更新记忆；保持率模型 R = e^(−t/S)（公式取自搜索摘录） | https://arxiv.org/abs/2305.10250 |
| | RMM（ACL 2025） | 前瞻式多粒度摘要 + 强化学习调整检索，LongMemEval 提升 10% 以上 | https://arxiv.org/abs/2503.08026 |
| | COMEDY（2024） | 单一模型完成记忆生成、压缩与使用；附中文数据 Dolphin | https://arxiv.org/abs/2402.11975 |
| 多方归属 | SocialMemBench（2026-05） | 成员离开后的归属、群体规范与个人例外；开源记忆框架得分 0.12–0.18 | https://arxiv.org/abs/2605.17789 |
| | GroupMemBench（2026-05） | 记忆写入后说话人结构丢失 | https://arxiv.org/abs/2605.14498 |
| 人设一致性 | Persona drift（2024） | 8 轮内出现显著漂移 | https://arxiv.org/abs/2402.10962 |
| | PersonaMem（2025） | 前沿模型准确率约 50% | https://arxiv.org/abs/2504.14225 |
| 遗忘 | FadeMem（2026） | 按相关性、访问频率与时间调整指数衰减，存储减少 45% | https://arxiv.org/abs/2601.18642 |
| | MOOM（2025） | 竞争-抑制式遗忘限制记忆增长；提出中文角色扮演数据 ZH-4O（平均 600 轮） | https://arxiv.org/abs/2509.11860 |
| 共享记忆隐私 | CIMemories（ICLR 2026） | 模型在错误语境中透露已记住的属性，属性级违规最高 69% | https://arxiv.org/abs/2511.14937 |
| | Collaborative Memory（ICML 2025） | 私有与共享两层，来源记录不可变，读取时按当前权限判定 | https://arxiv.org/abs/2505.18279 |
| | Famigo（ICEIC 2026） | 家庭语音助手，私有与共享两个存储，自动识别用户，设备端语音处理 + 云端 RAG，延迟 2–3 s〔文档摘录〕 | https://scholarx.skku.edu/item/284001d9-5901-48f3-aa81-8496d316ef6d |
| | MemOS 文档 | 每人一个 user_id；多人同一对话时 user_id 可为列表，按参与者分别记忆〔文档摘录〕 | https://memos-docs.openmem.net/memos_cloud/introduction/isolation_filters/ ；https://memos-docs.openmem.net/usecase/home_assistant/ |

### 7.5 中文数据集补充

- Dolphin（COMEDY）、ZH-4O（MOOM）、ChMapData（CoNLL 2025，https://arxiv.org/abs/2503.05150 ）、PAL-Set／PAL-Bench（AAAI 2026，https://arxiv.org/abs/2511.13410 ）、CharacterEval（1785 段对话、77 个角色，https://arxiv.org/abs/2401.01275 ）。
- 以中英文检索，未找到家庭多说话人记忆或普通话口语记忆的中文评测。

### 7.6 空白

- Alexa+、Gemini for Home、小爱、小度、天猫精灵均未公开记忆如何绑定声纹，以及未识别说话人的记忆如何处理。
- 已公开的访客行为只有两类：使用主账户的音乐设置，或给出过滤后的回答。
