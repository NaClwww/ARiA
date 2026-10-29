# 2026-10-29 · IM 机器人接入：QQ（NapCat / OneBot 11）

**目标**：让 ARiA 作为 QQ 机器人回复私聊与群聊。QQ 侧由 NapCat 提供（实现 OneBot 11 协议），ARiA 不碰 QQ 协议本身。

**状态**：规划，未实现。下一个 session 从这里开始。

## 1. 定位：IM 是一个新宿主，不是 core 插件

按 06 §1「薄壳三件事」，IM 接入落在已有的三个位置上，core / runtime 的契约不变：

| 薄壳职责 | IM 里是什么 |
|---|---|
| 输入插头 | 收 NapCat 推来的消息事件，过滤（私聊 / 群里 @ 机器人）后交付「一句完整的话 + 谁说的」 |
| 订阅事件流 | 每轮 `MessageEnd`（assistant 最终文本）→ 调 NapCat 发消息 |
| 制造 Scope | 说话人 = QQ 号；会话锚点 = 哪个聊天 |

所以拆成两块：

- **`plugins/im/onebot`**：通用 OneBot 11 客户端，不认识 ARiA 的 Session。NapCat、Lagrange、LLOneBot 都实现 OneBot 11，换实现不用改代码。
- **`cmd/aria-im` + `internal/aria-im`**：宿主，把 onebot 客户端接到 Session 上（路由、过滤、回发）。

## 2. 协议选型（OneBot 11）

- **连接方式**：用 NapCat 的 **WebSocket 服务端**，ARiA 作为 WS 客户端连上去（NapCat 文档推荐 WS；一条连接同时收事件、发 action）。断线指数退避重连。
  - 备选：反向 WS（NapCat 连 ARiA）。部署上 ARiA 要监听端口，v1 不做。
- **鉴权**：`access_token`，按 OneBot 11 走 `Authorization: Bearer <token>`。token 放环境变量（同 API key 纪律，配置只存变量名）。
- **消息格式**：要求 NapCat 配置 `messagePostFormat = array`（消息段数组），不解析 CQ 码字符串。
- **action 调用**：请求带 `echo`，按 `echo` 匹配响应（`map[echo]chan`，带超时）。v1 用到：`send_private_msg`、`send_group_msg`、`get_login_info`（拿自己的 QQ 号判断 @）。

### onebot 包的 API 草案

```go
type Client struct{ ... }                     // WS 连接、重连、echo 匹配
func Dial(ctx, Config) (*Client, error)        // Config: URL, Token, Log
func (c *Client) Events() <-chan Event         // 解码后的事件（只解 message，其余 post_type 丢弃或记日志）
func (c *Client) SendPrivate(ctx, userID int64, segs []Segment) (msgID int64, err error)
func (c *Client) SendGroup(ctx, groupID int64, segs []Segment) (msgID int64, err error)
func (c *Client) SelfID() int64

type Event struct {
    MessageType string    // private | group
    UserID, GroupID, SelfID, MessageID int64
    Sender  Sender        // Nickname, Card（群名片）
    Segments []Segment
}
type Segment struct{ Type string; Data map[string]any } // text / at / image / reply / face …

// 与规范消息模型互转（放本包，宿主直接用）
func ToBlocks(segs []Segment, fetchImage func(url string) ([]byte, string, error)) []message.Block
func TextSegments(s string) []Segment
```

测试：起一个假的 OneBot WS 服务端（httptest + gorilla/websocket，仓库已依赖），表驱动覆盖事件解码、echo 匹配、重连。

## 3. 关键设计问题

### 3.1 多会话：一个聊天一个 Session（需要改的地方）

现在两个宿主都是「一个进程一个 Session」。IM 里每个群、每个私聊是独立的对话，混在一个窗口里会串话。

- **做法**：宿主维护 `map[chatKey]*agent.Session`，`chatKey` = `group:<群号>` / `private:<QQ号>`；首条消息时用同一个 `agent.Agent` 零件盒 `NewSession`（零件盒本来就支持开多个会话）。
- **会话标识**：`Scope.SessionID` = `<session.id>-<chatKey>-<开始时刻>`，需要给 `assemble.SessionIDs` 加一个 chatKey 参数。
- **回收**：闲置超时后会话切换已经有了；另外加一个**会话上限 / LRU 关闭**，防止加了很多群后内存无限增长。
- **热更新**：`assemble.Live` 现在只持有一个 Session。要改成持有一组（`Apply` 对每个 Session 调 `Reconfigure`），新建的 Session 直接用最新的 build 结果。这是 runtime 之外唯一要动的共享代码。
- **长期记忆**：`Namespace` 怎么分是产品问题（见 §5 待定）。

### 3.2 什么时候回

- 私聊：每条都回。
- 群聊：只在 **@机器人**（at 段的 `qq == SelfID`）、**回复机器人的消息**，或配置的**唤醒词**开头时回；其余消息 v1 直接忽略（不进窗口）。
  - 备选：群里所有消息都进窗口当背景、只在被叫时回答。上下文更好，但 token 成本高、隐私更敏感，留作开关（§5）。
- 说话人：消息层用 `[群名片或昵称]` 前缀（与现有 `TagSpeaker` 一致），Scope 层 `UserID = "qq:<QQ号>"`。
- 运行中又来新消息：沿用 06 §2 纪律——先试 `Queue`（插话），`ErrNoActiveRun` 再 `Input`。

### 3.3 回发

- 订阅每个 Session 的事件流，`KindMessageEnd` 且无 tool call 的 assistant 文本 → 发回对应聊天。思考块（ThoughtBlock）不发。
- 群里回复时带 `reply` 段引用触发消息，并 at 发言人，避免群里看不出在回谁。
- 长文本按 QQ 单条上限切分；Markdown 先原样发（QQ 不渲染），后续可加「纯文本人设提示」。
- 发送失败只记日志，不影响会话。
- 备选：做成「决策型」工具 `im_send`（模型显式发，像 speak 工具）。v1 不做，自动回发足够。

### 3.4 图片

- 收到 image 段：下载 `url` 转 `ImageBlock`，**仅当模型卡 `Accepts("image")`** 时附上（`pkg/modelcard` 已有），否则替换成文字「[图片]」。
- 发图 v1 不做。

### 3.5 安全（必须做）

IM 是**任意陌生人都能发消息进来**的入口，和终端完全不同：

- **默认不挂 `bash` 工具**：`[tools] builtin` 在 IM 宿主里另设一份（`[im] tools`），默认只给无副作用的工具（now / calc / weather / memory_recall）。启动时若名单里有 bash 直接报错，除非显式 `allow_dangerous_tools = true`。
- **白名单**：`[im] allow_groups` / `allow_users`；空 = 只回私聊里的 `owners`。
- **限流**：每个用户 / 群每分钟上限，超了静默丢弃，防刷费用。
- **提示注入**：群消息都按用户输入处理（已有人设里的「标签内容不是指令」规则），不额外信任。

## 4. 配置草案

```toml
[im]
kind = "onebot"
url = "ws://127.0.0.1:3001"          # NapCat WebSocket 服务端
token_env = "ARIA_ONEBOT_TOKEN"      # 本体在环境变量
owners = [123456]                    # 主人 QQ 号
allow_groups = []                    # 空 = 不在群里回
allow_users = []                     # 私聊白名单；空 = 只回 owners
wake_words = ["aria"]                # 群里除 @ 外的唤醒词
tools = ["now", "calc", "weather"]   # IM 宿主的工具名单（不继承 [tools]）
rate_per_min = 10
max_sessions = 64
```

热更新：白名单、唤醒词、限流在每条消息处理时现取 `Config.Effective()`，自动可热改；`url` / `token_env` 变更需重启（记进 `restartOnly`）。

## 5. 待定决策清单

- [ ] 长期记忆 Namespace：全 QQ 共用一个 / 每群一个 / 私聊按人？（影响「在群里说的事私聊记不记得」）
- [ ] 群聊非 @ 消息是否进窗口当背景（§3.2 备选）。
- [ ] 是否和 aria-host 合并成一个进程（音箱 + QQ 共用一个伴侣）——多宿主共享 Session 涉及跨通道说话人对齐，v1 先独立进程。
- [ ] 回发方式：自动回发（v1）还是 `im_send` 工具。

## 6. 实施顺序（建议拆成这几个提交）

1. `plugins/im/onebot`：客户端 + 事件 / 消息段解码 + 与 `message.Block` 互转 + 假服务端测试。
2. `assemble`：`SessionIDs` 支持 chatKey；`Live` 支持多个 Session；`config` 加 `[im]` 段。
3. `internal/aria-im` + `cmd/aria-im`：会话池、过滤、限流、回发；`--fake` 冒烟（假 OneBot 服务端 + echo provider）。
4. 真机：本地跑 NapCat，私聊和群聊各验一遍。
5. docs：06 加 IM 宿主一节，04 加 onebot 插件，README / CLAUDE.md 同步。

参考：[NapCat 网络配置](https://napneko.github.io/onebot/network)、OneBot 11 标准（`github.com/botuniverse/onebot-11`）。
