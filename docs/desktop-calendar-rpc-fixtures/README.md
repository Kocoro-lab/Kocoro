# Calendar RPC v1 — 共享 JSON Fixtures

## 用途

两端（Daemon Go / Desktop Swift）的 codec 单测 / round-trip 测试 / 联调期间 wire-level 验证都对这份目录里的 JSON fixture 做匹配。**任何字段名 / 类型 / 嵌套结构变更都先改这里，再两端同步更新代码**。

避免的问题：手写 JSON 时把 `series_master_id` 拼成 `series_master_event_id`，两端单测各自跑通但集成时炸。fixture 一处错 → 两端单测都 fail → 早发现。

## 内容

每个文件是**一个完整的 Unix sock 帧的 JSON 体**（length prefix 4 字节 big-endian uint32 由 codec 加，不在文件里）。文件名约定：

```
<frame-type>.<method-or-event-name>.<request|result|err>.json
```

- `<frame-type>` ∈ `desktop_rpc_request` / `desktop_rpc_result` / `desktop_event`
- 帧 type 为 request 的文件含 `.request.json` 或 `.result.json`（成对）
- 帧 type 为 result 含 `err` 时是错误形态（`.err.json`）
- 帧 type 为 event 的文件直接以 event name 命名

## 文件清单（v0.5.1 起）

### system.* 方法（reconciliation 必需）

| 文件 | 描述 |
|---|---|
| `desktop_rpc_request.system_ping.request.json` | echo "hello" |
| `desktop_rpc_result.system_ping.result.json` | pong + server_time |
| `desktop_rpc_request.system_capabilities.request.json` | 空 params |
| `desktop_rpc_result.system_capabilities.result.json` | 完整 ProtocolMethods + platform |

### calendar 读路径

| 文件 | 描述 |
|---|---|
| `desktop_rpc_request.calendar_list_events.request.json` | 一天时间窗 + null calendar_ids + limit 500 |
| `desktop_rpc_result.calendar_list_events.result.json` | 2 个 event（一个普通 + 一个 recurring instance 带 series_master_id） + truncated:false |

### calendar 写路径

| 文件 | 描述 |
|---|---|
| `desktop_rpc_request.calendar_create_event.request.json` | 含 attendees + alarms + recurrence_rule（weekly） |
| `desktop_rpc_result.calendar_create_event.result.json` | id + pending_remote_sync + invitations_sent:false |
| `desktop_rpc_request.calendar_update_event.request.json` | scope:this + patch + clear_recurrence:false |
| `desktop_rpc_result.calendar_update_event.result.json` | scope:this_and_future 时 id 可能变（demo） |

### 错误形态

| 文件 | 描述 |
|---|---|
| `desktop_rpc_result.calendar_list_events.err.json` | calendar_permission_denied + details.status:"denied" |

### Desktop 推 event

| 文件 | 描述 |
|---|---|
| `desktop_event.desktop_online.json` | reconciliation 完成后第一帧 |
| `desktop_event.calendar_permission_changed.json` | TCC 翻转 |

## 验证脚本（建议）

```bash
# Daemon Go 端单测
go test ./internal/daemon/desktop_rpc -run TestFixtureRoundTrip

# Desktop Swift 端单测
swift test --filter DesktopRPCFixtureTests
```

两端单测应该都做：read fixture → unmarshal → marshal → 与原文件 **semantically equal**（重新 parse 成 struct/dict 后比对字段值），**不要 byte-equal**。

⚠️ **为什么不能 byte-equal**：JSON map 序列化时 key 顺序在多语言间不保证一致（Go 的 `encoding/json` 按结构体字段定义顺序，但 `map[string]any` 序列化顺序随机；Swift Codable 按 struct 定义顺序）。还有 Daemon 端的 `stripDescription` 写工具内部走 `map[string]json.RawMessage` 再 Marshal，**会重排 key**。byte-equal 比对会因此 fail，但语义完全一致。

具体做法（伪代码）：
```
data = readFixture("calendar.list_events.request.json")
struct = decode(data)
reencoded = encode(struct)
decoded_back = decode(reencoded)
assertDeepEqual(struct, decoded_back)   // ✓ semantic
// assertEqual(data, reencoded)         // ✗ may fail on key reorder
```

## 变更规则

- v1.x 加新方法 → 加一对新 fixture 文件，更新本 README 文件清单
- 字段重命名 / 类型变更 → 同时改 spec §5 + 本目录所有相关 fixture + 两端常量
- **不要绕过 fixture 直接改两端代码** —— 那是 calendar RPC v0.5.1 协议契约约定要避免的
