# Kocoro 日历能力 — 需求与 Desktop 端技术方案

**状态**：草案 v0.5.1
**作者**：chen.zhai
**日期**：2026-05-26
**受众**：Kocoro Desktop 团队 / Daemon 团队

### 读者指引

| 角色 | 重点章节 |
|---|---|
| **Desktop 实现者** | §1（背景与范围）→ §3（Desktop 端方案）→ §5（RPC 协议）→ §7.2（待 Desktop 确认项）→ §6 Phase 0 验收 |
| **Daemon 实现者** | §1 → §2（架构）→ §4（Daemon 端契约面）→ §5 → §7.3（待 Daemon 确认项） |
| **产品 / Review** | §1 → §6（里程碑）→ §7（风险与开放问题） |

文中标注 ⚠️ 的段落是会咬人的具体坑（EventKit 行为陷阱 / 时区 / TCC 边界），实现前务必读。

---

## 一、产品需求

### 1.1 背景

用户希望 Kocoro Agent 能操作 macOS 日历（含 iCloud / Google / Microsoft 365 / Exchange / Outlook / Teams 会议），以便完成：

- 查询日程、查找会议邀请、读 Teams 加入 URL
- 创建 / 修改 / 取消事件、加 attendee 元数据、设提醒
- 让 Agent 主动汇报今日会议

> ⚠️ 一处先打预防针：v1 写入 attendees 字段是 EventKit 限制下的"元数据写入"——**Google / Exchange 后端不会自动发邀请**，详见 §3.3。"打开 Teams 加入 URL" 需要识别 URL 后交给 `browser` 工具，v1 不在本方案做（规划在 v2，见 §6 Phase 5+）。

我们的核心约束是**本地优先**和**不为每个供应商单独做 OAuth**。利用 macOS 系统层 (`系统设置 → 互联网账户` 已经把日历同步到本地 EventKit 数据库) 这一既成事实，是最经济的路径。

### 1.2 技术选型结论

| 选项 | 结论 |
|---|---|
| 自己做 Google/MS OAuth + Graph API | ✗ 重复造轮子，每家维护成本高 |
| Daemon (`.app` 包装) 直接 cgo 调 EventKit | ✗ 两条独立硬拦截：① `Kocoro Engine.app` 现有 `LSBackgroundOnly = true` 阻止 TCC 弹框（`requestFullAccessToEvents` 静默返回 `.denied`，没有错误日志，UX 等同"用户拒绝了"）；② TCC "responsible code" 归因走 bundle 层级向上找最近 user-visible 祖先，Calendar 授权会归到父 app `Kocoro Desktop` 名下而不是 `Kocoro Engine`——AppleEvents/Accessibility 能独立 grant 是它俩走特殊归因路径（client 二进制 code-signing identifier / 按二进制独立 grant），EventKit/Contacts/Reminders 走的是 TCC 常规路径，不适用。已在 macOS 15、macOS 26 Tahoe release notes 确认未变 |
| Sidecar (`cal_server` 类似 `ax_server`) | ✗ 受同样的 LSBackgroundOnly + responsible code 约束，同样不行 |
| **Desktop 调 EventKit，Daemon 通过 Unix domain socket RPC 调 Desktop** | ✓ **本方案** |

**为什么是这条路**：Kocoro Desktop 本身是 macOS 原生 App、已经 code-signed + notarized、已经持有用户其它 TCC 授权（截屏 / 通知）；EventKit 又是 Swift/ObjC 一等公民。让 Desktop 承担 EventKit 接入，Daemon 通过新增的 **Unix domain socket** 反向 RPC 通道（详见 §4.1）调用 Desktop 暴露的方法，是改动量最小且和 macOS 设计意图最一致的方案。

> v0.4 的"双向 WebSocket on `127.0.0.1`"方案在 v0.5 改为 Unix socket。原因是数据流向不对称：localhost TCP 对**接收命令**的现有端点（`/approval`、`/health`）信任模型成立，对**作为数据出口推 Desktop 端 TCC 状态和日历数据**则不成立——本机任意用户态进程都能连同一端口冒充 Desktop 喂假数据 / 监听 TCC 事件。Unix socket + 0600 文件权限 + 0700 父目录把信任边界缩到"能读当前用户 `~/Library` 的进程"，和 TCC 同级。

### 1.3 范围

> ⚠️ 关键判断：「互联网账户」是配置入口，**不是统一数据出口**。Calendar / Reminders / Contacts 走 EventKit / Contacts framework（Apple 一等公民 PIM 数据），Mail / Notes 没有公共 framework，只能 AppleScript。本方案 v1 只覆盖 framework 一类，v2 才把 AppleScript 一类并进来。

**v1（必须）—— 日历**

- 读：列出日历源、列出指定时间窗内事件、按 ID 取事件详情
- 写：创建、修改、删除事件；**attendees 字段作为元数据写入，邀请不自动发出**（Google/Exchange 限制，见 §3.3）；result 里 `invitations_sent: false` 让调用方知道
- 重复事件的实例级删除 / 修改 (this / this_and_future / all)
- TCC 授权状态查询和触发申请
- Daemon 与 Desktop 在线/离线状态协商

**v2（后续，按 framework 形态分两组）**

A. *EventKit / Contacts framework 类，沿用本方案的 RPC 架构*：
- 提醒事项（Reminders，EventKit 同 store，独立 TCC 类别 `NSRemindersFullAccessUsageDescription`）
- 联系人（Contacts framework，独立 TCC `NSContactsUsageDescription`）
- 查空闲时间（多人 attendee free/busy 聚合）

B. *AppleScript 包装类，Desktop 端用 NSAppleScript 包一层 RPC*：
- 邮件（Mail.app，无公共 framework，走 AppleScript；结构化输出 + 缓存 + 统一权限 UX 由 Desktop 承担，但底层仍是脚本）
- 智能识别 Teams / Zoom / Meet 加入链接并交给 `browser` 工具

v2 阶段建议把本文件重命名/扩展为 `docs/desktop-pim-rpc.md`，方法命名空间 `calendar.*` / `reminders.*` / `contacts.*` / `mail.*` 并行。

### 1.4 非目标

- ❌ 直接读用户在「互联网账户」里配置的 OAuth Token（Apple 的隐私边界，做不到也不该做）
- ❌ Teams 桌面客户端的聊天/通话状态（不在互联网账户体系内）
- ❌ **备忘录（Notes）** —— 没有公共 framework，Notes.app AppleScript 字典残缺（搜索 API 不完整、富文本基本读不出来），本地 SQLite 加密。v1 v2 均不做；如有强需求单独评估"截屏 + OCR + iCloud.com/notes 浏览器访问"的弯路方案
- ❌ 用户没在「互联网账户」里加过的日历源（例如只装了 Google Calendar 网页版的用户）—— 这种情况 v1 不支持
- ❌ **v1 阶段不通过本 RPC 通道做邮件** —— 邮件需求由 daemon 现有的 `applescript` 工具 + Mail.app AppleScript 字典临时承担（零代码、自动覆盖「互联网账户」里所有邮箱）。理由：邮件底层只能 AppleScript，没有 framework 红利可享，没必要为了"统一 RPC 通道"硬塞进 v1 推迟日历上线。v2 再把它并入是为了 UX 统一（权限管理面板、结构化输出），不是为了能力提升

---

## 二、整体架构

### 2.1 进程边界与职责

```
┌──────────────────────────────────────────┐
│  Agent Loop (Daemon, Go)                 │
│   - calendar_* 工具（仅 daemon 模式注册）  │
│   - DesktopRPCBroker（复用 ApprovalBroker）│
│   - permission/approval/audit 链路        │
└────────┬─────────────────────────────────┘
         │   Unix domain socket，权限 0600（仅当前 UID 可读写）
         │   path:    ~/Library/Application Support/run.shannon.shanclaw/daemon.sock
         │   pidfile: ~/Library/Application Support/run.shannon.shanclaw/daemon.pid
         │   传输框架：length-prefixed JSON（4 字节 big-endian uint32 + body，见 §5.1）
         │
         │   ① 帧: desktop_rpc_request    (daemon → desktop)
         │   ② 帧: desktop_rpc_result     (desktop → daemon)
         │   ③ 帧: desktop_event          (desktop → daemon，异步通知)
         │   ④ 帧: desktop_rpc_cancel     (daemon → desktop，撤销 in-flight RPC；占位，schema v1.x 定稿)
         ↓
┌──────────────────────────────────────────┐
│  Kocoro Desktop (.app, Swift)            │
│   - DesktopRPCService 路由               │
│   - CalendarProvider (EKEventStore)      │
│   - TCC 首次授权 UX                       │
│   - daemon 进程 supervision（DaemonManager） │
└──────────────────────────────────────────┘
```

> 部署前提（v0.5 显式化）：daemon 是 Desktop 的子进程（`DaemonManager` 通过 `Process()` spawn `Helpers/ShanClaw Engine.app/Contents/MacOS/shan`，传 `--rpc-socket <path>`），版本随 Desktop release 捆绑。Desktop UI 退出时不主动 kill daemon——daemon 被 launchd 收养继续跑（PPID → 1），sock 文件保留，Slack/LINE 等 cloud channel 仍能触发。Desktop 下次启动时按 §4.1.1 的 reconciliation 流程决定复用 vs respawn。`internal/daemon/launchd_darwin.go` 仅服务 npm CLI 独立安装路径，不在本方案 v1 适用范围。

### 2.2 调用链关键节点

1. **模型决定调 `calendar_list_events`** — Daemon 端工具入口
2. **Daemon 权限/审批链** — `RequiresApproval` 判断（读=否，写=是），写操作走 `ApprovalBroker` 请用户批准
3. **Daemon 通过 Unix socket 帧推 `desktop_rpc_request`** — `DesktopRPCBroker.Request(method, params)` 阻塞等待
4. **Desktop 接收帧** — 路由到 `CalendarProvider.handle(method, params)`
5. **Desktop 调 EventKit** — `EKEventStore.events(matching:)` 或 `.save(_:span:)`
6. **Desktop 通过同一 sock 帧推 `desktop_rpc_result`** — 含 `request_id` + `result` 或 `error`
7. **Daemon Broker 按 `request_id` 解阻塞** — 工具返回结构化结果给模型

### 2.3 进程生命周期与可用性

**前提**：daemon 由 Desktop spawn（见 §2.1 部署前提说明）。calendar 工具的注册是**启动时 conditional registration**（详见 §4.3）——daemon 模式（被 Desktop spawn 且持有 `DesktopRPCBroker`）才注册 `calendar_*` 工具；TUI / one-shot / MCP / scheduled 等其他运行模式根本不会注册，不会出现"工具在列表里但调不通"的情况。

| 情形 | 行为 |
|---|---|
| Desktop 启动，daemon 首次 spawn | Desktop 通过 §4.1.1 reconciliation 流程检测无残留 daemon → spawn 新 daemon → daemon listen sock + 写 pidfile → Desktop connect sock → 发 `system.capabilities` 协商版本 → 发 `desktop_event` 报告上线。calendar 工具正常可用 |
| Desktop 启动，已有残留 daemon（上次 UI 退出后未 kill，被 launchd 收养） | Desktop 按 §4.1.1：发现 pidfile + PID 活跃 → connect sock → `system.capabilities` 版本匹配则复用，不匹配则 SIGTERM 老 daemon 后 respawn |
| Desktop UI 退出（用户 Cmd+Q 或自动更新） | daemon 不被 kill，继续 listen sock，Slack/LINE 等 cloud channel 触发的请求仍能完成（calendar 工具仍可用，只是 Desktop 端 UI 反馈通道关闭） |
| daemon 在调用过程中崩溃 / sock 断开 | Daemon `CancelAll` 立即解所有 pending RPC → 模型拿到 `desktop_disconnected` 错误。Desktop 端 supervisor（`DaemonManager`）检测到子进程退出后按 `maxRestarts` 策略尝试 respawn |
| Cloud channel 触发（Slack/LINE 等）且本地 Desktop 没运行过 | 该用户机器上根本没有 daemon 进程 → cloud relay 找不到 endpoint → 用户在 Slack 看到的是 cloud relay 层的兜底回复（与 calendar 工具无关，是 daemon 总体不可达） |

---

## 三、Desktop 端技术方案

### 3.1 必备前置条件 — 由 Desktop 团队负责确认

| 项 | 要求 | 验证方式 |
|---|---|---|
| `Info.plist` 加 usage description | `NSCalendarsFullAccessUsageDescription`（macOS 14+，要 read 必须 full-access）、`NSContactsUsageDescription`（v2 用）、`NSRemindersFullAccessUsageDescription`（v2 用） | 重新打包后用 `codesign -dvvv` + `defaults read Info` 检查 |
| Hardened Runtime 启用 | 是 | `codesign -d --entitlements - <App>` |
| App 已 notarize | 必须 | TCC 才会"记住"授权，否则每次启动都重弹 |
| Bundle ID 稳定 | `run.shannon.shanclaw`（Kocoro Desktop 现有 ID，**不要变**） | TCC 数据库按 bundle ID + code requirement 索引，ID 变了授权全清空 |
| Team ID 稳定 | 同上 | 用同一个开发者账号签 |

**警告**：Bundle ID 或签名身份变更等同于"换了一个新 App"，用户授权全部失效。计划期 v1 上线前不要改这两个。

### 3.2 EventKit 接入要点

```swift
// Kocoro Desktop minimum target = macOS 15+（CLAUDE.md "macOS 15+ and iOS 18+"），
// requestFullAccessToEvents 在 macOS 14+ 可用，因此本项目无需 fallback。

// 1. 持有一个 store 实例
let store = EKEventStore()

// 2. 申请 Full Access（read 必须 full-access，详见 §5.2 check_permission 映射表）
try await store.requestFullAccessToEvents()

// 3. 监听 EKEventStoreChangedNotification 处理外部变更（CalendarAgent 同步回写）
NotificationCenter.default.addObserver(
    forName: .EKEventStoreChanged, object: store, queue: .main
) { _ in /* 失效本地缓存 */ }

// 4. 查询用 predicate（不要遍历）
let predicate = store.predicateForEvents(
    withStart: start, end: end, calendars: calendars)
let events = store.events(matching: predicate)
```

### 3.3 ⚠️ 四个具体坑（必须处理）

#### 时区与日期时间格式

- EventKit 的 `EKEvent.startDate / endDate` 是 `Date`（绝对时刻）+ `timeZone: TimeZone?` 单独存
- RPC 协议钉死 **RFC 3339**（ISO-8601 的严格子集），不接受宽泛 ISO 8601：
  - ✅ `2026-05-26T09:00:00+08:00`
  - ✅ `2026-05-26T09:00:00.123+08:00`（fractional seconds 可选）
  - ✅ `2026-05-26T01:00:00Z`（`Z` 是 `+00:00` 别名）
  - ❌ 无时区后缀（`2026-05-26T09:00:00`）→ 返 `invalid_argument`
  - ❌ 仅日期（`2026-05-26`）→ 除非 `all_day: true`，否则返 `invalid_argument`
- Swift 端用 `ISO8601DateFormatter` 配 `[.withInternetDateTime, .withFractionalSeconds, .withTimeZone]`；Go 端用 `time.RFC3339Nano`
- **All-day 事件 end 边界**（EventKit 长期歧义点）：
  - RPC 层钉死：`end` 是**最后一天 23:59:59 当地时区**（inclusive end-of-day）
  - Desktop 内部需转换到 EventKit 的 exclusive next-midnight（`endDate = end-of-day + 1 second`，或转成次日 00:00:00）
  - 一个一天的 all-day 事件：`start: 2026-05-26T00:00:00+08:00`, `end: 2026-05-26T23:59:59+08:00`

#### 重复事件的删除/修改 scope

EventKit 的 API：
```swift
try store.remove(event, span: .thisEvent)        // 删除单次实例
try store.remove(event, span: .futureEvents)     // 删除该实例及之后所有
// 没有 .allEvents — 要删整个系列必须先拿到 series master 再 remove
```

RPC 协议层封装成三态：
```
scope: "this"            → .thisEvent
scope: "this_and_future" → .futureEvents
scope: "all"             → Desktop 端先 fetch master event 再 .thisEvent
```

#### Google / Exchange 同步延迟

EventKit 写入立刻落本地 SQLite，CalendarAgent 后台异步推到云端。如果 Daemon 工具链需要"写后立刻读校验"，Desktop 应：

- 写入返回前 await `store.commit()` 显式落地
- 不在 RPC 内做"写后查"的二次校验（让 Daemon 端工具自己决定要不要轮询）
- 在 result 里附带 `pending_remote_sync: bool` 告诉调用方"本地已写，云端可能未推完"

#### ⚠️ attendees 写入在 macOS EventKit 上不会自动发邀请

这是会咬人的坑。EventKit 在 macOS 允许**读** `EKEvent.attendees`，但**写入 attendees 不会触发邀请邮件**到 Google CalDAV / Exchange 后端 —— `EKEventStore.save` 不报错，但 Google/Outlook 那边的收件人根本收不到邀请。Calendar.app 自己能发邀请是因为它通过 Mail.app/Exchange 协议另外发，不是通过 EventKit save 路径。

**v1 决策**：
- 接受 `attendees` 字段，作为事件 metadata 写入
- create_event / update_event 在 result 中**始终返回** `invitations_sent: false`（无论 attendees 是否为空），明确告诉调用方"邀请没发出去"。schema always-present 比"按情况省略"对模型更友好（不用做字段存在检查）—— v1 始终 `false`，v1.x AppleScript-Calendar fallback 落地后可能翻 `true`
- **v1.x patch** 计划走 Desktop 端 AppleScript-Calendar.app fallback 来发邀请（Calendar.app 内部会调 Mail.app/Exchange 协议），按 §7.4 changelog 中"cancel > request_permission > attendees"顺序解锁，不等 v2

这样模型拿到 result 后可以告诉用户："事件创建好了，但邀请需要你自己在 Calendar 里点发送" —— 避免静默失败的最坏 UX。

### 3.4 TCC 首次授权 UX

```
用户首次让 Agent 调日历
    │
    ↓
Daemon: calendar.check_permission → Desktop
Desktop 回 "not_determined"
    ↓
Daemon: 工具返回 [calendar_permission_not_determined] 错误，附 hint
模型告诉用户："需要日历权限，请在 Kocoro 设置里授权"
    │
    ↓
用户点开 Desktop 设置 → 点"授权日历"
Desktop 调 store.requestFullAccessToEvents()
macOS 弹出系统对话框（"Kocoro 想要访问你的日历"）
用户点"允许"
    ↓
Desktop 通过 sock 通道推送 desktop_event { event: "calendar_permission_changed", data: { status: "granted" } } 给 Daemon
    ↓
用户重新让 Agent 试，这次成功
```

**两条触发授权的路径**（模型可选）：

| 路径 | 适用场景 | 行为 |
|---|---|---|
| **A. 用户驱动**（上图） | 推荐 / UX 友好 | 模型告诉用户去 Desktop 设置里点"授权日历"按钮，用户看见自己点了什么 |
| **B. 模型直接 RPC** | 用户已经在和 Agent 交互、信任度高 | 模型直接调 daemon 工具 `calendar_request_permission` → RPC 到 Desktop → Desktop 调 `requestFullAccessToEvents` → 系统弹框，Desktop 不需要 UI 介入 |

两种都合法。路径 B 的好处是不打断 Agent 对话；路径 A 的好处是用户更清楚自己在授权什么。**Desktop 两边都要实现** —— 路径 B 用 `calendar.request_permission` RPC，路径 A 用 Desktop 设置页里的按钮（同一个内部函数）。

**deep link 约定**：Daemon 在权限缺失时除了返工具错误，可以通过现有 notification 通道让 Desktop 弹一个**带"去授权"按钮**的卡片（参考现有 approval card 模式）。按钮点了直接跳到对应设置页：

```
kocoro://settings/permissions/calendar
kocoro://settings/permissions/reminders   (v2)
kocoro://settings/permissions/contacts    (v2)
```

Desktop 端注册 URL scheme 处理这些 deep link，跳到对应 panel。

### 3.5 Desktop 端可用性事件

Desktop 端的非请求型通知（TCC 状态翻转、EventKit 数据变更等）通过 **Unix socket 上的 `desktop_event` 帧**主动推给 Daemon（见 §5.1）。Desktop 离线由 Daemon 自己根据 sock EOF/EPIPE 判定，无需 Desktop 显式推送。

| 事件类型 | 谁产生 | 时机 | payload |
|---|---|---|---|
| `desktop_online` | Desktop 推 | **§4.1.1 reconciliation 完成、`system.capabilities` 版本协商通过之后**的第一个非请求型帧 | `{version: "1.0.0", platform: {os: "macOS", os_version: "14.4.1", app_version: "1.2.3"}}` — 仅声明"我作为 client 已上线"；version / methods 列表已由 `system.capabilities` 完成协商，不在此帧重复 |
| `desktop_offline` | **Daemon 内部构造** | sock 断开时 daemon 自己生成（EOF / EPIPE），不用 Desktop 推 | `{}` |
| `calendar_permission_changed` | Desktop 推 | TCC 状态翻转（用户在系统设置或 Desktop 设置里改了授权） | `{status: "granted" \| "denied" \| "not_determined" \| ...}` |
| `calendar_data_changed` | Desktop 推 | `.EKEventStoreChanged` Notification 触发 | `{}`（Daemon 仅作失效缓存提示，可选实现） |

---

## 四、Daemon 端契约面 — 让 Desktop 团队知道我们怎么对接

> 这部分由 Daemon 团队实现，列在这里是为了让 Desktop 团队心里有数。

### 4.1 IPC 通道：Unix domain socket，复用 ApprovalBroker 代码结构

> ⚠️ **关键澄清**：DesktopRPCBroker 只在 **daemon ↔ 本地 Desktop** 之间走，永不经 Cloud relay。即使用户从 Slack/LINE 这类 cloud channel 触发了一次日历查询，RPC 也是从本地 daemon 直接发到本地 Desktop。"复用 ApprovalBroker"指的是**代码结构**（pending map / blocking channel / cleanup hooks / onCleanup 总线发布模式），不是同一条传输链路。ApprovalBroker 的 wire path 是 daemon → Cloud → Ptfrog，DesktopRPCBroker 的 wire path 是 daemon ↔ 本机 Unix socket ↔ Desktop。

**单一传输：Unix domain socket**

- **生命周期**：daemon 是 Desktop 的子进程（已经通过 `Helpers/ShanClaw Engine.app` 这条路启动），Desktop 启动 daemon 时把 sock path **和** pidfile path 作为**两个独立 CLI flag** 传入（v0.5.1 起：避免 daemon 派生 pidfile path 与 Desktop 硬编码 path 的隐式耦合）：
  ```
  shan --rpc-socket  "$HOME/Library/Application Support/run.shannon.shanclaw/daemon.sock" \
       --rpc-pidfile "$HOME/Library/Application Support/run.shannon.shanclaw/daemon.pid"
  ```
  daemon 启动时 `os.Remove` 旧 sock 文件后 `net.Listen("unix", path)`，监听完立刻 `os.Chmod(path, 0600)`（macOS Unix socket 文件权限**默认遵从 umask**，必须显式 chmod）。
- **目录权限**：父目录 `~/Library/Application Support/run.shannon.shanclaw/` 应为 0700。Desktop 在启动 daemon 之前 `mkdir -p` 并设置权限。
- **pidfile**：daemon 启动 listen sock 成功后 atomically（write-then-rename）写 `--rpc-pidfile` flag 指定的路径（约定值 `~/Library/Application Support/run.shannon.shanclaw/daemon.pid`），内容是单行 PID（与 daemon 现有 `internal/daemon/pidfile.go` 风格一致）。Desktop 在 §4.1.1 reconciliation 流程中读它。两边**绝对不要派生** pidfile path（不要假定它是 sock path 改后缀）—— 用 flag 显式传入的值即可。
- **连接方向**：daemon 监听，Desktop 是 client，connect 后 §4.1.1 reconciliation 完成后才发 `desktop_event { event: "desktop_online", payload: {...} }`。
- **重连**：Desktop 检测到 sock 断开（read EOF / write EPIPE）→ 退避重连。daemon 重启时会重写 sock 文件，Desktop 重连即可。
- **帧 schema**：见 §5.1（length-prefixed JSON）。

**为什么不用 localhost TCP + WebSocket（v0.4 方案）**：
1. **信任边界**：TCP `127.0.0.1` 对本机所有用户态进程开放（浏览器扩展、第三方脚本、被投毒的工具链）。Unix socket + 0600 文件权限 + 0700 目录权限把攻击面缩到"能读当前用户 `~/Library` 的进程"——和 TCC 同级信任边界。
2. **不需要端口发现**：sock path 是约定常量，不需要 `~/.shannon/daemon.port` 文件或 `KOCORO_DAEMON_PORT` env，少两个故障模式（stale port file / env not inherited）。
3. **不需要 WebSocket 协议开销**：HTTP upgrade 握手、masking、ping/pong——sock 都不需要，length-prefixed JSON 简单几行代码。

**为什么不用 HTTP POST 回执（v0.3 方案）**：sock 是双向流，回执走同一条 sock 简单很多——不用每请求生成 token 防伪。HTTP POST 那条路在 v1 不开放，未来如有 browser-based admin UI 需要再说。

**信任模型**：依靠 Unix 文件权限。daemon 在 accept 后还可以 `getpeereid`（macOS）二次验证连接对端 UID 等于自己 UID——v1 默认 OFF（文件权限 0600 已足够），v2 可加可选 `--strict-peer-uid` flag，给企业部署场景用。

**代码层面**：
- 复制 `ApprovalBroker` 拷一份叫 `DesktopRPCBroker`（v1 不做泛型抽象 —— YAGNI）
- pending 表 key 是 `request_id`，value 是带 result channel 的 struct，超时 / sock 断开 / 显式 cancel 都会解阻塞
- Desktop sock 断开时调 `CancelAll`，pending 全部以 `desktop_disconnected` 错误解阻塞，**不做请求重放**
- framing codec 写在 `internal/daemon/desktop_rpc/codec.go`，不引第三方库（`bufio.Reader` + `binary.BigEndian.Uint32` 几十行）

**调试辅助**：联调期间用 `system.ping` 方法（schema 见 §5.2）验证通道双向通。版本协商已在 reconciliation 流程（§4.1.1）+ `system.capabilities` 中完成，不需要单独联调。

### 4.1.1 Desktop 启动 daemon 时的版本协商（reconciliation 流程）

**问题背景**：v0.5 部署模型是"半绑定"（见 §2.1 部署前提）—— Desktop UI 退出时不主动 kill daemon，daemon 被 launchd 收养继续跑。这意味着：

```
T0  用户跑 Kocoro Desktop v1.0，spawn shan v1.0，daemon listen sock
T1  用户关 Desktop UI    → shan v1.0 孤儿化继续跑（PPID = 1）
T2  Sparkle 自动更新 → Desktop.app 文件已替换为 v2.0，但 shan v1.0 进程还在跑
T3  用户重新打开 Desktop v2.0
    如果 Desktop v2.0 直接 connect 这个 sock：会和 shan v1.0 对话，
    调到 v2.0 才有的 RPC 方法时 shan v1.0 报 unknown method → 工具诡异半挂
```

**reconciliation 规约**：Desktop 每次启动按以下顺序处理 daemon 进程：

```
1. 读 pidfile: ~/Library/Application Support/run.shannon.shanclaw/daemon.pid

2. 如果 pidfile 存在 且 对应 PID 还在跑（kill(pid, 0) == 0）：
   a. 尝试 connect sock
   b. 连上后第一帧发 system.capabilities（不是 desktop_event）
   c. 比对 result.version 和 Desktop 自己期望的协议版本：
      - 匹配 → 复用现有 daemon，发 desktop_event { event: "desktop_online", ... }
      - 不匹配 → 步骤 2c-i 的 SIGTERM 路径
   d. connect 失败（sock 已 stale、daemon 进程已死但 pidfile 残留）→ 转步骤 3

   2c-i. SIGTERM 路径：
        - kill(pid, SIGTERM)
        - 轮询 kill(pid, 0) 直到返回 ESRCH（进程已退）或 5s 超时
        - 5s 超时未退 → kill(pid, SIGKILL)，再轮询 2s
        - 2s 仍未退（极端罕见，僵尸进程等）→ Desktop 弹用户可见错误
          "Engine 残留进程清理失败，请重启 Mac"，不静默继续
        - 退出成功 → 转步骤 3

3. 清理残骸：os.Remove(sock) + os.Remove(pidfile)

4. spawn 新 daemon：
   Process(executableURL = <bundled shan>, arguments = ["--rpc-socket", <path>])
   inherit shell env（让 daemon 找到 nvm/npx/pyenv/GOPATH 等，参考现有
   DaemonManager.resolveShellEnvironment 实现）

5. daemon 端启动顺序：
   a. os.Remove(sock_path)  ← 防御 stale file
   b. net.Listen("unix", sock_path)
   c. os.Chmod(sock_path, 0600)
   d. write-then-rename pidfile（先写 daemon.pid.tmp，再 rename 到 daemon.pid）
   e. 开始 accept loop
```

**关键设计点**：
- pidfile 是 daemon 写的（步骤 5d）、Desktop 读的（步骤 1）—— 双方仅约定 path，不走 IPC
- pidfile 内容：纯数字 PID 一行，无其他字段
- 步骤 2b 的 `system.capabilities` 必须在 `desktop_event { desktop_online }` **之前**——收到 desktop_event 意味着 Desktop 已"上线"，再杀 daemon 时序会乱
- `system.capabilities` 在本流程中**不只是诊断方法**，是 reconciliation 必备的版本协商方法（见 §5.2）
- pidfile 指向的 PID 被复用给其它进程（极罕见 wrap-around 场景）：步骤 2a 的 connect sock 会失败（其它进程不监听这个 sock）→ 自然 fallback 到步骤 3，不需要额外检查二进制
- pidfile 存在但 PID 已死（daemon 异常崩溃没清理 pidfile）：步骤 2 的 `kill(pid, 0)` 返 ESRCH，直接跳到步骤 3 cleanup
- 同 Mac 多 Desktop 实例并发启动：v1 假设单 Desktop 实例。同账户 macOS LaunchServices 默认复用现有实例；多账户共享 Mac 各自的 `~/Library/...` 天然隔离

**Swift 端 SIGTERM 实现**：daemon 在被 launchd 收养后 PPID = 1，Desktop 不能用 `Process.terminate()`（那只能终止自己的子进程）。需要从 pidfile 读 PID，然后调 Darwin 的 `kill(pid_t, Int32)` syscall（C 互操作，`import Darwin` 即可）。

### 4.2 新增 Daemon 端工具（`internal/tools/calendar_*.go`）

每个工具的 `RequiresApproval` 表现：

| 工具 | 读/写 | RequiresApproval | 备注 |
|---|---|---|---|
| `calendar_list_sources` | 读 | 否 | 列出所有日历源（账户） |
| `calendar_list_events` | 读 | 否 | 时间窗内事件 |
| `calendar_get_event` | 读 | 否 | 单事件详情 |
| `calendar_create_event` | 写 | **是** | 走标准审批流 |
| `calendar_update_event` | 写 | **是** | |
| `calendar_delete_event` | 写 | **是** | |
| `calendar_check_permission` | 读 | 否 | 内部用 |
| `calendar_request_permission` | 副作用 | **是** | 触发 TCC 弹框是显式行为 |

**审批策略**：写操作沿用现有 always-allow 机制（参考 `internal/daemon/alwaysallow.go`）。用户可以"始终允许 calendar_create_event for agent X"，下次就不弹了。

### 4.3 工具列表注册（启动时 conditional registration）

shan 有多种运行模式：daemon（被 Desktop spawn 持有 `DesktopRPCBroker`）、TUI、one-shot CLI、MCP server、scheduled task。**只有 daemon 模式才有 sock 反向通道**，其它模式根本到不了 Desktop——把 calendar 工具注册到这些模式是误导（模型看到工具但调用必失败）。

正确处理：**进程启动时按是否持有 broker 决定注册与否**，不做 v0.4 提议过的 per-request runtime filter（运行时过滤是"工具注册了但又不让模型看到"的反模式）。

`internal/tools/register.go` 现有 `session_search` / `cloud_delegate` 等已是按依赖项条件注册，calendar 工具沿用同模式：

```go
// 伪代码示意，实际放在 RegisterLocalTools 内
if rpcBroker != nil {
    registry.Register("calendar_list_sources", ...)
    registry.Register("calendar_list_events", ...)
    // ... 其余 calendar_* 工具
}
```

**TUI / one-shot / MCP / scheduled 模式下的 calendar 需求 fallback**：模型可以自然推理走 daemon 现有的 `applescript` 工具（osascript → Calendar.app）。`internal/skills/bundled/skills/kocoro/references/calendar.md` 应该带一段 fallback prompt 提示（注意 AppleScript 走 Automation TCC 而不是 Calendars TCC，权限模型不同），引导模型在 calendar 工具不可见时使用这条路径。这不需要 daemon 端额外编码——纯 prompt 工程。

### 4.4 文档同步

CLAUDE.md 约定每个 `mux.HandleFunc(...)` 都要在 `internal/skills/bundled/skills/kocoro/references/*.md` 有对应文档。本方案新增：

- `internal/skills/bundled/skills/kocoro/references/calendar.md` — 工具用法、字段含义、错误码
- `internal/skills/bundled/skills/kocoro/references/desktop-rpc.md` — Unix socket（`~/Library/Application Support/run.shannon.shanclaw/daemon.sock`）+ pidfile + length-prefixed JSON 帧 schema + §4.1.1 reconciliation 流程契约
- README.md / AGENT.md / AGENTS.md — 顶层用户/外部 Agent 视角说明

---

## 五、RPC 协议详细规约

### 5.1 信封（Unix socket 帧）

所有帧都是 JSON 对象，**length-prefixed**：4 字节 big-endian uint32 表示后续 JSON body 字节数，body 是 UTF-8 编码的 JSON。顶层有 `type` 字段。v1 共三种 type（payload schema 见下文）；v1.x 将引入第四种 `desktop_rpc_cancel`（架构图占位，schema 待 cancel 规约落地）：

#### Daemon → Desktop: `desktop_rpc_request`

```json
{
  "type": "desktop_rpc_request",
  "payload": {
    "request_id": "drpc_<16hex>",
    "method": "calendar.list_events",
    "params": { /* method-specific */ },
    "timeout_ms": 30000,
    "session_id": "sess_...",
    "agent": "default",
    "source": "slack",
    "ts": "2026-05-26T10:00:00+08:00"
  }
}
```

字段说明：
- `request_id`：daemon 生成的 16 字符 hex，加 `drpc_` 前缀。Desktop 在 `desktop_rpc_result` 里原样回填
- `method`：见 §5.2 方法清单
- `timeout_ms`：daemon 端的硬超时；Desktop 应自我限时，超过 `timeout_ms - 2000` 还没出结果建议主动 abort 并回 `timeout` 错误，让 daemon 直接拿到错误而不是干等
- `source`：触发本次请求的源 channel（`slack` / `wecom` / `kocoro` / `cli` / `schedule` 等，对应 RunAgentRequest.Source）。Desktop 可在审计 UI 展示"这次是从 Slack 触发的"
- `session_id` / `agent`：daemon 端的上下文信息，Desktop 不需要解读，但可用于日志关联
- `ts`：daemon 发送时间戳，RFC 3339

#### Desktop → Daemon: `desktop_rpc_result`

成功：

```json
{
  "type": "desktop_rpc_result",
  "payload": {
    "request_id": "drpc_<16hex>",
    "ok": true,
    "result": { /* method-specific */ }
  }
}
```

失败：

```json
{
  "type": "desktop_rpc_result",
  "payload": {
    "request_id": "drpc_<16hex>",
    "ok": false,
    "error": {
      "code": "calendar_permission_denied",
      "message": "human-readable explanation",
      "retriable": false,
      "details": { /* code-specific, optional */ }
    }
  }
}
```

- `code` 取值见 §5.3
- `retriable: true` 仅用于真正瞬态错误（暂时性 EventKit 锁竞争），daemon 端工具可重试一次；持久错误（permission_denied / not_found / invalid_argument 等）必须 `false`
- `details` 是 code 特定的结构化补充，可选

#### Desktop → Daemon（少数情况下也可 Daemon → Desktop）: `desktop_event`

非请求型异步通知，无 `request_id`：

```json
{
  "type": "desktop_event",
  "payload": {
    "event": "calendar_permission_changed",
    "data": {"status": "granted"},
    "ts": "2026-05-26T10:00:00+08:00"
  }
}
```

v1 事件列表见 §3.5。

#### 帧大小与 keepalive

- 单帧 JSON body 上限 **4 MB**（length prefix 强约束 ≤ `4 * 1024 * 1024`，超出读到 prefix 即关连接，避免 OOM）。list_events 极限场景下事件数仍按 §5.2 的 `limit: 2000 + truncated: true` 限流，4 MB 足以容纳 2000 个事件的最大 JSON。
- **不需要应用层 ping**：Unix socket 断开是 OS 级 EOF / EPIPE，立刻可感知，不需要心跳。daemon 端 `ReadDeadline` 留空（或设极大值），靠 cancel 帧（v1.x）或对端关 sock 触发清理。
- **断开行为**：Desktop 主动关闭 sock → daemon 收到 EOF → `CancelAll` 所有 pending RPC。daemon 退出时关 sock，Desktop 收到 EOF，触发 §4.1.1 reconciliation 重启流程。
- **framing 规约**：读 = 先读 4 字节 → uint32 长度 N → 检查 `0 < N ≤ 4 * 1024 * 1024`（其它范围立即关 sock）→ 读 N 字节 body → `json.Unmarshal`；写 = `json.Marshal` → 检查 `len ≤ 4 * 1024 * 1024` → write 4 字节长度 + body。**不做分片**：单帧一次完整收发，避免 reassembly 复杂度。

### 5.2 方法清单（v1）

**enum 命名规则**：所有 enum 字符串值统一 **lowercase snake_case**（`icloud` / `needs_action` / `not_determined`）。文档里多处出现的 `iCloud` / `CamelCase` 都按此规则归一。

---

#### `system.ping`

联调用，验证通道双向通。

**params**: `{"echo": "<arbitrary string>"}`（可选）

**result**: `{"pong": "<echo back>", "server_time": "<RFC 3339>"}`

#### `system.capabilities`

**双向方法（v0.5.1 钉死）**：两端**都**实现 responder 端，调用方向取决于使用场景：

| 场景 | 方向 | 响应方填什么 |
|---|---|---|
| §4.1.1 reconciliation（Desktop 启动握手） | **Desktop → Daemon**（Desktop 发请求） | Daemon 自己的 protocol version + ProtocolMethods（见 §5.5.2）+ daemon binary version |
| 诊断包（用户报 bug 时） | **Daemon → Desktop**（Daemon 发请求） | Desktop 自己的 protocol version + ProtocolMethods + Desktop bundle app version |

> v0.5 spec 此处一处笔误说"system.capabilities 是 request-response（daemon → desktop）" —— 该方向仅是诊断场景，reconciliation 主流程是 **Desktop → Daemon**，v0.5.1 修正。

**params**: `{}`

**result schema**:
```json
{
  "version": "1.0.0",
  "methods": [ /* 见 §5.5.2 ProtocolMethods，两端响应返回 byte-identical 数组 */ ],
  "platform": {
    "os": "macOS",
    "os_version": "14.4.1",
    "app_version": "1.2.3"
  }
}
```

**`version` 字段**：本协议规约的版本号（**不是** app version）。两端各自硬编码自己实现的 protocol version（v1 都是 `"1.0.0"`，见 §5.5.1）。Reconciliation 比对：
- 匹配 → 复用现有 daemon
- 不匹配 → Desktop 走 SIGTERM 路径 respawn 新版本 daemon（详见 §4.1.1 步骤 2c-i）

**`methods` 字段语义（v0.5.1 钉死）**：列表是"**本协议版本支持的所有方法**"，**不是**"本端 implement 的方法"。两端响应时返回**完全相同的硬编码数组**（按 §5.5.2 ProtocolMethods 顺序）。这样设计的两个原因：
1. Daemon 响应可以精确指出"我看不到 calendar.list_events 这个方法" —— v1.1 加新方法后，老 daemon 响应里没新方法名，新 Desktop 能精准报错
2. 不需要两端各自维护"我实现了哪些方法"的运行时表 —— protocol version 已经做了 lockstep 保证

Daemon-side 处理：reconciliation 握手时比对 `methods` 与自己 ProtocolMethods，diff 非空时 log warn 并写入 audit log（不做运行时过滤 —— 那是 §4.3 conditional registration 已经覆盖的 startup-time 决策）。

**`platform.app_version` 字段语义（v0.5.1 钉死）**：
- 当 responder 是 **Desktop**：bundle 的 `CFBundleShortVersionString`（如 `"1.2.3"`）
- 当 responder 是 **Daemon**：daemon binary 版本（来自 Daemon 端 `internal/version` 或等效包的 `Version` 常量，与 `shan --version` 输出一致）

**与 `desktop_event { desktop_online }` 的关系**：reconciliation 阶段 Desktop 先发 `system.capabilities` 请求（请求 → 响应），版本匹配后才发 `desktop_event { desktop_online }`（fire-and-forget，声明 client 已上线）。两者顺序固定：先 capabilities，匹配后才发 online。

---

#### `calendar.list_sources`

**params**: `{}`

**result**:
```json
{
  "sources": [
    {
      "id": "<EKCalendar.calendarIdentifier>",
      "title": "工作",
      "account_type": "icloud" | "google" | "exchange" | "outlook" | "local" | "subscription" | "other",
      "color_hex": "#FF5733",
      "writable": true,
      "default_for_new_events": false
    }
  ]
}
```

#### `calendar.list_events`

**params**:
```json
{
  "start": "2026-05-26T00:00:00+08:00",
  "end":   "2026-05-26T23:59:59+08:00",
  "calendar_ids": ["<id>", ...] | null,
  "query": null | "客户会议",
  "limit": 500
}
```

- `start` / `end`：RFC 3339（见 §3.3 时区段），必填
- `calendar_ids`：`null` = 所有日历；`["id1", "id2"]` = 指定日历；`[]` 空数组 = **明确返回空结果**（不要 helpfully 当成 null 处理）
- `query`：可选关键词，对 `title` + `notes` 做大小写不敏感 substring 匹配
- `limit`：返回事件数上限，默认 500，最大 2000。超过上限时 result 加 `truncated: true`

**result**:
```json
{
  "events": [
    {
      "id": "<EKEvent.eventIdentifier>",
      "calendar_id": "<EKCalendar.calendarIdentifier>",
      "title": "Q2 Review",
      "start": "2026-05-26T14:00:00+08:00",
      "end":   "2026-05-26T15:00:00+08:00",
      "all_day": false,
      "location": "Conference Room A",
      "notes": "见附件",
      "url": "https://teams.microsoft.com/l/meetup-join/...",
      "is_recurring": false,
      "is_recurring_instance": false,
      "series_master_id": null,
      "attendees": [
        {"email": "alice@x.com", "name": "Alice", "status": "accepted" | "tentative" | "declined" | "needs_action"}
      ],
      "organizer_email": "bob@x.com",
      "has_alarms": true
    }
  ],
  "truncated": false
}
```

**字段说明（list 视图的最小集，详细字段去 `get_event`）**：
- `series_master_id`：当 `is_recurring_instance: true` 时，指向系列 master event 的 `eventIdentifier`，否则 `null`。模型需要"删除整个重复系列"时，先从 instance 拿到 master_id 再调 `delete_event(id=master_id, scope="all")`
- `organizer_email`：对 organizer 是用户自己的事件，EventKit 的 `EKEvent.organizer` 可能为 `nil` —— 该字段也返回 `null`，不要伪造
- `attendees.status`：snake_case 化的 `EKParticipantStatus`，未知值统一映射成 `needs_action`

#### `calendar.get_event`

**params**: `{"id": "<EKEvent.eventIdentifier>"}`

**result**: 同 `list_events.events[0]` 全部字段，再加：
```json
{
  "recurrence_rule": {
    "frequency": "daily" | "weekly" | "monthly" | "yearly",
    "interval": 1,
    "by_day": ["MO", "WE"] | null,
    "end_date": "2026-12-31T00:00:00+08:00" | null,
    "occurrence_count": null | 10,
    "raw_rrule": "FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20261231T000000"
  } | null,
  "alarms": [
    {"minutes_before": 15}
  ]
}
```

- `recurrence_rule`：v1 显式支持的字段就是上面 5 个；`by_day` 仅在 `frequency: weekly` 时有意义，v1 不支持月/年频率的位置式表达（如"每月第一个周一"）
- `raw_rrule`：始终回填一份 RFC 5545 字符串，给模型/调用方做"我看不懂的 rule 也能原样回写"的逃生通道。读出来的 `raw_rrule` 是 EventKit 自己生成的，权威
- `alarms`：v1 只支持 relative alarm（事件前 N 分钟）；EventKit 的 absolute alarm 暂不暴露，遇到会被静默忽略

#### `calendar.create_event`

**params**:
```json
{
  "calendar_id": "<id>" | null,
  "title": "...",
  "start": "...",
  "end":   "...",
  "all_day": false,
  "location": null,
  "notes":    null,
  "url":      null,
  "attendees": [{"email": "...", "name": "..."}],
  "alarms": [{"minutes_before": 15}],
  "recurrence_rule": null
}
```

- `calendar_id`：`null` = `EKEventStore.defaultCalendarForNewEvents`
- 写入只读日历（如生日日历、订阅日历）→ 返回 `read_only_calendar` 错误
- `recurrence_rule` 结构同 `get_event`，但 v1 接受 `raw_rrule` 或结构化两种，二选一即可（同时给以 `raw_rrule` 为准）

**result**:
```json
{
  "id": "<new event id>",
  "pending_remote_sync": true,
  "invitations_sent": false
}
```

**`invitations_sent` 字段语义（v0.5.1 钉死 always-present）**：
- **v1 始终返回 `false`**，无论 request 的 `attendees` 字段是否为空 —— schema always-present 让模型不用做字段存在检查
- 语义：EventKit-only 路径不会通过 CalDAV/Exchange 自动发邀请（见 §3.3 attendees 坑）。即使 attendees 为空（事件无收件人），字段仍存在且为 `false`，表达"本次 RPC 没有发出任何邀请"这一事实，符合 EventKit 实际行为
- v1.x AppleScript-Calendar fallback 落地后，此字段可能翻 `true`（见 §3.3 v1.x 计划）

#### `calendar.update_event`

**params**:
```json
{
  "id": "<EKEvent.eventIdentifier>",
  "scope": "this" | "this_and_future",
  "patch": {
    "title": "...",
    "start": "...",
    "end":   "..."
  },
  "clear_recurrence": false
}
```

**`patch` 语义（v1 钉死，Desktop 实现必须严格遵守）**：

| 字段在 patch 中的形态 | 行为 |
|---|---|
| key 不在 JSON 里（缺失） | **不修改** |
| 值为 `null` | **不修改**（等价于缺失；Swift Decodable 区分这俩成本高，统一对待） |
| 字符串值为 `""` 或 list 值为 `[]` | **清空**该字段 |
| `attendees` / `alarms` 传非空 list | **整体替换**（不做 merge） |
| `recurrence_rule` 传新值 | **整体替换** |

**字段层面的额外约束**：
- `start` / `end` 在 EKEvent 上是必填，**不能清空** —— patch 里只能"改值"或"不改"，传 `""` 视为 `invalid_argument`
- `calendar_id` patch 改动 = 跨日历搬迁事件，EventKit 通过 `EKEvent.calendar = newCalendar` 实现，受目标日历 writable 限制；v1 支持
- `all_day` 切换会重写 start/end 的语义，patch 同时传 `all_day` + `start/end` 时按"先解释 all_day 再用对应语义解析 start/end"处理

要解除事件的重复属性，**必须传顶层 `clear_recurrence: true`**（patch 内传 null 含义已锁定为"不改"，不要复用）。

**`scope` 取值**：
- `this`：只改本次实例（创建 detached event）
- `this_and_future`：⚠️ **EventKit 这条路径会拆 series** —— 实际行为是创建一个新的 series 从本实例开始，原 series 在本实例前一次截断。结果是**该事件 ID 可能失效**，调用方应以 result 里返回的 `id` 为准

**为什么 update_event 没有 `scope: "all"`**（与 delete_event 不对称）：
- "update 整个系列" 在 EventKit 里 = 找到 series master + 更新它 + 让 CalendarAgent 把变更传播到所有实例
- 但 master 更新会让所有用户手动改过的 detached instance 也被覆盖（EventKit 不区分），是高破坏性操作
- v1 决策：要"改整个系列"，让调用方走 `delete_event(scope=all)` + `create_event` 两步，**显式、可审计**，避免静默覆盖用户的 instance 修改
- 真正需要"修改 master" 的场景留到 v2 再加 `scope: "series_master"`

**result**:
```json
{
  "id": "<可能是新 id，特别是 scope=this_and_future 时>",
  "pending_remote_sync": true,
  "invitations_sent": false
}
```

`invitations_sent` 字段语义同 `create_event` —— **v0.5.1 起 always-present，v1 永远 `false`**（即使 patch 不修改 attendees / 修改后为空列表）。schema 形状与 create_event 一致。

#### `calendar.delete_event`

**params**: `{"id": "...", "scope": "this" | "this_and_future" | "all"}`

**`scope` 取值**：
- `this` → `EKEventStore.remove(event, span: .thisEvent)`
- `this_and_future` → `EKEventStore.remove(event, span: .futureEvents)`
- `all` → 删除整个重复系列。Desktop 自动判断传入 `id` 形态：
  - 若 id 是 series **master**（`is_recurring && !is_recurring_instance`）→ 直接 `remove(master, span: .thisEvent)`，EventKit 会清掉整个 series
  - 若 id 是 series **instance**（`is_recurring_instance`）→ 通过 `event.calendarItemIdentifier` 或 `recurringEventsMatching` 找到 master 后 `remove(master, span: .thisEvent)`
  - 若 id 是非重复事件 → `scope: "all"` 等价 `scope: "this"`，不报错
  - 模型可以传 instance id 或 master id 任一形态，Desktop 内部归一

**result**: `{"ok": true, "pending_remote_sync": true}`

#### `calendar.check_permission`

**params**: `{}`

**result**: `{"status": "not_determined" | "denied" | "restricted" | "granted" | "write_only"}`

**`EKAuthorizationStatus` → RPC `status` 映射表（Desktop 严格按此映射）**：

| Apple enum | RPC `status` |
|---|---|
| `.notDetermined` | `not_determined` |
| `.restricted` | `restricted` |
| `.denied` | `denied` |
| `.fullAccess` | `granted` |
| `.writeOnly` | `write_only` |

> 项目 min target macOS 15+，`EKAuthorizationStatus.authorized`（macOS 14 前的语义）不需要处理。Swift 编译器在 15+ SDK 下也会标 deprecated。

Daemon 端工具看到 `write_only` 时：写操作可继续，读操作返 `calendar_permission_denied` 并附 `details.status = "write_only"`，提示用户去授权升级到 full access。

#### `calendar.request_permission`

**params**: `{}`

**result**: `{"status": "<new status after user decision>"}`

Desktop 内部调 `requestFullAccessToEvents`，阻塞到用户点完系统对话框才返回。已经是 `granted` / `denied` / `restricted` 状态时立即返回当前值，**不重新弹框**（macOS TCC 也不允许，会被静默忽略）。

**⚠️ v1 已知限制**：本方法在同步 RPC 模型下与默认 `timeout_ms = 30s`（spec §5.1）冲突 —— 系统弹框可以挂任意长时间等用户决定，30 秒太短。

**v0.5.1 缓解方案**（v1 内做的，不等 v1.x 异步化）：

| 端 | 行为 |
|---|---|
| **Daemon 端** | `calendar_request_permission` tool 在 RPC envelope 里**单独覆盖** `timeout_ms = 5 * 60 * 1000`（5 分钟），不用 calendar 其它工具的默认 30s |
| **Desktop 端** | `requestPermission()` 内部不要再加短于 5 分钟的超时；让阻塞调用一直等用户决定，避免 daemon timeout 与 Desktop timeout 双 race |

v1 行为（5 分钟覆盖仍超时的极端情况）：
1. Desktop 在 `timeout_ms - 2000` 处自我限时（按 §5.1 字段说明），返回 `timeout` 错误给 daemon
2. 但**系统弹框仍在显示**，用户最终点完后通过 §3.5 的 `calendar_permission_changed` 事件通知 daemon
3. 模型应在收到 `timeout` 后告诉用户"已发起授权请求，请点击系统弹框"，并在后续重新调 `calendar_check_permission` 确认最终状态

**v1.x 计划**改为 fire-and-forget + 事件回送的异步模型（按 §7.4 changelog 中 cancel > request_permission > attendees 的解锁顺序）。届时本方法返回 `{status: "pending"}`，最终状态由 `calendar_permission_changed` 事件单向回送。

### 5.3 错误码清单

| code | 由谁产生 | 含义 |
|---|---|---|
| `calendar_permission_denied` | Desktop | TCC 拒绝；`details.status` 给当前态（`denied` / `restricted` / `write_only`） |
| `calendar_permission_not_determined` | Desktop | 还没问过用户，需要先调 `request_permission` |
| `not_found` | Desktop | 事件 / 日历 ID 不存在 |
| `invalid_argument` | Desktop | 参数格式错（时间解析失败、必填缺失、enum 值无效、试图清空 start/end 等）；`details.field` 指出错在哪个字段 |
| `read_only_calendar` | Desktop | 试图写入只读日历（订阅日历、生日日历等） |
| `internal_error` | Desktop | 未分类异常，`message` 必填 |
| `timeout` | Desktop **或** Daemon | Desktop 自我限时主动放弃 → Desktop 推回 `desktop_rpc_result` 带此码；超过 `timeout_ms` 仍没回执 → Daemon 自己构造 |
| `desktop_disconnected` | **Daemon 构造** | Desktop 未连接 / 已断开；不应出现在 Desktop 回包里 |

### 5.4 并发与序列化

- **Daemon 端可能并发**发多个 `desktop_rpc_request`（bash_concurrency 默认开启，模型可能并发查多个时间窗）
- **Desktop 必须支持并发处理**：读类方法（list / get / check_permission）可任意并发
- **Desktop 内部建议把写操作（create / update / delete）serialize 到一个串行 dispatch queue**：EventKit 的并发写行为没有 Apple 官方保证，且 CalendarAgent 同步状态对并发写不友好
- Desktop 不需要做请求去重 —— Daemon 端的 ApprovalBroker 模式确保每个 `request_id` 唯一

### 5.5 正式标识符清单（v0.5.1 新增 — 两端契约 single source of truth）

**目的**：消除 Daemon Go 常量与 Desktop Swift 常量之间字符串拼写漂移的风险。所有 method 名、错误码、enum 字符串、deep link 等**两端共享的字符串**集中在本节钉死。两端实现期间 grep 本节做最后一道校验。

#### 5.5.1 ProtocolVersion

```
"1.0.0"
```

v1 唯一值。Bump 规则（v1.x 后决议）：方法新增 / 字段新增 = patch（1.0.x）；字段删除 / 重命名 / 类型变更 = minor（1.x.0）；架构层重写 = major（x.0.0）。

#### 5.5.2 ProtocolMethods（10 项，两端响应 `system.capabilities` 时返回**完全相同的 JSON 数组**）

```json
[
  "system.ping",
  "system.capabilities",
  "calendar.list_sources",
  "calendar.list_events",
  "calendar.get_event",
  "calendar.create_event",
  "calendar.update_event",
  "calendar.delete_event",
  "calendar.check_permission",
  "calendar.request_permission"
]
```

#### 5.5.3 帧 type 标识

```
"desktop_rpc_request"
"desktop_rpc_result"
"desktop_event"
"desktop_rpc_cancel"   ← v1.x 占位，schema 待定
```

#### 5.5.4 错误码（8 项；spec §5.3 同表）

```
"calendar_permission_denied"
"calendar_permission_not_determined"
"not_found"
"invalid_argument"
"read_only_calendar"
"internal_error"
"timeout"
"desktop_disconnected"
```

#### 5.5.5 TCC permission status（5 项；spec §5.2 `calendar.check_permission` 同表）

```
"not_determined"
"restricted"
"denied"
"granted"
"write_only"
```

#### 5.5.6 日历账户类型（7 项；spec §5.2 `calendar.list_sources` `account_type`）

```
"icloud"
"google"
"exchange"
"outlook"
"local"
"subscription"
"other"
```

#### 5.5.7 Attendee 参会状态（4 项；spec §5.2 `calendar.list_events` `attendees[].status`）

```
"accepted"
"tentative"
"declined"
"needs_action"
```

#### 5.5.8 Event scope（3 项；delete vs update 接受集不同）

| 值 | `calendar.delete_event` | `calendar.update_event` |
|---|---|---|
| `"this"` | ✓ | ✓ |
| `"this_and_future"` | ✓ | ✓ |
| `"all"` | ✓ | ✗（spec §5.2 显式禁用，Desktop 收到 → 返 `invalid_argument`） |

#### 5.5.9 Desktop event 类型（4 项；spec §3.5）

```
"desktop_online"              ← Desktop 推
"desktop_offline"             ← Daemon 内部构造（不在 wire 上）
"calendar_permission_changed" ← Desktop 推
"calendar_data_changed"       ← Desktop 推（可选实现）
```

#### 5.5.10 Recurrence 频率（4 项；spec §5.2 `calendar.get_event` `recurrence_rule.frequency`）

```
"daily"
"weekly"
"monthly"
"yearly"
```

#### 5.5.11 Deep link URL

scheme 名 v1 待定（Desktop 实际注册的可能是 `shanclaw://` 或别的，见 §7.2 Q4）。本节 host + path 钉死：

```
<scheme>://settings/permissions/calendar
<scheme>://settings/permissions/reminders   ← v2
<scheme>://settings/permissions/contacts    ← v2
```

#### 5.5.12 实施纪律

- **Daemon Go 端**：所有上述字符串集中在 `internal/daemon/desktop_rpc/types.go` 作为 `const` / `var` 导出，按本节顺序排列；其他模块（tools / dispatcher / register.go）一律 `import` 这些常量，**禁止 inline 字面字符串**
- **Desktop Swift 端**：所有上述字符串集中在 `Packages/ShanClawBridge/Sources/ShanClawBridge/DesktopRPC/ProtocolConstants.swift`（新文件）作为 `enum ProtocolConstants` 下的 `static let` 常量；dispatcher / mappers / UI 一律 import，**禁止 inline 字面字符串**
- **Phase 1 检查点**：两端各跑一次 `grep -r '"calendar\.\|"desktop_rpc_\|"calendar_permission_'` 确保 inline 字符串数量 = 0；任何 grep 命中都要替换为常量引用
- **本节就是契约**：实施期间发现需要新增字符串（v1.x 加新方法 / 错误码 / enum 值）→ 先 PR 本 spec §5.5，merge 后两端再 commit 代码

---

## 六、阶段与里程碑

### Phase 0 — 可行性 PoC（Desktop 团队，1-2 天）

**目标**：验证 TCC 流程能跑通，授权持久化生效。**这一阶段不写任何 RPC 代码**，只在 Desktop 内部验证 EventKit 接入 + 签名链路。

**实现内容**：
- Desktop `Info.plist` 加 `NSCalendarsFullAccessUsageDescription`
- 加一个临时 menubar / 设置页按钮，调 `requestFullAccessToEvents`（项目 min target macOS 15+，无 fallback 需求）
- 第二个按钮：拉未来 7 天事件，console.log 输出 `title / start / end / calendar.title`

**验收清单（全部 ✅ 才能进入 Phase 1）**：

- [ ] 首次点授权按钮时，macOS 系统对话框正确弹出，显示 `NSCalendarsFullAccessUsageDescription` 的文案
- [ ] 用户点"允许"后，能 list 出未来 7 天的事件
- [ ] **重启 Desktop 后授权不丢**（无需重新弹框；如果丢了 → Notarization / Hardened Runtime 配置有问题）
- [ ] **重启 Mac 后授权不丢**
- [ ] Desktop 升级（替换 `.app` 但保持 Bundle ID + Team ID 不变）后授权不丢
- [ ] 用 `tccutil reset Calendar <bundle_id>` 清掉权限后，下次点按钮能重新弹框（验证 TCC 数据库索引正确）
- [ ] 在「系统设置 → 互联网账户」里**同时配置 iCloud + Google + 一个 Exchange / Office 365 账户**，每个的"日历"开关都打开 → list 出来的事件能看到三个源都有数据（`EKCalendar.source.title` 分别能识别）
- [ ] 给 Google 日历加一个新事件（手机或 web 端），等 30s 让 CalendarAgent 同步，重新 list → 能看到新事件
- [ ] 把 Bundle ID 改一下重打包（这是反例验证）→ 应该需要重新授权（验证 TCC 确实按 Bundle ID 索引）

Phase 0 PoC 通不过的话**所有下游开发都白搭**，必须先解决签名链路问题再继续。

### Phase 1 — RPC 通道（Daemon + Desktop 并行，3-5 天）

- Daemon: `DesktopRPCBroker`（复刻 ApprovalBroker 模式）+ Unix socket listener（监听 `--rpc-socket` CLI flag 传入的 path，启动顺序：`os.Remove` → `Listen` → `Chmod 0600` → atomic 写 pidfile）+ length-prefixed JSON 帧路由（三种 type）+ `internal/daemon/desktop_rpc/codec.go`（≤ 4 MB body）+ `internal/tools/register.go` conditional registration
- Daemon: 暴露现有 `internal/daemon/pidfile.go` 供 `desktop_rpc` 子包复用
- Desktop: `DaemonManager` 改为 spawn daemon 时传 `--rpc-socket <path>` + sock client（unix endpoint connect + 重连退避）+ `DesktopRPCService` 帧路由 + §4.1.1 reconciliation 流程实现（pidfile 读取 / `kill(pid, 0)` 探活 / SIGTERM via Darwin syscall / `system.capabilities` 版本协商 / SIGTERM 5s 超时升级 SIGKILL）+ reconciliation 完成后才推 `desktop_event { event: "desktop_online" }`
- 两边对 `system.ping` 做 e2e 联调；`system.capabilities` 通过 reconciliation 流程自动验证（不用单独联调）

### Phase 2 — 日历读路径（4-5 天）

- Desktop: `CalendarProvider` 实现 `list_sources / list_events / get_event / check_permission / request_permission`
- Daemon: `calendar_list_sources / calendar_list_events / calendar_get_event / calendar_check_permission / calendar_request_permission` 五个工具
- Desktop 端首次授权 UX
- E2E 测试：Slack 里问"我下午有什么会"，能拿到 Google / Exchange / iCloud 多源数据

### Phase 3 — 日历写路径（4-5 天）

- Desktop: `create_event / update_event / delete_event` + 重复事件 scope 处理（注意 `this_and_future` 拆 series 行为，见 §5.2）
- Daemon: 对应三个工具，走 approval 链 + `description` 字段强制
- always-allow 持久化测试
- E2E：让 Agent 自动创建事件、改时间、取消事件；验证 result 里 `invitations_sent: false` 模型能正确传递给用户（v1 不自动发邀请，见 §3.3）

### Phase 4 — 文档与发布（1-2 天）

- 同步 CLAUDE.md / README / AGENT.md / kocoro skill references
- 灰度发布到 internal channel

**总计**：约 13-19 工作日（Desktop 和 Daemon 大致 50/50）。

### Cross-team Convergence Checkpoints（v0.5.1 新增）

每个 phase 完成时，**Daemon team 和 Desktop team 必须共同验证以下场景**才能进入下一 phase。两边各自单元测试通过 ≠ 集成就绪 —— convergence checkpoint 才算 phase 真正完成。

#### Checkpoint @ End of Phase 1（RPC 通道联调）

**目标**：通道双向通 + reconciliation 完整路径走通。**用 `nc` + 手写 JSON 也能验证，无需 agent loop**。

测试 case：
1. Desktop 用 `--rpc-socket` + `--rpc-pidfile` 两条 path 启动 Daemon
2. Daemon Listen 成功 + 写 sock 0600 + atomic 写 pidfile + accept loop running
3. Desktop reconciliation：读 pidfile（首次为空跳过）→ connect sock → 发 `system.capabilities` REQUEST 帧
4. Daemon 解码帧 → 路由到 daemonMethods → 返 `system.capabilities` RESULT（含 `version: "1.0.0"` + 完整 ProtocolMethods 数组 + platform.app_version = shan binary version）
5. Desktop 比对 version 匹配 → 发 `desktop_event { event: "desktop_online" }` 帧
6. Daemon EventBus 收到 desktop_online 事件（log 看见即可）
7. 双向 ping：Daemon 发 `system.ping {echo: "from-daemon"}` REQUEST → Desktop 返 RESULT；Desktop 发 `system.ping {echo: "from-desktop"}` → Daemon 返 RESULT
8. 模拟版本漂移：手动 kill Daemon 进程，nc 起一个假的"老 daemon"返 `version: "0.9.0"` → 验证 Desktop 走 SIGTERM ladder（5s SIGTERM + 2s SIGKILL + user error 如果都失败）
9. 模拟 sock listen 失败：先占用 sock 文件，启动 Daemon → 验证非 0 退出 + stderr 报错 + Desktop DaemonManager 捕获弹用户可见错误（不静默重试）

#### Checkpoint @ End of Phase 2（读路径 e2e）

**目标**：calendar 读路径 + 多源数据汇聚 + TCC 状态正确传播

测试 case（在已经走通 §4.1.1 reconciliation 的真 Desktop + 真 Daemon 上）：
1. macOS 系统设置 → 互联网账户配置 iCloud + Google + 一个 Exchange / Office 365
2. 触发 Cloud channel（Slack）发"今天有什么会"
3. 期望：calendar_list_events RPC 走通，返多源事件（每个事件 `calendar_id` 可识别属于哪个源）
4. 时区正确（事件 start/end 跟 Calendar.app 看到的字面一致）
5. `limit: 2000` 触发时 result 含 `truncated: true`
6. TCC denied 状态下返 `calendar_permission_denied` 错误（不是空数组）
7. Desktop 收到 `EKEventStoreChanged` Notification → 推 `desktop_event { calendar_data_changed }` → Daemon EventBus 收到（log 看见）

#### Checkpoint @ End of Phase 3（写路径 e2e + approval + 重复事件）

**目标**：完整 write 流程 + approval 链路 + 重复事件 scope 三态

测试 case：
1. Slack 发"明天 3 点和 Alice 半小时会议" → `calendar_create_event` → daemon approval card 弹出（`description` 字段非空）→ 用户在 Desktop 上批准 → Desktop EventKit save → result 返 `invitations_sent: false`
2. always-allow 持久化：用户点"始终允许" → 第二次同样的创建不弹 card
3. 创建周重复事件 → "改下周三这次的时间" 走 `update_event scope: "this"` → 原 series 不变，本次实例 detached
4. "改下周三这次及之后所有" 走 `update_event scope: "this_and_future"` → ⚠️ result 返**新 id**（series 拆分），调用方以新 id 为准
5. 模拟错误使用：`update_event scope: "all"` → 返 `invalid_argument`（spec §5.5.8 禁用）
6. 写入只读日历（Birthdays / 订阅）→ 返 `read_only_calendar` 错误
7. `calendar_request_permission` 超时验证：用户挂着不点 → Daemon 等满 5 分钟超时后返 timeout（而不是 30s）

#### Checkpoint @ End of Phase 4（发布就绪）

- 两边 CLAUDE.md / README / AGENT.md / kocoro skill references 一致提及
- 两边 commit message 互相 cross-reference 主 PR
- 灰度 internal channel 至少 1 周无 crash / 数据错乱

---

### Phase 5+（v2）

按 framework 形态分两组推进，建议先做 A 组（顺着 v1 架构惯性，工作量小），再做 B 组（需要 Desktop 端额外的 AppleScript 包装层）：

**A 组 — EventKit / Contacts framework**（沿用本方案 RPC 架构，每项约 3-5 天）
- Reminders（EventKit 同 store，复用授权流，主要工作在 Daemon 端的 `reminder_*` 工具）
- Contacts（CNContactStore，独立 TCC 申请 UX）
- Free-busy 查询（attendee 多人聚合，需要先把 Contacts 接好再做）

**B 组 — AppleScript 包装**（Desktop 端新做 NSAppleScript 包装层，每项约 4-6 天）
- 邮件（Mail.app —— 从 daemon `applescript` 工具过渡到 Desktop RPC，UX 收益 > 能力收益）
- Teams / Zoom / Meet 加入链接识别（轻量，可能并入 Calendar v1.x patch）

**文档重命名**：v2 启动时把 `desktop-calendar-rpc.md` 升级为 `desktop-pim-rpc.md`，方法命名空间扩展为 `calendar.*` / `reminders.*` / `contacts.*` / `mail.*`。

---

## 七、风险与开放问题

### 7.1 已识别风险

| 风险 | 缓解 |
|---|---|
| Desktop 签名 / Bundle ID 变更导致用户授权全部失效 | v1 上线前冻结这两项；之后改动走 ADR 并提前通告用户 |
| EventKit API 跨 macOS 版本差异 | 已 N/A：项目 min target macOS 15+，统一走 `requestFullAccessToEvents` / `.fullAccess` / `.writeOnly` 一套 API |
| CalendarAgent 同步延迟造成"刚写完查不到" | `pending_remote_sync` 字段 + Daemon 工具不做自动二次校验 |
| 用户没把账号加到「互联网账户」 | v1 不支持；v2 评估是否做内置 OAuth fallback |
| 写操作 + always-allow 后被滥用 | 沿用现有 approval policy，必要时把 `calendar_create_event` 加入 `agent.DisallowsAutoApproval` 永远不可持久化 |
| **sock 文件被第三方占用 / 文件系统异常导致 daemon 无法 listen** | daemon 立即非 0 退出 + stderr 报错（"failed to listen on socket `<path>`: `<error>`. Another daemon may be running, or stale file system state. Manual cleanup: `rm <path>` and restart Kocoro Desktop."）+ Desktop 的 `DaemonManager` 捕获非 0 退出后弹用户可见错误，**不静默重试** |
| **半绑定 lifecycle 导致 daemon 版本漂移**（老 daemon 孤儿化继续跑，新 Desktop 启动时遇到旧 daemon） | §4.1.1 reconciliation 流程：pidfile + `system.capabilities` 版本协商，不匹配走 SIGTERM → respawn 路径 |
| **同 Mac 多 Desktop 实例并发启动** | v1 假设单 Desktop 实例。同账户 LaunchServices 默认复用现有实例；多账户共享 Mac 各自 `~/Library/...` 天然隔离，不冲突 |
| **pidfile 指向的 PID 被复用给其它进程** | 极罕见 wrap-around 场景。reconciliation 步骤 2a connect sock 会失败（其它进程不监听该 sock）→ 自然 fallback 到 cleanup + respawn |

### 7.2 待 Desktop 团队确认

1. Desktop 当前是否已经持有稳定 Bundle ID + Team ID？v1 期间能冻结吗？实际 Bundle ID 为 `run.shannon.shanclaw`（来自 Kocoro Desktop CLAUDE.md "do not change"），不再用文档早期示例 `ai.kocoro.desktop`
2. ~~Desktop 目前是否已经实现连接 Daemon 的 IPC client？如果有，是 WS 还是 SSE？~~ → v0.5 已落实：Desktop 现有 HTTP+SSE 通道（Desktop→Daemon 业务请求方向）保留不动；v0.5 新增 Unix domain socket 通道仅承载 daemon→Desktop 的反向 RPC（见 §4.1）。两条通道并存，v1 不合并
3. Desktop 设置页有没有现成的"权限管理"入口可以挂日历授权？还是要新做一个 panel？
4. Desktop 是否已经注册了 `kocoro://` URL scheme？v1 需要响应 `kocoro://settings/permissions/calendar` 这类 deep link（见 §3.4）。注意 Bundle ID `run.shannon.shanclaw` 实际注册的 scheme 可能是 `shanclaw://` 或其他，需 Desktop 团队回填实际 scheme 名
5. ~~macOS 最低支持版本？~~ → 已确认 macOS 15+（Kocoro Desktop CLAUDE.md `macOS 15+ and iOS 18+`）。`requestFullAccessToEvents` 在 macOS 14+ 可用，本项目 always-on，文档已删去 pre-14 fallback 代码
6. **新增**：Desktop 端 `DaemonManager` 是否能改造为 spawn daemon 时传 `--rpc-socket` flag？现有 `launchProcess()` 实现已经是 `Process()` + `proc.arguments = args`，加一个 flag 是单点改动。请确认

### 7.3 待 Daemon 团队确认

1. `DesktopRPCBroker` 直接复刻 ApprovalBroker 还是先做泛型抽象？倾向前者（YAGNI）。
2. 写操作 approval 的 `description` 字段强制要求模型生成（参考 CLAUDE.md 里 approval card description 规约）。
3. `RegisterLocalTools` 中 calendar 工具的 conditional registration 入口点确认（依据 §4.3 改为启动时按 `if rpcBroker != nil` 注册，与 `session_search` / `cloud_delegate` 同模式；v0.5 已放弃 v0.4 设计过的 `loadedToolsForRequest` 运行时 filter 思路，不再有 runtime filter 一说）。

### 7.4 待产品确认

1. v1 上线时是否需要在 Kocoro Desktop 设置里给用户一个"日历能力"总开关，让 privacy-conscious 用户可以一键关闭？
2. 写操作的 audit log 是否需要在 Desktop 端也展示历史（"Kocoro 在 5/26 14:00 创建了事件 …"）？

---

## 八、附录

### 8.1 参考

- Apple EventKit 文档：https://developer.apple.com/documentation/eventkit
- macOS 14 Calendar 权限分级：https://developer.apple.com/documentation/eventkit/ekauthorizationstatus
- 既有 IPC 模式参考代码：
  - `internal/daemon/approval.go` (ApprovalBroker)
  - `internal/daemon/skill_filter.go` (能力过滤模式)
  - `internal/daemon/permissions_darwin.go` (TCC 状态探测)
  - `internal/daemon/types.go` (信封定义)

### 8.2 变更记录

| 日期 | 版本 | 变更 |
|---|---|---|
| 2026-05-26 | v0.1 | 初稿 |
| 2026-05-26 | v0.2 | 厘清「互联网账户」≠ 统一出口；明确 v1 只覆盖 framework 类（日历），邮件 v1 走 daemon `applescript` 工具，v2 再并入 RPC；备忘录列为永久非目标 |
| 2026-05-26 | v0.3 | 协作向 Review：补 reader guide、RPC 端点/端口发现、result_url + result_token、source 字段、RFC 3339 时间格式、TCC 状态映射表、update_event patch 语义、scope=this_and_future 拆 series 警告、series_master_id 字段、attendees 不发邀请坑、all-day end 边界、enum 命名归一、并发与序列化、system.ping/capabilities、断连取消语义、deep link 约定、Phase 0 验收清单 |
| 2026-05-26 | v0.4 | 二轮 Review 修不一致：①§1.1/§1.3 attendee 邀请承诺与 §3.3 自相矛盾 → 改为"元数据写入，邀请不自动发"；②§3.4 流程里 `calendar.permission_status` / `[calendar_permission_required]` 是过时方法名/错误码 → 改为 `calendar.check_permission` / `calendar_permission_not_determined`；③§3.5 "Desktop 发 SSE 事件" 方向错（Desktop 是客户端） → 改为通过双向 WS 反向通道；④把 v0.3 的 HTTP POST 回执 + result_url + result_token 整套去掉，统一改为**双向 WebSocket**（更简、不用端口发现、不用 token、信任模型与现有 localhost-only 端点一致）；⑤§5.1 重写为三种 WS 帧（request/result/event）+ ping/pong + 帧大小约束；⑥§5.2 patch 补 start/end 不可清空 + calendar_id 跨日历搬迁；⑦说明为什么 update_event 不对称没有 scope=all；⑧合并 §4.2 调试小节到 §4.1；⑨§7.2 Desktop 团队问题清单加 `kocoro://` URL scheme 注册项 |
| 2026-05-26 | v0.5 | Desktop CC 提议 + Daemon CC review + Desktop CC followup + Daemon CC final ack 四轮往返后并入主文档。**核心变更**：①§1.2 表格把 daemon 直调 EventKit 否决理由从单行"裸 Go 二进制没 bundle"扩为两条 Apple 平台硬约束（`LSBackgroundOnly` 阻止 TCC 弹框 + TCC "responsible code" 归因到父 app），并补 macOS 26 Tahoe release notes 未变；Sidecar 受同样约束改为不可行。②**传输层从 v0.4 的 TCP+WebSocket 改为 Unix domain socket**（信任边界对齐 TCC：0600 文件权限 + 0700 父目录，避免本机任意进程冒充 Desktop 喂假日历），砍掉端口发现（`~/.shannon/daemon.port` + `KOCORO_DAEMON_PORT` env）和 WebSocket 协议开销，改用 length-prefixed JSON framing（≤ 4 MB body）。③显式部署前提：daemon 是 Desktop 子进程（`DaemonManager.Process()` spawn），launchd 路径仅服务 npm CLI 独立安装；Desktop UI 退出时不主动 kill daemon，被 launchd 收养继续跑（半绑定模型，Slack 等 cloud channel 仍能触发）。④**新增 §4.1.1 reconciliation 流程**应对半绑定下的版本漂移：Desktop 启动时通过 pidfile + `system.capabilities` 协商版本，不匹配则 SIGTERM 老 daemon（5s 后升级 SIGKILL，2s 仍不退则用户可见错误，不静默继续）后 respawn。⑤§4.3 把"runtime 工具过滤"改为"startup conditional registration"——TUI/one-shot/MCP/scheduled 等非 daemon 模式根本不注册 calendar 工具，用户在这些模式下走 `applescript` + Calendar.app fallback。⑥§5.2 `system.capabilities` 从 v0.4 的运行时 tool filter 用途升级为 reconciliation 必备版本协商方法 + 诊断用，保留 `methods` 字段（半绑定下版本漂移是必然事件而非理论可能，diff 信息丰富 fail-fast 诊断）。⑦§7.1 风险表加 sock listen 失败兜底（非 0 退出 + 用户可见错误，禁止静默重试）/ version mismatch reconciliation / multi-instance scope / PID wrap-around 四条。⑧延后未并入本版的三项独立改进：`desktop_rpc_cancel` 帧（架构图占位 schema 待定）、`calendar.request_permission` 异步化（与 `timeout_ms` 冲突）、attendees v1 走 AppleScript-Calendar 真实发邀请——按 cancel > request_permission > attendees 顺序在 v1.x patch 中陆续解决。 |
| 2026-05-26 | v0.5.1 | Daemon Plan + Desktop Plan 对齐 review 后做 8 项 spec 修正，全部为契约层澄清，不改动 v0.5 已确立的架构：①§4.1 sock + pidfile 改成**两个独立 CLI flag**（`--rpc-socket` + `--rpc-pidfile`），避免 Daemon 派生 path 与 Desktop 硬编码 path 隐式耦合；②§5.2 `system.capabilities` 修正 v0.5 的"daemon → desktop"方向笔误 —— reconciliation 主流程是 **Desktop → Daemon**，诊断场景才是反向；③§5.2 `platform.app_version` 字段明确两端 responder 各填什么（Desktop = bundle CFBundleShortVersionString，Daemon = shan binary version）；④§5.2 `methods` 数组语义钉死为"协议版本支持的所有方法"，两端响应返 byte-identical 数组（不是"本端 implement 的方法"）；⑤**新增 §5.5 正式标识符清单**作为两端契约 single source of truth —— 10 个 method 名 / 8 个错误码 / 5 个 TCC 状态 / 7 个账户类型 / 4 个 attendee 状态 / 3 个 scope / 4 个 desktop event 类型 / 4 个 recurrence 频率 / 3 个 deep link path 全集中本节，两端 grep 校对避免拼写漂移；⑥§6 加 cross-team Convergence Checkpoints，每个 phase 完成时两边必须共同验证的场景（Phase 1 双向 ping + 版本漂移 + sock listen 失败；Phase 2 多源数据 + 时区 + TCC denied；Phase 3 approval 链 + 重复事件 + scope 三态 + 5min request_permission timeout）；⑦**revert `invitations_sent` 字段为 always-present**（v0.5 阶段曾被改成"无 attendees 时省略"，与示例 schema + §1.3 + Phase 3 验收清单等多处矛盾）：§3.3 + §5.2 create_event 字段语义段 + §5.2 update_event 字段语义行三处统一回 always-present，v1 永远返 `false`；⑧§5.2 `calendar.request_permission` 加 timeout 缓解方案（v1 不等 v1.x 异步化）—— Daemon 端在 envelope 里单独覆盖 `timeout_ms = 5 * 60 * 1000`，Desktop 端配合不再加短超时。 |
