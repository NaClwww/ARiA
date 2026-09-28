# 06 · 宿主薄壳与配置（2026-09-14 拍板）

> 定位：引擎可嵌入，**宿主拥有全部 IO 通道**；本文定薄壳形态、输入插头契约、配置分层与操作面板。
> 状态：配置分层已实现（`internal/config`，viper v1.21）；`cmd/aria-demo` 薄壳已跑通；`cmd/aria-web` 待做（见 §6）。

## 1. 薄壳的三件事

薄壳 = 引擎之上的宿主程序，只做三件事，引擎对通道类型零感知：

1. **制造 Scope**：谁在说话（`ctxx.Scope.UserID`）、会话锚点（`SessionID`）；
2. **订阅事件流**：渲染 / TTS / 落盘 / 表情动作，都是事件消费者；
3. **输入插头**：把各路原始输入加工成「一句完整的话 + 谁说的」，喂进 `Session.Input`。

产品入口（CLI / HTTP / 桌面）不与里程碑绑定，任何时候可作为新薄壳加入——现役两个：`cmd/aria-demo`（终端）、`cmd/aria-web`（待做）。

## 2. 输入插头（2026-09-14 定稿）

引擎的输入口**只有一个**：`Session.Input(ctx, msg)`。所谓「输入源」是往这个口喂话的插头，插座统一、插头可换可并存。

**插头契约**：交付「**一句说完整的话 + 谁说的**」。判断「什么时候算说完」由插头自理：

| 插头 | 说完判定 | 状态 |
|---|---|---|
| 终端手打 | 按下回车 | ✅ 已在 `cmd/aria-demo` |
| ASR（SSE） | 对方静音 ~1s（**服务端已做好端点检测**，只收 `type=final`；final 另带 `speaker_id`/`speaker_status` 认主——matched 用之，未匹配/旧版缺字段回落默认说话人） | ✅ `plugins/voice/gowild`（partial 另作「mic 听到人声」信号交付宿主，驱状态灯） |
| 网页聊天框 | 点发送 | 随 `cmd/aria-web` |
| 定时/传感器 | 到点/触发 | 将来 |

纪律与语义：

- **多插头可并存**：谁先来谁先进；一 Session 同时只跑一轮。两段式用法：**起一轮用 `Input`**（阻塞到本轮结算），**往正在跑的这一轮里插话用 `Queue`**（steering，非阻塞）。
- **`Queue` 空闲时返回 `ErrNoActiveRun`**（2026-09-14 审查；2026-09-28 修正判据）：Queue 的语义是轮间注入；空闲入队会被**下一次** Run 在它的输入之后排空，历史变成「后说的在前」（与本节「谁先来谁先进」相悖，且错误顺序随记忆长期留着）。所以宿主在多插头场景下应当：试 `Queue`，拿到该错误就改用 `Input`。判据在 core：入队口与「结束运行」的收敛判定（自然收敛/Interrupt 收敛）在同一临界区互斥，Run 一返回即拒绝——不再用「会话锁是否空闲」推断（旧判据在模型循环结束后的结算等待期误收，消息滞留且排到下一轮输入之后）。
  - *残留限制*：本轮以**错误/取消**收场时，收场前瞬间入队的消息仍可能在下一轮的输入之后排空（入队与失败天然竞态，消息不丢、仅顺序后移）；此类滞留积压由下一次 Run 在起点排空。彻底消除需在 Run 入口把积压并入 InitialInput（顺序语义变更），随调度设计（05 G1）一起定；v1 记录在案。
- **排队 ≠ 抢话**：v1 的「边听边打字」是排队（等当前轮答完再进）。**打断**（AI 说一半被拦下、半截回答以 Interrupted 留历史 + 小轮垫话）是另一条路（03 §7 小轮），**暂缓**；
- **感知不是插头**：照片/视频帧不是「会说话的源」，而是**随话语进同一条消息的多模态块**（03 §5 分层：「当前感知」位）。一轮对话 = 一条消息 = 文字块 + 图片块并存，引擎天然支持（消息模型多块、provider 转 `text` + `image_url`、落盘存 base64 均已验证）；
- **输入必须带主人**（03 §6）：说话人落两处——消息层（speaker 标签进历史，LLM 知道谁在说）+ Scope 层（当次 Input 的 `ctx.UserID`，预算/凭据跟着走）。**怎么认出是谁**（数人头 / 唇动 / 声纹 / 名册）属插件层与周边服务协商范围，见 03 §6，暂缓。

## 3. 配置分层（冲突解法：分层，不是回写博弈）

```
生效配置 = local     （进程内：启动 flag 覆盖，不落盘）
         △ override  （机器写：网页设置面板「保存」的目标，aria.override.toml）
         △ base      （人写：aria.toml，程序只读、永不重写，注释永远安全）
         △ 默认值     （代码内）
```

**读一次，运行期只写回（2026-09-14 定稿）**：web 是配置的操作面（内存即真相），文件是持久化载体（下次启动还原）。**不做任何自动监听**——文件级监听的原子保存盲区（[viper#142](https://github.com/spf13/viper/issues/142)）、mtime 粒度（实测 tmp+rename 前后同纳秒）这些坑随之不再需要解决。外部手改文件后要生效：调 `Manager.Reload()`（面板给一个「重新读取」按钮）或重启进程。

- **网页改配置只写 override 层**：viper 实例只含被改的键。这是官方推荐的写法——`viper.WriteConfig()` 会把整个文件重写成 map（**注释必丢**，见 [viper#1104](https://github.com/spf13/viper/discussions/1104)，无修复计划），所以人写的 base 层必须让程序碰都不碰；
- **写回是原子写**：先写同目录临时文件再改名覆盖，任何读者都看不到半截文件；
- **`Reload()` 的失败取向**（拒绝静默降级）：override 文件被删 = 合法的「无覆盖层」，回到基准值；**base 文件读不到 = 保持上次生效值并记日志**（base 是必需文件，若按空内容解析会把整份配置静默重置成默认值）；语法错误同样保持旧值、记日志，修好后 `Reload` 自动恢复；
- **UI 上每个字段显示两态**：基准值 + 是否被覆盖，可一键「恢复默认」（= 删 override 里的该项）；
- key 本体永远在环境变量（配置里只存变量名），不落任何文件。

**热调 vs 重启**（网页面板上控件的生效语义）：

| 配置 | 生效机制 | 热调 |
|---|---|---|
| model / temperature / max_tokens | `ctxx.Options` 每请求覆盖（引擎现成能力） | ✅ 白送 |
| 压缩策略（`compress.strategy`：provider / keeplast / 将来 twopart） | 装配表造实例 + `Session.SetCompressor` 热切换（已实现；在途压缩用旧策略跑完） | ✅ 已实现 |
| persona / system prompt | `window.SetSystem` | ⚠️ 可热改，代价是前缀缓存失效（切换不频繁可接受） |
| 工具开关 | loop 工具表构建时固定 | ❌ v1 重启生效 |
| base_url / api_key_env / listen / record | Agent 构建时定 | ❌ 重启 |

配置键与默认值见仓库根 [`aria.toml`](../aria.toml)（带注释样例）。

## 4. 操作面板：aria-web（待做）

**设置面板为主 + 对话测试页**：面板管配置（改完保存即热生效），对话页用来在网页上直接聊——网页聊天本身就是测试手段，不另造断言框架。

| 页面 | 内容 |
|---|---|
| 设置 | 上面热调表里的各项 + 显示「基准值 / 已覆盖」两态；保存写 override 层 |
| 对话 | 聊天框（自带插头：点发送=说完）+ 实时事件流（工具轨迹、增量）+ 窗口状态（memory/recent 看得见压缩在换血） |

实现形态：`go:embed` 单页静态资源（零构建链，`go run ./cmd/aria-web` 即用）；协议 = SSE 收事件流 + `POST /input` 发话 + `GET /status` 拉窗口快照；默认只绑 `127.0.0.1`（这个口能触发 LLM 花费，暴露局域网需显式改配置）。

## 5. 实现落点

| 位置 | 职责 |
|---|---|
| `internal/config` | 配置管理器（Load/Effective/Base/Set/Clear/SetLocal/Reload/OnChange/SetLogf）；启动读一次、运行期只写回 |
| `internal/assemble` | 宿主侧「名字 → 零件」装配表（压缩策略等）：引擎只认接口，不认识配置里的名字 |
| `internal/aria-host` | aria-host 的定制组件集：`Engine`（配置→provider→agent→会话组装/收尾）、`Sink`（多插头投递纪律，本表 §2）、`StdinPlug`/`SplitSpeaker`、`ConsumeTerminal` 终端渲染、`ResolvePersona`、echo provider——main 只做接线 |
| `runtime/app` | 宿主外设挂载与生命周期容器：`Mount`（事件流消费者：订阅+goroutine，收尾=退订+排干）、`Service`（后台服务：独立 ctx，收尾=取消+等待）、`OnShutdown`（链底钩子）。顺序纪律：挂载即订阅（消费者先挂、输入插头最后——事件不重放），收尾严格逆序 LIFO（输入先停→消费者排干→资产回收→会话/引擎关闭）；只依赖 core/loop，main 退化成一份挂载清单 |
| `plugins/vision/gowild` | 视觉注入（discussions 2026-09-11 §1 拍板 B 形态，2026-09-28 落地）：`Vision` 实现 core 槽 1 的额外变换——每次 LLM 调用现拉 `GET /api/camera/frame`（2s 硬顶）追加在组装结果最底部（user 消息：说明 + `ImageBlock(Data)`）；不进窗口历史、不落盘、失败安静跳过。每请求恰好一张「此刻」帧，旧帧永不重发 |
| `cmd/aria-demo` | 终端薄壳：**吃配置文件**（flag 只作进程内覆盖 = local 层）、stdin 行 = 成轮话语、`[名字]` 前缀切换说话人、流式打印、Ctrl+C 打断、`--record` JSONL、`/reload` 重读文件、`--print-config` 打印生效配置 |
| `cmd/aria-web` | 浏览器薄壳（待做） |
| `plugins/persist/jsonl` | durable 事件的 JSONL 落盘（格式 v1 带版本号，写路完整、读路随恢复需求） |
| `plugins/voice/gowild` | Gowild-HE 语音插件全家桶：`Gate`（半双工闸门：引用计数 + 迁移订阅，控制流的信号源）、`Input`（半双工输入纪律，引擎投递口 `InputSink` 由宿主实现多插头纪律）、ASR 输入插头（backend SSE）、TTS 事件订阅者（流式合成 → launcher 三段式播放 / paplay）、`Light`（RGB 状态灯：订阅闸门迁移与 partial 心跳，事件驱动）、`MicMute`/`FollowGate`（闭耳：订阅闸门迁移 → backend `POST /asr/mute`，回合中源头丢 mic 流——自回声 final 不产生、省流式解码） |

**同一进程内的即时生效**：面板保存 → `Set` 写入 override 层并更新内存 → 宿主下一轮 `Effective()` 就能拿到新值（demo 里模型/温度是每轮现取的，所以改完下一句就换模型）。压缩策略这类「构造期才注入」的部件由宿主在变更后重新装配并调 `Session.SetCompressor` 热切换（demo 的 `/reload` 就是这么做的）。这是「改配置不必重启」的实际含义；手改文件仍需 `Reload`/重启（见 §3）。

## 6. 未落地清单

1. `cmd/aria-web`（设置面板 + 对话测试页 + 事件流/窗口状态）；
2. 两段式压缩策略 `twopart`（引擎侧位置已备好，等 05 C4 拍板）；
3. **认主 / 名册**：插件层与周边服务协商（03 §6），暂缓；
4. **打断 / 小轮**：03 §7 设计已定稿，实现暂缓。
