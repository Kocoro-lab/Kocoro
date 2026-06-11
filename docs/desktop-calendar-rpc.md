# Kocoro Calendar Capability — Requirements & Desktop-Side Technical Design

**Status**: Draft v0.5.1
**Author**: chen.zhai
**Date**: 2026-05-26
**Audience**: Kocoro Desktop team / Daemon team

### Reader's Guide

| Role | Key sections |
|---|---|
| **Desktop implementer** | §1 (background & scope) → §3 (Desktop-side design) → §5 (RPC protocol) → §7.2 (open items for Desktop) → §6 Phase 0 acceptance |
| **Daemon implementer** | §1 → §2 (architecture) → §4 (Daemon-side contract surface) → §5 → §7.3 (open items for Daemon) |
| **Product / Review** | §1 → §6 (milestones) → §7 (risks & open questions) |

Paragraphs marked ⚠️ are concrete gotchas that will bite (EventKit behavior traps / time zones / TCC boundaries); read them before implementing.

---

## 1. Product Requirements

### 1.1 Background

Users want the Kocoro Agent to operate the macOS Calendar (including iCloud / Google / Microsoft 365 / Exchange / Outlook / Teams meetings) in order to:

- Query schedules, find meeting invitations, read Teams join URLs
- Create / modify / cancel events, add attendee metadata, set reminders
- Let the Agent proactively report today's meetings

> ⚠️ One pre-emptive warning: in v1, writing the attendees field is a "metadata write" under EventKit's constraints — **Google / Exchange backends will not auto-send invitations**; see §3.3. "Opening a Teams join URL" requires recognizing the URL and handing it to the `browser` tool, which v1 does not do (planned for v2, see §6 Phase 5+).

Our core constraints are **local-first** and **no per-vendor OAuth**. Leveraging the macOS system layer (`System Settings → Internet Accounts` already syncs calendars into the local EventKit database) as an established fact is the most economical path.

### 1.2 Technical Decision

| Option | Conclusion |
|---|---|
| Build our own Google/MS OAuth + Graph API | ✗ Reinventing the wheel; high per-vendor maintenance cost |
| Daemon (wrapped in a `.app`) calls EventKit directly via cgo | ✗ Two independent hard blockers: ① `Kocoro Engine.app`'s existing `LSBackgroundOnly = true` prevents the TCC dialog (`requestFullAccessToEvents` silently returns `.denied`, no error log, UX equivalent to "user denied"); ② TCC "responsible code" attribution walks up the bundle hierarchy to the nearest user-visible ancestor, so Calendar authorization attributes to the parent app `Kocoro Desktop` rather than `Kocoro Engine` — AppleEvents/Accessibility can be granted independently because those two take a special attribution path (client binary code-signing identifier / per-binary grant), while EventKit/Contacts/Reminders take the regular TCC path, which does not apply. Confirmed unchanged in macOS 15 and macOS 26 Tahoe release notes |
| Sidecar (`cal_server`, like `ax_server`) | ✗ Subject to the same LSBackgroundOnly + responsible-code constraints, equally infeasible |
| **Desktop calls EventKit; Daemon calls Desktop via Unix domain socket RPC** | ✓ **This design** |

**Why this path**: Kocoro Desktop is itself a native macOS App — already code-signed + notarized, already holding the user's other TCC grants (screen capture / notifications); and EventKit is a first-class Swift/ObjC citizen. Having Desktop own EventKit access, with the Daemon calling Desktop's exposed methods through a new **Unix domain socket** reverse-RPC channel (see §4.1), is the smallest-delta design and the one most consistent with macOS's design intent.

> The v0.4 "bidirectional WebSocket on `127.0.0.1`" design was changed to a Unix socket in v0.5. The reason is asymmetric data flow: the localhost TCP trust model holds for **receiving commands** on the existing endpoints (`/approval`, `/health`), but not for **acting as a data egress that pushes Desktop-side TCC state and calendar data** — any local userland process can connect to the same port to impersonate Desktop and feed fake data / monitor TCC events. A Unix socket + 0600 file permission + 0700 parent directory shrinks the trust boundary to "processes that can read the current user's `~/Library`", on par with TCC.

### 1.3 Scope

> ⚠️ Key judgment: "Internet Accounts" is a configuration entry point, **not a unified data egress**. Calendar / Reminders / Contacts go through the EventKit / Contacts framework (Apple first-class PIM data), while Mail / Notes have no public framework and can only be reached via AppleScript. This design's v1 covers only the framework category; v2 brings the AppleScript category in.

**v1 (required) — Calendar**

- Read: list calendar sources, list events within a given time window, get event details by ID
- Write: create, modify, delete events; **the attendees field is written as metadata, invitations are not auto-sent** (Google/Exchange limitation, see §3.3); `invitations_sent: false` in the result tells the caller
- Instance-level deletion / modification of recurring events (this / this_and_future / all)
- TCC authorization status query and request triggering
- Daemon ↔ Desktop online/offline state negotiation

**v2 (later, split into two groups by framework form)**

A. *EventKit / Contacts framework category, reusing this design's RPC architecture*:
- Reminders (EventKit, same store, separate TCC category `NSRemindersFullAccessUsageDescription`)
- Contacts (Contacts framework, separate TCC `NSContactsUsageDescription`)
- Free/busy lookup (multi-attendee free/busy aggregation)

B. *AppleScript-wrapped category, where Desktop wraps an RPC layer with NSAppleScript*:
- Mail (Mail.app, no public framework, via AppleScript; structured output + caching + unified permission UX provided by Desktop, but the underlying mechanism is still scripting)
- Smart recognition of Teams / Zoom / Meet join links, handed to the `browser` tool

For the v2 phase, consider renaming/extending this file to `docs/desktop-pim-rpc.md`, with parallel method namespaces `calendar.*` / `reminders.*` / `contacts.*` / `mail.*`.

### 1.4 Non-goals

- ❌ Directly reading the OAuth tokens the user configured in "Internet Accounts" (Apple's privacy boundary — can't and shouldn't)
- ❌ The Teams desktop client's chat/call status (not within the Internet Accounts system)
- ❌ **Notes** — no public framework, the Notes.app AppleScript dictionary is incomplete (search API is incomplete, rich text is essentially unreadable), local SQLite is encrypted. Neither v1 nor v2 will do it; if there's strong demand, separately evaluate the workaround of "screenshot + OCR + iCloud.com/notes browser access"
- ❌ Calendar sources the user has not added in "Internet Accounts" (e.g. a user who only installed the Google Calendar web version) — v1 does not support this case
- ❌ **The v1 phase does not do Mail through this RPC channel** — mail needs are temporarily handled by the daemon's existing `applescript` tool + the Mail.app AppleScript dictionary (zero code, automatically covers all mailboxes in "Internet Accounts"). Rationale: mail can only go through AppleScript at the bottom, with no framework dividend to reap, so there's no reason to cram it into v1 and delay the calendar launch. Merging it in v2 is for UX unification (permission management panel, structured output), not a capability gain

---

## 2. Overall Architecture

### 2.1 Process Boundaries & Responsibilities

```
┌──────────────────────────────────────────────┐
│  Agent Loop (Daemon, Go)                       │
│   - calendar_* tools (daemon mode only)        │
│   - DesktopRPCBroker (reuses ApprovalBroker)   │
│   - permission/approval/audit pipeline         │
└────────┬───────────────────────────────────────┘
         │   Unix domain socket, permission 0600 (only current UID can read/write)
         │   path:    ~/Library/Application Support/run.shannon.shanclaw/daemon.sock
         │   pidfile: ~/Library/Application Support/run.shannon.shanclaw/daemon.pid
         │   transport framing: length-prefixed JSON (4-byte big-endian uint32 + body, see §5.1)
         │
         │   ① frame: desktop_rpc_request    (daemon → desktop)
         │   ② frame: desktop_rpc_result     (desktop → daemon)
         │   ③ frame: desktop_event          (desktop → daemon, async notification)
         │   ④ frame: desktop_rpc_cancel     (daemon → desktop, cancel in-flight RPC; placeholder, schema finalized in v1.x)
         ↓
┌──────────────────────────────────────────────┐
│  Kocoro Desktop (.app, Swift)                  │
│   - DesktopRPCService routing                  │
│   - CalendarProvider (EKEventStore)            │
│   - TCC first-time authorization UX            │
│   - daemon process supervision (DaemonManager) │
└──────────────────────────────────────────────┘
```

> Deployment premise (made explicit in v0.5): the daemon is a child process of Desktop (`DaemonManager` spawns `Helpers/ShanClaw Engine.app/Contents/MacOS/shan` via `Process()`, passing `--rpc-socket <path>`), with its version bundled with the Desktop release. When the Desktop UI quits it does not actively kill the daemon — the daemon is adopted by launchd and keeps running (PPID → 1), the sock file remains, and cloud channels like Slack/LINE can still trigger it. On Desktop's next launch it decides reuse vs respawn per the §4.1.1 reconciliation flow. `internal/daemon/launchd_darwin.go` serves only the npm CLI standalone install path and is not in scope for this design's v1.

### 2.2 Key Nodes in the Call Chain

1. **Model decides to call `calendar_list_events`** — Daemon-side tool entry
2. **Daemon permission/approval pipeline** — `RequiresApproval` decision (read = no, write = yes); write operations go through `ApprovalBroker` to request user approval
3. **Daemon pushes `desktop_rpc_request` over the Unix socket** — `DesktopRPCBroker.Request(method, params)` blocks and waits
4. **Desktop receives the frame** — routes to `CalendarProvider.handle(method, params)`
5. **Desktop calls EventKit** — `EKEventStore.events(matching:)` or `.save(_:span:)`
6. **Desktop pushes `desktop_rpc_result` over the same sock** — contains `request_id` + `result` or `error`
7. **Daemon Broker unblocks by `request_id`** — the tool returns a structured result to the model

### 2.3 Process Lifecycle & Availability

**Premise**: the daemon is spawned by Desktop (see the §2.1 deployment premise). Registration of the calendar tools is **startup-time conditional registration** (see §4.3) — only daemon mode (spawned by Desktop and holding a `DesktopRPCBroker`) registers the `calendar_*` tools; other run modes such as TUI / one-shot / MCP / scheduled simply never register them, so there's no "tool is in the list but can't be called" situation.

| Situation | Behavior |
|---|---|
| Desktop starts, daemon first spawn | Desktop detects no leftover daemon via the §4.1.1 reconciliation flow → spawns a new daemon → daemon listens on sock + writes pidfile → Desktop connects sock → sends `system.capabilities` to negotiate version → sends `desktop_event` reporting online. Calendar tools available normally |
| Desktop starts, leftover daemon exists (not killed after last UI quit, adopted by launchd) | Desktop follows §4.1.1: finds pidfile + PID alive → connects sock → if `system.capabilities` version matches, reuse; if not, SIGTERM the old daemon then respawn |
| Desktop UI quits (user Cmd+Q or auto-update) | The daemon is not killed, keeps listening on sock; requests triggered by cloud channels like Slack/LINE can still complete (calendar tools still available, only the Desktop-side UI feedback channel is closed) |
| daemon crashes mid-call / sock disconnects | Daemon `CancelAll` immediately resolves all pending RPCs → the model gets a `desktop_disconnected` error. The Desktop-side supervisor (`DaemonManager`) detects the child exit and attempts respawn per the `maxRestarts` policy |
| Cloud channel triggers (Slack/LINE etc.) and local Desktop has never run | There is simply no daemon process on that user's machine → the cloud relay finds no endpoint → what the user sees in Slack is the cloud relay layer's fallback reply (unrelated to calendar tools; the daemon is globally unreachable) |

---

## 3. Desktop-Side Technical Design

### 3.1 Prerequisites — to be confirmed by the Desktop team

| Item | Requirement | Verification |
|---|---|---|
| Add usage descriptions to `Info.plist` | `NSCalendarsFullAccessUsageDescription` (macOS 14+, read requires full-access), `NSContactsUsageDescription` (for v2), `NSRemindersFullAccessUsageDescription` (for v2) | After repackaging, check with `codesign -dvvv` + `defaults read Info` |
| Hardened Runtime enabled | Yes | `codesign -d --entitlements - <App>` |
| App is notarized | Required | Only then will TCC "remember" the grant, otherwise it re-prompts every launch |
| Stable Bundle ID | `run.shannon.shanclaw` (Kocoro Desktop's existing ID, **do not change**) | The TCC database is indexed by bundle ID + code requirement; changing the ID wipes all grants |
| Stable Team ID | Same as above | Sign with the same developer account |

**Warning**: Changing the Bundle ID or signing identity is equivalent to "a brand-new App"; all user grants are invalidated. Do not change these two before the v1 launch during the planning period.

### 3.2 EventKit Integration Notes

```swift
// Kocoro Desktop minimum target = macOS 15+ (CLAUDE.md "macOS 15+ and iOS 18+"),
// requestFullAccessToEvents is available on macOS 14+, so this project needs no fallback.

// 1. Hold a store instance
let store = EKEventStore()

// 2. Request Full Access (read requires full-access, see the §5.2 check_permission mapping table)
try await store.requestFullAccessToEvents()

// 3. Observe EKEventStoreChangedNotification to handle external changes (CalendarAgent sync write-back)
NotificationCenter.default.addObserver(
    forName: .EKEventStoreChanged, object: store, queue: .main
) { _ in /* invalidate local cache */ }

// 4. Query with a predicate (don't iterate)
let predicate = store.predicateForEvents(
    withStart: start, end: end, calendars: calendars)
let events = store.events(matching: predicate)
```

### 3.3 ⚠️ Four Concrete Gotchas (must handle)

#### Time zones and datetime format

- EventKit's `EKEvent.startDate / endDate` is a `Date` (absolute instant) + a separately stored `timeZone: TimeZone?`
- The RPC protocol pins down **RFC 3339** (a strict subset of ISO-8601), not broad ISO 8601:
  - ✅ `2026-05-26T09:00:00+08:00`
  - ✅ `2026-05-26T09:00:00.123+08:00` (fractional seconds optional)
  - ✅ `2026-05-26T01:00:00Z` (`Z` is an alias for `+00:00`)
  - ❌ No time-zone suffix (`2026-05-26T09:00:00`) → returns `invalid_argument`
  - ❌ Date only (`2026-05-26`) → returns `invalid_argument` unless `all_day: true`
- Swift side uses `ISO8601DateFormatter` with `[.withInternetDateTime, .withFractionalSeconds, .withTimeZone]`; Go side uses `time.RFC3339Nano`
- **All-day event end boundary** (a long-standing EventKit ambiguity):
  - The RPC layer pins it down: `end` is **the last day's 23:59:59 in local time** (inclusive end-of-day)
  - Internally, Desktop must convert to EventKit's exclusive next-midnight (`endDate = end-of-day + 1 second`, or convert to the next day's 00:00:00)
  - A one-day all-day event: `start: 2026-05-26T00:00:00+08:00`, `end: 2026-05-26T23:59:59+08:00`

#### Deletion/modification scope for recurring events

EventKit's API:
```swift
try store.remove(event, span: .thisEvent)        // delete a single instance
try store.remove(event, span: .futureEvents)     // delete this instance and all later ones
// No .allEvents — to delete the whole series you must first get the series master, then remove
```

The RPC protocol layer wraps this into three states:
```
scope: "this"            → .thisEvent
scope: "this_and_future" → .futureEvents
scope: "all"             → Desktop first fetches the master event, then .thisEvent
```

#### Google / Exchange sync delay

EventKit writes land in local SQLite immediately; CalendarAgent pushes to the cloud asynchronously in the background. If the Daemon tool chain needs "read-verify immediately after write", Desktop should:

- Explicitly `await store.commit()` before returning from the write
- Not do a "read-after-write" double-check inside the RPC (let the Daemon-side tool decide whether to poll)
- Include `pending_remote_sync: bool` in the result to tell the caller "written locally, the cloud may not have finished pushing"

#### ⚠️ Writing attendees does not auto-send invitations on macOS EventKit

This is a gotcha that bites. On macOS, EventKit allows **reading** `EKEvent.attendees`, but **writing attendees does not trigger invitation emails** to the Google CalDAV / Exchange backend — `EKEventStore.save` does not error, but the recipients on the Google/Outlook side simply never receive the invitation. Calendar.app can send invitations itself because it sends them separately via the Mail.app/Exchange protocol, not through the EventKit save path.

**v1 decision**:
- Accept the `attendees` field, written as event metadata
- create_event / update_event **always return** `invitations_sent: false` in the result (regardless of whether attendees is empty), clearly telling the caller "invitations were not sent". An always-present schema is friendlier to the model than "omit by case" (no field-existence check needed) — v1 is always `false`; once the v1.x AppleScript-Calendar fallback lands it may flip to `true`
- The **v1.x patch** plans a Desktop-side AppleScript-Calendar.app fallback to send invitations (Calendar.app internally invokes the Mail.app/Exchange protocol), unlocked in the order "cancel > request_permission > attendees" per the §7.4 changelog, without waiting for v2

This way, once the model gets the result it can tell the user: "The event is created, but you'll need to click Send in Calendar yourself to send the invitation" — avoiding the worst-case silent-failure UX.

### 3.4 TCC First-Time Authorization UX

```
User asks the Agent to use the calendar for the first time
    │
    ↓
Daemon: calendar.check_permission → Desktop
Desktop replies "not_determined"
    ↓
Daemon: tool returns [calendar_permission_not_determined] error, with a hint
Model tells the user: "Calendar permission is needed, please authorize it in Kocoro settings"
    │
    ↓
User opens Desktop settings → clicks "Authorize Calendar"
Desktop calls store.requestFullAccessToEvents()
macOS shows the system dialog ("Kocoro wants to access your calendar")
User clicks "Allow"
    ↓
Desktop pushes desktop_event { event: "calendar_permission_changed", data: { status: "granted" } } to the Daemon over the sock channel
    ↓
User asks the Agent to try again, this time it succeeds
```

**Two paths for triggering authorization** (model's choice):

| Path | When | Behavior |
|---|---|---|
| **A. User-driven** (diagram above) | Recommended / UX-friendly | The model tells the user to go to Desktop settings and click the "Authorize Calendar" button; the user sees what they clicked |
| **B. Model direct RPC** | The user is already interacting with the Agent, high trust | The model directly calls the daemon tool `calendar_request_permission` → RPC to Desktop → Desktop calls `requestFullAccessToEvents` → system dialog, no Desktop UI intervention needed |

Both are valid. Path B's benefit is not interrupting the Agent conversation; path A's benefit is the user is clearer about what they're authorizing. **Desktop must implement both** — path B uses the `calendar.request_permission` RPC, path A uses a button on the Desktop settings page (the same internal function).

**deep link convention**: when permission is missing, besides returning a tool error, the Daemon can use the existing notification channel to have Desktop pop a card **with an "Authorize" button** (modeled on the existing approval card). Clicking the button jumps directly to the corresponding settings page:

```
kocoro://settings/permissions/calendar
kocoro://settings/permissions/reminders   (v2)
kocoro://settings/permissions/contacts    (v2)
```

Desktop registers the URL scheme to handle these deep links and jumps to the corresponding panel.

### 3.5 Desktop-Side Availability Events

Desktop's non-request notifications (TCC status flips, EventKit data changes, etc.) are pushed proactively to the Daemon via the **`desktop_event` frame on the Unix socket** (see §5.1). Desktop being offline is determined by the Daemon itself from sock EOF/EPIPE, with no explicit push from Desktop needed.

| Event type | Producer | Timing | payload |
|---|---|---|---|
| `desktop_online` | Pushed by Desktop | The first non-request frame **after §4.1.1 reconciliation completes and `system.capabilities` version negotiation passes** | `{version: "1.0.0", platform: {os: "macOS", os_version: "14.4.1", app_version: "1.2.3"}}` — declares only "I, as client, am online"; the version / methods list is already negotiated by `system.capabilities` and is not repeated in this frame |
| `desktop_offline` | **Constructed internally by Daemon** | Generated by the daemon itself when the sock disconnects (EOF / EPIPE), not pushed by Desktop | `{}` |
| `calendar_permission_changed` | Pushed by Desktop | TCC status flip (user changed the grant in System Settings or Desktop settings) | `{status: "granted" \| "denied" \| "not_determined" \| ...}` |
| `calendar_data_changed` | Pushed by Desktop | `.EKEventStoreChanged` Notification fires | `{}` (Daemon uses it only as a cache-invalidation hint, optional implementation) |

---

## 4. Daemon-Side Contract Surface — so the Desktop team knows how we connect

> This part is implemented by the Daemon team; it's listed here so the Desktop team has a clear picture.

### 4.1 IPC channel: Unix domain socket, reusing the ApprovalBroker code structure

> ⚠️ **Key clarification**: DesktopRPCBroker runs only between **daemon ↔ local Desktop**, never through the Cloud relay. Even if a user triggers a calendar query from a cloud channel like Slack/LINE, the RPC is sent from the local daemon directly to the local Desktop. "Reuse ApprovalBroker" refers to the **code structure** (pending map / blocking channel / cleanup hooks / onCleanup bus-publish pattern), not the same transport path. ApprovalBroker's wire path is daemon → Cloud → Ptfrog; DesktopRPCBroker's wire path is daemon ↔ local Unix socket ↔ Desktop.

**Single transport: Unix domain socket**

- **Lifecycle**: the daemon is a child of Desktop (already started via the `Helpers/ShanClaw Engine.app` path); when Desktop starts the daemon it passes the sock path **and** the pidfile path as **two independent CLI flags** (since v0.5.1: to avoid implicit coupling between a daemon-derived pidfile path and a Desktop hard-coded path):
  ```
  shan --rpc-socket  "$HOME/Library/Application Support/run.shannon.shanclaw/daemon.sock" \
       --rpc-pidfile "$HOME/Library/Application Support/run.shannon.shanclaw/daemon.pid"
  ```
  On startup the daemon does `os.Remove` of the old sock file then `net.Listen("unix", path)`, and immediately `os.Chmod(path, 0600)` after listening (macOS Unix socket file permissions **default to the umask**, so an explicit chmod is required).
- **Directory permission**: the parent directory `~/Library/Application Support/run.shannon.shanclaw/` should be 0700. Desktop `mkdir -p` and sets the permission before starting the daemon.
- **pidfile**: after the daemon successfully starts listening on the sock, it atomically (write-then-rename) writes the path specified by the `--rpc-pidfile` flag (conventional value `~/Library/Application Support/run.shannon.shanclaw/daemon.pid`), whose content is a single-line PID (consistent with the daemon's existing `internal/daemon/pidfile.go` style). Desktop reads it in the §4.1.1 reconciliation flow. Both sides **must never derive** the pidfile path (don't assume it's the sock path with a changed suffix) — just use the value passed explicitly via the flag.
- **Connection direction**: the daemon listens, Desktop is the client; after connecting, only once §4.1.1 reconciliation completes does it send `desktop_event { event: "desktop_online", payload: {...} }`.
- **Reconnect**: Desktop detects sock disconnect (read EOF / write EPIPE) → backs off and reconnects. The daemon rewrites the sock file on restart; Desktop just reconnects.
- **Frame schema**: see §5.1 (length-prefixed JSON).

**Why not localhost TCP + WebSocket (the v0.4 design)**:
1. **Trust boundary**: TCP `127.0.0.1` is open to all local userland processes (browser extensions, third-party scripts, a poisoned tool chain). A Unix socket + 0600 file permission + 0700 directory permission shrinks the attack surface to "processes that can read the current user's `~/Library`" — the same trust boundary as TCC.
2. **No port discovery needed**: the sock path is a conventional constant, with no need for a `~/.shannon/daemon.port` file or a `KOCORO_DAEMON_PORT` env, eliminating two failure modes (stale port file / env not inherited).
3. **No WebSocket protocol overhead**: HTTP upgrade handshake, masking, ping/pong — none of which the sock needs; length-prefixed JSON is a few lines of code.

**Why not HTTP POST receipts (the v0.3 design)**: the sock is a bidirectional stream, so sending the receipt back over the same sock is much simpler — no need to generate a per-request anti-forgery token. The HTTP POST path is not opened in v1; revisit it later if a browser-based admin UI ever needs it.

**Trust model**: relies on Unix file permissions. After accept, the daemon may additionally `getpeereid` (macOS) to double-check that the connecting peer's UID equals its own — v1 defaults this OFF (0600 file permission is sufficient); v2 can add an optional `--strict-peer-uid` flag for enterprise deployment scenarios.

**At the code level**:
- Copy `ApprovalBroker` into one called `DesktopRPCBroker` (v1 does no generic abstraction — YAGNI)
- The pending table key is `request_id`, the value is a struct with a result channel; timeout / sock disconnect / explicit cancel all unblock it
- On Desktop sock disconnect, call `CancelAll`; all pending unblock with a `desktop_disconnected` error, **no request replay**
- The framing codec lives in `internal/daemon/desktop_rpc/codec.go`, with no third-party library (`bufio.Reader` + `binary.BigEndian.Uint32`, a few dozen lines)

**Debugging aid**: during integration, use the `system.ping` method (schema in §5.2) to verify the channel is bidirectionally connected. Version negotiation is already done in the reconciliation flow (§4.1.1) + `system.capabilities`, so no separate integration is needed.

### 4.1.1 Version negotiation when Desktop starts the daemon (reconciliation flow)

**Problem background**: the v0.5 deployment model is "semi-bound" (see the §2.1 deployment premise) — when the Desktop UI quits it does not actively kill the daemon, and the daemon is adopted by launchd and keeps running. This means:

```
T0  User runs Kocoro Desktop v1.0, spawns shan v1.0, daemon listens on sock
T1  User closes the Desktop UI    → shan v1.0 is orphaned and keeps running (PPID = 1)
T2  Sparkle auto-update → Desktop.app file is already replaced with v2.0, but the shan v1.0 process is still running
T3  User reopens Desktop v2.0
    If Desktop v2.0 connects this sock directly: it talks to shan v1.0,
    and when it calls an RPC method that only exists in v2.0, shan v1.0 reports unknown method → the tool half-hangs strangely
```

**reconciliation spec**: on every startup Desktop handles the daemon process in the following order:

```
1. Read the pidfile: ~/Library/Application Support/run.shannon.shanclaw/daemon.pid

2. If the pidfile exists AND the corresponding PID is still running (kill(pid, 0) == 0):
   a. Try to connect the sock
   b. On connect, the first frame is system.capabilities (not desktop_event)
   c. Compare result.version with the protocol version Desktop itself expects:
      - Match → reuse the existing daemon, send desktop_event { event: "desktop_online", ... }
      - Mismatch → the SIGTERM path of step 2c-i
   d. Connect fails (sock is stale, daemon process is dead but pidfile lingers) → go to step 3

   2c-i. SIGTERM path:
        - kill(pid, SIGTERM)
        - Poll kill(pid, 0) until it returns ESRCH (process exited) or a 5s timeout
        - 5s timeout without exit → kill(pid, SIGKILL), then poll for 2s
        - Still not exited after 2s (extremely rare, zombie processes etc.) → Desktop shows a user-visible error
          "Engine leftover-process cleanup failed, please restart your Mac", not a silent continue
        - Exit succeeds → go to step 3

3. Clean up debris: os.Remove(sock) + os.Remove(pidfile)

4. Spawn a new daemon:
   Process(executableURL = <bundled shan>, arguments = ["--rpc-socket", <path>])
   inherit shell env (so the daemon finds nvm/npx/pyenv/GOPATH etc., see the existing
   DaemonManager.resolveShellEnvironment implementation)

5. Daemon-side startup order:
   a. os.Remove(sock_path)  ← defend against a stale file
   b. net.Listen("unix", sock_path)
   c. os.Chmod(sock_path, 0600)
   d. write-then-rename pidfile (write daemon.pid.tmp first, then rename to daemon.pid)
   e. start the accept loop
```

**Key design points**:
- The pidfile is written by the daemon (step 5d) and read by Desktop (step 1) — both sides agree only on the path, no IPC
- pidfile content: a single line of pure numeric PID, no other fields
- The `system.capabilities` in step 2b must come **before** `desktop_event { desktop_online }` — receiving desktop_event means Desktop is already "online", and killing the daemon after that would scramble the ordering
- `system.capabilities` is **not just a diagnostic method** in this flow; it is the version-negotiation method required by reconciliation (see §5.2)
- The PID the pidfile points to is reused by another process (extremely rare wrap-around scenario): the connect-sock in step 2a will fail (the other process isn't listening on this sock) → naturally falls back to step 3, no extra binary check needed
- pidfile exists but the PID is dead (daemon crashed abnormally without cleaning the pidfile): step 2's `kill(pid, 0)` returns ESRCH, jump straight to step 3 cleanup
- Multiple Desktop instances starting concurrently on the same Mac: v1 assumes a single Desktop instance. On the same account macOS LaunchServices reuses the existing instance by default; on a multi-account shared Mac each account's `~/Library/...` is naturally isolated

**Swift-side SIGTERM implementation**: once the daemon is adopted by launchd, its PPID = 1, so Desktop cannot use `Process.terminate()` (that can only terminate its own child). It needs to read the PID from the pidfile, then call Darwin's `kill(pid_t, Int32)` syscall (C interop, just `import Darwin`).

### 4.2 New Daemon-side tools (`internal/tools/calendar_*.go`)

Each tool's `RequiresApproval` behavior:

| Tool | Read/Write | RequiresApproval | Notes |
|---|---|---|---|
| `calendar_list_sources` | Read | No | List all calendar sources (accounts) |
| `calendar_list_events` | Read | No | Events within a time window |
| `calendar_get_event` | Read | No | Single-event details |
| `calendar_create_event` | Write | **Yes** | Goes through the standard approval flow |
| `calendar_update_event` | Write | **Yes** | |
| `calendar_delete_event` | Write | **Yes** | |
| `calendar_check_permission` | Read | No | Internal use |
| `calendar_request_permission` | Side effect | **Yes** | Triggering the TCC dialog is an explicit action |

**Approval policy**: write operations follow the existing always-allow mechanism (see `internal/daemon/alwaysallow.go`). The user can "always allow calendar_create_event for agent X", and it won't prompt next time.

### 4.3 Tool registration (startup-time conditional registration)

shan has multiple run modes: daemon (spawned by Desktop, holding a `DesktopRPCBroker`), TUI, one-shot CLI, MCP server, scheduled task. **Only daemon mode has the sock reverse channel**; the other modes simply can't reach Desktop — registering the calendar tools in those modes is misleading (the model sees the tool but the call is bound to fail).

Correct handling: **decide whether to register at process startup based on whether a broker is held**, not the per-request runtime filter proposed in v0.4 (runtime filtering is the anti-pattern of "the tool is registered but then hidden from the model").

`internal/tools/register.go` already registers `session_search` / `cloud_delegate` etc. conditionally by their dependencies; the calendar tools follow the same pattern:

```go
// Pseudocode; actually placed inside RegisterLocalTools
if rpcBroker != nil {
    registry.Register("calendar_list_sources", ...)
    registry.Register("calendar_list_events", ...)
    // ... the rest of the calendar_* tools
}
```

**Fallback for calendar needs in TUI / one-shot / MCP / scheduled modes**: the model can naturally reason to use the daemon's existing `applescript` tool (osascript → Calendar.app). `internal/skills/bundled/skills/kocoro/references/calendar.md` should carry a fallback prompt hint (note that AppleScript goes through Automation TCC rather than Calendars TCC, a different permission model), guiding the model to use this path when the calendar tools are not visible. This needs no extra daemon-side coding — pure prompt engineering.

### 4.4 Doc synchronization

CLAUDE.md's convention is that every `mux.HandleFunc(...)` must have a corresponding doc in `internal/skills/bundled/skills/kocoro/references/*.md`. This design adds:

- `internal/skills/bundled/skills/kocoro/references/calendar.md` — tool usage, field meanings, error codes
- `internal/skills/bundled/skills/kocoro/references/desktop-rpc.md` — Unix socket (`~/Library/Application Support/run.shannon.shanclaw/daemon.sock`) + pidfile + length-prefixed JSON frame schema + the §4.1.1 reconciliation flow contract
- README.md / AGENT.md / AGENTS.md — top-level user / external-Agent perspective notes

---

## 5. RPC Protocol Detailed Specification

### 5.1 Envelope (Unix socket frame)

Every frame is a JSON object, **length-prefixed**: a 4-byte big-endian uint32 giving the byte count of the following JSON body, where the body is UTF-8-encoded JSON. The top level has a `type` field. v1 has three types total (payload schemas below); v1.x will introduce a fourth, `desktop_rpc_cancel` (a placeholder in the architecture diagram, schema pending the cancel spec):

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

Field descriptions:
- `request_id`: a 16-character hex generated by the daemon, prefixed with `drpc_`. Desktop echoes it back verbatim in `desktop_rpc_result`
- `method`: see the §5.2 method list
- `timeout_ms`: the daemon-side hard timeout; Desktop should self-limit, and if there's still no result by `timeout_ms - 2000` it's advised to abort proactively and return a `timeout` error, so the daemon gets an error rather than waiting in vain
- `source`: the source channel that triggered this request (`slack` / `wecom` / `kocoro` / `cli` / `schedule` etc., corresponding to RunAgentRequest.Source). Desktop can show "this was triggered from Slack" in its audit UI
- `session_id` / `agent`: Daemon-side context info; Desktop doesn't need to interpret it but can use it for log correlation
- `ts`: the daemon's send timestamp, RFC 3339

#### Desktop → Daemon: `desktop_rpc_result`

Success:

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

Failure:

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

- `code` values are in §5.3
- `retriable: true` is only for genuinely transient errors (temporary EventKit lock contention); the daemon-side tool may retry once. Persistent errors (permission_denied / not_found / invalid_argument etc.) must be `false`
- `details` is a code-specific structured supplement, optional

#### Desktop → Daemon (in a few cases also Daemon → Desktop): `desktop_event`

A non-request async notification, no `request_id`:

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

The v1 event list is in §3.5.

#### Frame size and keepalive

- A single frame's JSON body is capped at **4 MB** (the length prefix is a hard constraint ≤ `4 * 1024 * 1024`; if exceeded, close the connection as soon as the prefix is read, to avoid OOM). In the extreme list_events case, event count is still throttled per §5.2's `limit: 2000 + truncated: true`, and 4 MB is enough to hold the maximum JSON for 2000 events.
- **No application-layer ping needed**: Unix socket disconnect is an OS-level EOF / EPIPE, immediately observable, so no heartbeat is needed. The daemon's `ReadDeadline` is left empty (or set to a very large value), relying on the cancel frame (v1.x) or the peer closing the sock to trigger cleanup.
- **Disconnect behavior**: Desktop actively closes the sock → daemon receives EOF → `CancelAll` of all pending RPCs. When the daemon exits it closes the sock, Desktop receives EOF and triggers the §4.1.1 reconciliation restart flow.
- **Framing spec**: read = first read 4 bytes → uint32 length N → check `0 < N ≤ 4 * 1024 * 1024` (any other range immediately closes the sock) → read N body bytes → `json.Unmarshal`; write = `json.Marshal` → check `len ≤ 4 * 1024 * 1024` → write the 4-byte length + body. **No fragmentation**: a single frame is sent/received in one complete operation, avoiding reassembly complexity.

### 5.2 Method List (v1)

**enum naming rule**: all enum string values are uniformly **lowercase snake_case** (`icloud` / `needs_action` / `not_determined`). The `iCloud` / `CamelCase` forms that appear in several places in this doc are normalized to this rule.

---

#### `system.ping`

For integration; verifies the channel is bidirectionally connected.

**params**: `{"echo": "<arbitrary string>"}` (optional)

**result**: `{"pong": "<echo back>", "server_time": "<RFC 3339>"}`

#### `system.capabilities`

**Bidirectional method (pinned in v0.5.1)**: **both** sides implement the responder end; the call direction depends on the use case:

| Scenario | Direction | What the responder fills in |
|---|---|---|
| §4.1.1 reconciliation (Desktop startup handshake) | **Desktop → Daemon** (Desktop sends the request) | The Daemon's own protocol version + ProtocolMethods (see §5.5.2) + daemon binary version |
| Diagnostics bundle (when a user reports a bug) | **Daemon → Desktop** (Daemon sends the request) | Desktop's own protocol version + ProtocolMethods + Desktop bundle app version |

> The v0.5 spec had a typo here saying "system.capabilities is request-response (daemon → desktop)" — that direction is only the diagnostics scenario; the reconciliation main flow is **Desktop → Daemon**, fixed in v0.5.1.

**params**: `{}`

**result schema**:
```json
{
  "version": "1.0.0",
  "methods": [ /* see §5.5.2 ProtocolMethods, both sides respond with a byte-identical array */ ],
  "platform": {
    "os": "macOS",
    "os_version": "14.4.1",
    "app_version": "1.2.3"
  }
}
```

**`version` field**: the version number of this protocol spec (**not** the app version). Each side hard-codes the protocol version it implements (v1 is `"1.0.0"` on both, see §5.5.1). Reconciliation comparison:
- Match → reuse the existing daemon
- Mismatch → Desktop takes the SIGTERM path to respawn the new-version daemon (see §4.1.1 step 2c-i)

**`methods` field semantics (pinned in v0.5.1)**: the list is "**all methods supported by this protocol version**", **not** "the methods this side implements". Both sides return **exactly the same hard-coded array** when responding (in §5.5.2 ProtocolMethods order). Two reasons for this design:
1. The Daemon's response can precisely indicate "I don't see the method calendar.list_events" — after v1.1 adds a new method, an old daemon's response won't have the new method name, and a new Desktop can report the error precisely
2. No need for each side to maintain a runtime table of "which methods I implement" — the protocol version already provides the lockstep guarantee

Daemon-side handling: during the reconciliation handshake, compare `methods` against its own ProtocolMethods; if the diff is non-empty, log a warning and write to the audit log (no runtime filtering — that's the startup-time decision already covered by §4.3 conditional registration).

**`platform.app_version` field semantics (pinned in v0.5.1)**:
- When the responder is **Desktop**: the bundle's `CFBundleShortVersionString` (e.g. `"1.2.3"`)
- When the responder is **Daemon**: the daemon binary version (from the Daemon-side `internal/version` or an equivalent package's `Version` constant, consistent with `shan --version` output)

**Relationship to `desktop_event { desktop_online }`**: in the reconciliation phase, Desktop first sends the `system.capabilities` request (request → response), and only after the version matches does it send `desktop_event { desktop_online }` (fire-and-forget, declaring the client is online). The order is fixed: capabilities first, online only after a match.

---

#### `calendar.list_sources`

**params**: `{}`

**result**:
```json
{
  "sources": [
    {
      "id": "<EKCalendar.calendarIdentifier>",
      "title": "Work",
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
  "query": null | "client meeting",
  "limit": 500
}
```

- `start` / `end`: RFC 3339 (see the §3.3 time-zone section), required
- `calendar_ids`: `null` = all calendars; `["id1", "id2"]` = specified calendars; `[]` empty array = **explicitly return an empty result** (don't "helpfully" treat it as null)
- `query`: optional keyword, case-insensitive substring match against `title` + `notes`
- `limit`: cap on returned event count, default 500, max 2000. When the cap is exceeded the result adds `truncated: true`

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
      "notes": "See attachment",
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

**Field notes (the minimal set for the list view; for detailed fields use `get_event`)**:
- `series_master_id`: when `is_recurring_instance: true`, points to the series master event's `eventIdentifier`, otherwise `null`. When the model needs to "delete the whole recurring series", first get the master_id from the instance, then call `delete_event(id=master_id, scope="all")`
- `organizer_email`: for events where the organizer is the user themselves, EventKit's `EKEvent.organizer` may be `nil` — this field then returns `null`, don't fabricate it
- `attendees.status`: a snake_case-ified `EKParticipantStatus`; unknown values are uniformly mapped to `needs_action`

#### `calendar.get_event`

**params**: `{"id": "<EKEvent.eventIdentifier>"}`

**result**: all fields of `list_events.events[0]`, plus:
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

- `recurrence_rule`: the fields v1 explicitly supports are the 5 above; `by_day` is only meaningful when `frequency: weekly`, and v1 does not support positional expressions for monthly/yearly frequency (e.g. "the first Monday of each month")
- `raw_rrule`: always populated with an RFC 5545 string, giving the model/caller an escape hatch to "write back a rule I don't understand verbatim". The `raw_rrule` read out is generated by EventKit itself and is authoritative
- `alarms`: v1 only supports relative alarms (N minutes before the event); EventKit's absolute alarms are not yet exposed and are silently ignored if encountered

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

- `calendar_id`: `null` = `EKEventStore.defaultCalendarForNewEvents`
- Writing to a read-only calendar (e.g. a birthday calendar or a subscription calendar) → returns a `read_only_calendar` error
- `recurrence_rule` has the same structure as in `get_event`, but v1 accepts either `raw_rrule` or the structured form, one of the two (if both are given, `raw_rrule` wins)

**result**:
```json
{
  "id": "<new event id>",
  "pending_remote_sync": true,
  "invitations_sent": false
}
```

**`invitations_sent` field semantics (pinned always-present in v0.5.1)**:
- **v1 always returns `false`**, regardless of whether the request's `attendees` field is empty — an always-present schema means the model doesn't need a field-existence check
- Semantics: the EventKit-only path does not auto-send invitations via CalDAV/Exchange (see the §3.3 attendees gotcha). Even when attendees is empty (the event has no recipients), the field is still present and `false`, expressing the fact that "this RPC sent no invitations", consistent with EventKit's actual behavior
- After the v1.x AppleScript-Calendar fallback lands, this field may flip to `true` (see the §3.3 v1.x plan)

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

**`patch` semantics (pinned in v1, the Desktop implementation must follow strictly)**:

| Form of a field in patch | Behavior |
|---|---|
| key not in the JSON (missing) | **No change** |
| value is `null` | **No change** (equivalent to missing; distinguishing these two is costly in Swift Decodable, so treat them the same) |
| string value is `""` or list value is `[]` | **Clear** that field |
| `attendees` / `alarms` given a non-empty list | **Whole-value replace** (no merge) |
| `recurrence_rule` given a new value | **Whole-value replace** |

**Additional field-level constraints**:
- `start` / `end` are required on EKEvent and **cannot be cleared** — patch can only "change the value" or "not change"; passing `""` is treated as `invalid_argument`
- A `calendar_id` patch change = moving the event across calendars, which EventKit does via `EKEvent.calendar = newCalendar`, subject to the target calendar's writable constraint; v1 supports it
- Toggling `all_day` rewrites the semantics of start/end; when patch passes both `all_day` + `start/end`, handle it as "interpret all_day first, then parse start/end with the corresponding semantics"

To remove an event's recurrence property, you **must pass top-level `clear_recurrence: true`** (passing null inside patch has the locked meaning "no change", don't reuse it).

**`scope` values**:
- `this`: change only this instance (creates a detached event)
- `this_and_future`: ⚠️ **this EventKit path splits the series** — the actual behavior is to create a new series starting from this instance, with the original series truncated just before this instance. The result is that **the event ID may become invalid**; the caller should rely on the `id` returned in the result

**Why update_event has no `scope: "all"`** (asymmetric with delete_event):
- "Update the whole series" in EventKit = find the series master + update it + let CalendarAgent propagate the change to all instances
- But updating the master also overwrites all instances the user manually edited as detached (EventKit doesn't distinguish), which is a highly destructive operation
- v1 decision: to "change the whole series", let the caller go through `delete_event(scope=all)` + `create_event` in two steps — **explicit, auditable** — avoiding silently overwriting the user's instance edits
- The scenario that genuinely needs "modifying the master" is left to v2 to add `scope: "series_master"`

**result**:
```json
{
  "id": "<may be a new id, especially when scope=this_and_future>",
  "pending_remote_sync": true,
  "invitations_sent": false
}
```

`invitations_sent` field semantics are the same as `create_event` — **always-present since v0.5.1, always `false` in v1** (even if the patch doesn't modify attendees / the modified result is an empty list). The schema shape is identical to create_event.

#### `calendar.delete_event`

**params**: `{"id": "...", "scope": "this" | "this_and_future" | "all"}`

**`scope` values**:
- `this` → `EKEventStore.remove(event, span: .thisEvent)`
- `this_and_future` → `EKEventStore.remove(event, span: .futureEvents)`
- `all` → delete the whole recurring series. Desktop auto-detects the form of the incoming `id`:
  - If the id is the series **master** (`is_recurring && !is_recurring_instance`) → directly `remove(master, span: .thisEvent)`, EventKit will clear the whole series
  - If the id is a series **instance** (`is_recurring_instance`) → find the master via `event.calendarItemIdentifier` or `recurringEventsMatching`, then `remove(master, span: .thisEvent)`
  - If the id is a non-recurring event → `scope: "all"` is equivalent to `scope: "this"`, no error
  - The model may pass either an instance id or a master id; Desktop normalizes internally

**result**: `{"ok": true, "pending_remote_sync": true}`

#### `calendar.check_permission`

**params**: `{}`

**result**: `{"status": "not_determined" | "denied" | "restricted" | "granted" | "write_only"}`

**`EKAuthorizationStatus` → RPC `status` mapping table (Desktop maps strictly per this)**:

| Apple enum | RPC `status` |
|---|---|
| `.notDetermined` | `not_determined` |
| `.restricted` | `restricted` |
| `.denied` | `denied` |
| `.fullAccess` | `granted` |
| `.writeOnly` | `write_only` |

> The project's min target is macOS 15+, so `EKAuthorizationStatus.authorized` (the pre-macOS-14 semantics) need not be handled. The Swift compiler also marks it deprecated under the 15+ SDK.

When the Daemon-side tool sees `write_only`: write operations may proceed, while read operations return `calendar_permission_denied` with `details.status = "write_only"`, prompting the user to upgrade the grant to full access.

#### `calendar.request_permission`

**params**: `{}`

**result**: `{"status": "<new status after user decision>"}`

Internally Desktop calls `requestFullAccessToEvents`, blocking until the user finishes the system dialog. When already in a `granted` / `denied` / `restricted` state it returns the current value immediately, **without re-prompting** (macOS TCC also disallows this and silently ignores it).

**⚠️ v1 known limitation**: this method conflicts with the default `timeout_ms = 30s` (spec §5.1) under the synchronous RPC model — the system dialog can hang for an arbitrarily long time waiting for the user's decision, and 30 seconds is too short.

**v0.5.1 mitigation** (done within v1, not waiting for v1.x async-ification):

| Side | Behavior |
|---|---|
| **Daemon side** | The `calendar_request_permission` tool **specifically overrides** `timeout_ms = 5 * 60 * 1000` (5 minutes) in the RPC envelope, instead of the 30s default of the other calendar tools |
| **Desktop side** | `requestPermission()` internally adds no timeout shorter than 5 minutes; let the blocking call wait for the user's decision, avoiding a double race between the daemon timeout and a Desktop timeout |

v1 behavior (the extreme case where the 5-minute override still times out):
1. Desktop self-limits at `timeout_ms - 2000` (per the §5.1 field description) and returns a `timeout` error to the daemon
2. But **the system dialog is still showing**; after the user eventually finishes, the daemon is notified via the §3.5 `calendar_permission_changed` event
3. On receiving the `timeout`, the model should tell the user "an authorization request has been initiated, please click the system dialog", and subsequently re-call `calendar_check_permission` to confirm the final status

**The v1.x plan** changes it to a fire-and-forget + event-callback async model (per the unlock order cancel > request_permission > attendees in the §7.4 changelog). At that point this method returns `{status: "pending"}`, and the final status is delivered one-way by the `calendar_permission_changed` event.

### 5.3 Error Code List

| code | Producer | Meaning |
|---|---|---|
| `calendar_permission_denied` | Desktop | TCC denied; `details.status` gives the current state (`denied` / `restricted` / `write_only`) |
| `calendar_permission_not_determined` | Desktop | The user hasn't been asked yet; need to call `request_permission` first |
| `not_found` | Desktop | The event / calendar ID does not exist |
| `invalid_argument` | Desktop | Bad parameter format (time parse failure, required field missing, invalid enum value, attempt to clear start/end, etc.); `details.field` points out which field is wrong |
| `read_only_calendar` | Desktop | Attempted to write a read-only calendar (subscription calendar, birthday calendar, etc.) |
| `internal_error` | Desktop | Unclassified exception, `message` required |
| `timeout` | Desktop **or** Daemon | Desktop self-limits and gives up proactively → Desktop pushes back a `desktop_rpc_result` with this code; if there's still no receipt past `timeout_ms` → the Daemon constructs it itself |
| `desktop_disconnected` | **Constructed by Daemon** | Desktop not connected / already disconnected; should not appear in a Desktop reply |

### 5.4 Concurrency & Serialization

- **The Daemon side may send multiple `desktop_rpc_request`s concurrently** (bash_concurrency is on by default, the model may query multiple time windows concurrently)
- **Desktop must support concurrent handling**: read-class methods (list / get / check_permission) can run with arbitrary concurrency
- **Desktop should internally serialize write operations (create / update / delete) onto a single serial dispatch queue**: EventKit's concurrent-write behavior has no official Apple guarantee, and CalendarAgent's sync state is not friendly to concurrent writes
- Desktop does not need to deduplicate requests — the Daemon-side ApprovalBroker pattern ensures each `request_id` is unique

### 5.5 Formal Identifier List (added in v0.5.1 — the single source of truth for the two-sided contract)

**Purpose**: eliminate the risk of string-spelling drift between the Daemon Go constants and the Desktop Swift constants. All method names, error codes, enum strings, deep links, etc. — **the strings shared by both sides** — are pinned down centrally in this section. Both sides grep this section as the final check during implementation.

#### 5.5.1 ProtocolVersion

```
"1.0.0"
```

The only v1 value. Bump rule (decided after v1.x): method addition / field addition = patch (1.0.x); field removal / rename / type change = minor (1.x.0); architecture-layer rewrite = major (x.0.0).

#### 5.5.2 ProtocolMethods (10 items; both sides return **exactly the same JSON array** when responding to `system.capabilities`)

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

#### 5.5.3 Frame type identifiers

```
"desktop_rpc_request"
"desktop_rpc_result"
"desktop_event"
"desktop_rpc_cancel"   ← v1.x placeholder, schema TBD
```

#### 5.5.4 Error codes (8 items; same table as spec §5.3)

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

#### 5.5.5 TCC permission status (5 items; same table as spec §5.2 `calendar.check_permission`)

```
"not_determined"
"restricted"
"denied"
"granted"
"write_only"
```

#### 5.5.6 Calendar account types (7 items; spec §5.2 `calendar.list_sources` `account_type`)

```
"icloud"
"google"
"exchange"
"outlook"
"local"
"subscription"
"other"
```

#### 5.5.7 Attendee participation status (4 items; spec §5.2 `calendar.list_events` `attendees[].status`)

```
"accepted"
"tentative"
"declined"
"needs_action"
```

#### 5.5.8 Event scope (3 items; delete vs update accept different sets)

| Value | `calendar.delete_event` | `calendar.update_event` |
|---|---|---|
| `"this"` | ✓ | ✓ |
| `"this_and_future"` | ✓ | ✓ |
| `"all"` | ✓ | ✗ (explicitly disabled by spec §5.2; Desktop returns `invalid_argument` on receipt) |

#### 5.5.9 Desktop event types (4 items; spec §3.5)

```
"desktop_online"              ← pushed by Desktop
"desktop_offline"             ← constructed internally by Daemon (not on the wire)
"calendar_permission_changed" ← pushed by Desktop
"calendar_data_changed"       ← pushed by Desktop (optional implementation)
```

#### 5.5.10 Recurrence frequency (4 items; spec §5.2 `calendar.get_event` `recurrence_rule.frequency`)

```
"daily"
"weekly"
"monthly"
"yearly"
```

#### 5.5.11 Deep link URL

The scheme name is TBD in v1 (what Desktop actually registers might be `shanclaw://` or something else, see §7.2 Q4). This section pins down the host + path:

```
<scheme>://settings/permissions/calendar
<scheme>://settings/permissions/reminders   ← v2
<scheme>://settings/permissions/contacts    ← v2
```

#### 5.5.12 Implementation discipline

- **Daemon Go side**: all the above strings are centralized in `internal/daemon/desktop_rpc/types.go` as exported `const` / `var`, ordered per this section; other modules (tools / dispatcher / register.go) `import` these constants — **inline literal strings are forbidden**
- **Desktop Swift side**: all the above strings are centralized in `Packages/ShanClawBridge/Sources/ShanClawBridge/DesktopRPC/ProtocolConstants.swift` (new file) as `static let` constants under `enum ProtocolConstants`; dispatcher / mappers / UI all import them — **inline literal strings are forbidden**
- **Phase 1 checkpoint**: each side runs `grep -r '"calendar\.\|"desktop_rpc_\|"calendar_permission_'` once to ensure the inline-string count = 0; any grep hit must be replaced with a constant reference
- **This section is the contract**: if you find you need a new string during implementation (v1.x adds a new method / error code / enum value) → first PR this spec's §5.5, and only after it merges do both sides commit code

---

## 6. Phases & Milestones

### Phase 0 — Feasibility PoC (Desktop team, 1-2 days)

**Goal**: verify the TCC flow works end to end and authorization persistence takes effect. **This phase writes no RPC code at all**, only validating EventKit integration + the signing chain inside Desktop.

**Implementation**:
- Add `NSCalendarsFullAccessUsageDescription` to Desktop's `Info.plist`
- Add a temporary menubar / settings-page button that calls `requestFullAccessToEvents` (project min target macOS 15+, no fallback needed)
- A second button: pull the next 7 days of events, console.log the `title / start / end / calendar.title`

**Acceptance checklist (all ✅ before entering Phase 1)**:

- [ ] On first clicking the authorize button, the macOS system dialog pops correctly, showing the `NSCalendarsFullAccessUsageDescription` text
- [ ] After the user clicks "Allow", the next 7 days of events can be listed
- [ ] **Authorization survives a Desktop restart** (no re-prompt needed; if it's lost → there's a Notarization / Hardened Runtime config problem)
- [ ] **Authorization survives a Mac restart**
- [ ] Authorization survives a Desktop upgrade (replace the `.app` but keep the Bundle ID + Team ID unchanged)
- [ ] After clearing the grant with `tccutil reset Calendar <bundle_id>`, the next button click re-prompts (verifies the TCC database is indexed correctly)
- [ ] In "System Settings → Internet Accounts", **configure iCloud + Google + an Exchange / Office 365 account simultaneously**, with each one's "Calendar" toggle on → the listed events show data from all three sources (each `EKCalendar.source.title` is identifiable)
- [ ] Add a new event to the Google calendar (from phone or web), wait 30s for CalendarAgent to sync, re-list → the new event is visible
- [ ] Change the Bundle ID and repackage (this is a negative test) → it should require re-authorization (verifies TCC really indexes by Bundle ID)

If the Phase 0 PoC fails, **all downstream development is moot**; the signing-chain problem must be solved first before continuing.

### Phase 1 — RPC channel (Daemon + Desktop in parallel, 3-5 days)

- Daemon: `DesktopRPCBroker` (replicating the ApprovalBroker pattern) + Unix socket listener (listens on the path passed via the `--rpc-socket` CLI flag, startup order: `os.Remove` → `Listen` → `Chmod 0600` → atomic pidfile write) + length-prefixed JSON frame routing (three types) + `internal/daemon/desktop_rpc/codec.go` (≤ 4 MB body) + `internal/tools/register.go` conditional registration
- Daemon: expose the existing `internal/daemon/pidfile.go` for the `desktop_rpc` subpackage to reuse
- Desktop: change `DaemonManager` to pass `--rpc-socket <path>` when spawning the daemon + a sock client (unix endpoint connect + reconnect backoff) + `DesktopRPCService` frame routing + the §4.1.1 reconciliation flow implementation (pidfile read / `kill(pid, 0)` liveness probe / SIGTERM via Darwin syscall / `system.capabilities` version negotiation / SIGTERM 5s timeout escalation to SIGKILL) + push `desktop_event { event: "desktop_online" }` only after reconciliation completes
- Both sides do `system.ping` e2e integration; `system.capabilities` is validated automatically by the reconciliation flow (no separate integration needed)

### Phase 2 — Calendar read path (4-5 days)

- Desktop: `CalendarProvider` implements `list_sources / list_events / get_event / check_permission / request_permission`
- Daemon: the five tools `calendar_list_sources / calendar_list_events / calendar_get_event / calendar_check_permission / calendar_request_permission`
- Desktop-side first-time authorization UX
- E2E test: ask "what meetings do I have this afternoon" in Slack and get multi-source data from Google / Exchange / iCloud

### Phase 3 — Calendar write path (4-5 days)

- Desktop: `create_event / update_event / delete_event` + recurring-event scope handling (note the `this_and_future` series-split behavior, see §5.2)
- Daemon: the corresponding three tools, through the approval pipeline + `description` field enforcement
- always-allow persistence test
- E2E: have the Agent auto-create an event, change the time, cancel the event; verify the model correctly relays `invitations_sent: false` from the result to the user (v1 does not auto-send invitations, see §3.3)

### Phase 4 — Docs & release (1-2 days)

- Sync CLAUDE.md / README / AGENT.md / kocoro skill references
- Staged release to the internal channel

**Total**: about 13-19 working days (Desktop and Daemon roughly 50/50).

### Cross-team Convergence Checkpoints (added in v0.5.1)

At the completion of each phase, **the Daemon team and Desktop team must jointly verify the following scenarios** before entering the next phase. Each side's unit tests passing ≠ integration-ready — a phase is only truly complete at the convergence checkpoint.

#### Checkpoint @ End of Phase 1 (RPC channel integration)

**Goal**: the channel is bidirectionally connected + the full reconciliation path works. **Can be verified with `nc` + hand-written JSON, no agent loop needed**.

Test cases:
1. Desktop starts the Daemon with the two paths `--rpc-socket` + `--rpc-pidfile`
2. Daemon Listen succeeds + writes sock 0600 + atomic pidfile write + accept loop running
3. Desktop reconciliation: read pidfile (empty on first run, skipped) → connect sock → send `system.capabilities` REQUEST frame
4. Daemon decodes the frame → routes to daemonMethods → returns `system.capabilities` RESULT (with `version: "1.0.0"` + the full ProtocolMethods array + platform.app_version = shan binary version)
5. Desktop compares version, matches → sends `desktop_event { event: "desktop_online" }` frame
6. Daemon EventBus receives the desktop_online event (seeing it in the log is enough)
7. Bidirectional ping: Daemon sends `system.ping {echo: "from-daemon"}` REQUEST → Desktop returns RESULT; Desktop sends `system.ping {echo: "from-desktop"}` → Daemon returns RESULT
8. Simulate version drift: manually kill the Daemon process, use nc to bring up a fake "old daemon" returning `version: "0.9.0"` → verify Desktop takes the SIGTERM ladder (5s SIGTERM + 2s SIGKILL + user error if all fail)
9. Simulate sock listen failure: occupy the sock file first, start the Daemon → verify a non-zero exit + stderr error + Desktop DaemonManager catches it and shows a user-visible error (no silent retry)

#### Checkpoint @ End of Phase 2 (read path e2e)

**Goal**: the calendar read path + multi-source data aggregation + correct TCC state propagation

Test cases (on a real Desktop + real Daemon that have already passed §4.1.1 reconciliation):
1. macOS System Settings → Internet Accounts configures iCloud + Google + an Exchange / Office 365
2. Trigger a Cloud channel (Slack) with "what meetings do I have today"
3. Expected: the calendar_list_events RPC works, returns multi-source events (each event's `calendar_id` identifies which source it belongs to)
4. Time zones correct (event start/end matches the literal values seen in Calendar.app)
5. When `limit: 2000` triggers, the result contains `truncated: true`
6. Under TCC denied status, returns a `calendar_permission_denied` error (not an empty array)
7. Desktop receives an `EKEventStoreChanged` Notification → pushes `desktop_event { calendar_data_changed }` → Daemon EventBus receives it (seen in the log)

#### Checkpoint @ End of Phase 3 (write path e2e + approval + recurring events)

**Goal**: the full write flow + approval pipeline + recurring-event three-state scope

Test cases:
1. Slack sends "a half-hour meeting with Alice at 3pm tomorrow" → `calendar_create_event` → daemon approval card pops (`description` field non-empty) → user approves on Desktop → Desktop EventKit save → result returns `invitations_sent: false`
2. always-allow persistence: user clicks "Always allow" → the second identical create does not pop a card
3. Create a weekly recurring event → "change the time of this coming Wednesday's" goes through `update_event scope: "this"` → the original series is unchanged, this instance is detached
4. "change this coming Wednesday's and all after it" goes through `update_event scope: "this_and_future"` → ⚠️ result returns a **new id** (series split), the caller relies on the new id
5. Simulate misuse: `update_event scope: "all"` → returns `invalid_argument` (disabled by spec §5.5.8)
6. Write to a read-only calendar (Birthdays / subscription) → returns a `read_only_calendar` error
7. `calendar_request_permission` timeout verification: user leaves it unclicked → Daemon returns timeout only after a full 5 minutes (not 30s)

#### Checkpoint @ End of Phase 4 (release-ready)

- Both sides' CLAUDE.md / README / AGENT.md / kocoro skill references consistently mention it
- Both sides' commit messages cross-reference the main PR
- At least 1 week of staged internal channel with no crash / data corruption

---

### Phase 5+ (v2)

Driven in two groups by framework form; recommend doing group A first (following v1's architectural inertia, smaller effort), then group B (which needs an extra AppleScript-wrapping layer on the Desktop side):

**Group A — EventKit / Contacts framework** (reusing this design's RPC architecture, ~3-5 days each)
- Reminders (EventKit, same store, reusing the authorization flow, the main work is the Daemon-side `reminder_*` tools)
- Contacts (CNContactStore, separate TCC request UX)
- Free-busy lookup (multi-attendee aggregation, needs Contacts hooked up first)

**Group B — AppleScript wrapping** (Desktop builds a new NSAppleScript-wrapping layer, ~4-6 days each)
- Mail (Mail.app — transitioning from the daemon `applescript` tool to Desktop RPC, the UX gain > capability gain)
- Teams / Zoom / Meet join-link recognition (lightweight, may be merged into the Calendar v1.x patch)

**Doc rename**: at the v2 kickoff, upgrade `desktop-calendar-rpc.md` to `desktop-pim-rpc.md`, with the method namespace expanded to `calendar.*` / `reminders.*` / `contacts.*` / `mail.*`.

---

## 7. Risks & Open Questions

### 7.1 Identified Risks

| Risk | Mitigation |
|---|---|
| A Desktop signing / Bundle ID change invalidates all user grants | Freeze these two before the v1 launch; later changes go through an ADR with advance notice to users |
| EventKit API differences across macOS versions | Already N/A: project min target macOS 15+, uniformly using the single API set `requestFullAccessToEvents` / `.fullAccess` / `.writeOnly` |
| CalendarAgent sync delay causing "can't read right after write" | The `pending_remote_sync` field + the Daemon tool not doing automatic read-after-write checks |
| The user hasn't added the account to "Internet Accounts" | v1 doesn't support it; v2 evaluates whether to build a built-in OAuth fallback |
| Write operation + always-allow being abused | Follow the existing approval policy; if necessary add `calendar_create_event` to `agent.DisallowsAutoApproval` so it can never be persisted |
| **The sock file is occupied by a third party / filesystem anomaly prevents the daemon from listening** | The daemon immediately exits non-zero + stderr error ("failed to listen on socket `<path>`: `<error>`. Another daemon may be running, or stale file system state. Manual cleanup: `rm <path>` and restart Kocoro Desktop.") + Desktop's `DaemonManager` catches the non-zero exit and shows a user-visible error, **no silent retry** |
| **Semi-bound lifecycle causing daemon version drift** (an old daemon orphaned and still running, encountered when a new Desktop starts) | The §4.1.1 reconciliation flow: pidfile + `system.capabilities` version negotiation, mismatch takes the SIGTERM → respawn path |
| **Multiple Desktop instances starting concurrently on the same Mac** | v1 assumes a single Desktop instance. On the same account LaunchServices reuses the existing instance by default; on a multi-account shared Mac each account's `~/Library/...` is naturally isolated, no conflict |
| **The PID the pidfile points to is reused by another process** | Extremely rare wrap-around scenario. The reconciliation step 2a connect-sock will fail (the other process isn't listening on that sock) → naturally falls back to cleanup + respawn |

### 7.2 Open Items for the Desktop Team

1. Does Desktop currently hold a stable Bundle ID + Team ID? Can they be frozen during v1? The actual Bundle ID is `run.shannon.shanclaw` (from the Kocoro Desktop CLAUDE.md "do not change"), no longer the early-doc example `ai.kocoro.desktop`
2. ~~Does Desktop currently have an IPC client connecting to the Daemon? If so, is it WS or SSE?~~ → settled in v0.5: Desktop's existing HTTP+SSE channel (the Desktop→Daemon business-request direction) is kept as-is; the new v0.5 Unix domain socket channel carries only the daemon→Desktop reverse RPC (see §4.1). The two channels coexist; v1 does not merge them
3. Does the Desktop settings page have an existing "permission management" entry point to hang calendar authorization on? Or does a new panel need to be built?
4. Has Desktop already registered the `kocoro://` URL scheme? v1 needs to respond to deep links like `kocoro://settings/permissions/calendar` (see §3.4). Note that the scheme actually registered for Bundle ID `run.shannon.shanclaw` may be `shanclaw://` or something else; the Desktop team needs to fill in the actual scheme name
5. ~~Minimum supported macOS version?~~ → confirmed macOS 15+ (Kocoro Desktop CLAUDE.md `macOS 15+ and iOS 18+`). `requestFullAccessToEvents` is available on macOS 14+, this project is always-on, and the doc has dropped the pre-14 fallback code
6. **New**: can the Desktop-side `DaemonManager` be modified to pass the `--rpc-socket` flag when spawning the daemon? The existing `launchProcess()` implementation is already `Process()` + `proc.arguments = args`, so adding a flag is a single-point change. Please confirm

### 7.3 Open Items for the Daemon Team

1. Does `DesktopRPCBroker` directly replicate ApprovalBroker, or do a generic abstraction first? Leaning toward the former (YAGNI).
2. The write-operation approval's `description` field is enforced to be model-generated (see the approval card description spec in CLAUDE.md).
3. Confirm the conditional-registration entry point for the calendar tools in `RegisterLocalTools` (per §4.3, register at startup with `if rpcBroker != nil`, same pattern as `session_search` / `cloud_delegate`; v0.5 abandoned the `loadedToolsForRequest` runtime-filter idea designed in v0.4, so there's no longer any runtime filter).

### 7.4 Open Items for Product

1. At v1 launch, do we need a "calendar capability" master switch in Kocoro Desktop settings so privacy-conscious users can turn it off with one click?
2. Does the write-operation audit log also need to show history on the Desktop side ("Kocoro created event … at 14:00 on 5/26")?

---

## 8. Appendix

### 8.1 References

- Apple EventKit docs: https://developer.apple.com/documentation/eventkit
- macOS 14 Calendar permission tiers: https://developer.apple.com/documentation/eventkit/ekauthorizationstatus
- Existing IPC pattern reference code:
  - `internal/daemon/approval.go` (ApprovalBroker)
  - `internal/daemon/skill_filter.go` (capability filtering pattern)
  - `internal/daemon/permissions_darwin.go` (TCC state probing)
  - `internal/daemon/types.go` (envelope definitions)

### 8.2 Change Log

| Date | Version | Change |
|---|---|---|
| 2026-05-26 | v0.1 | Initial draft |
| 2026-05-26 | v0.2 | Clarified that "Internet Accounts" ≠ unified egress; made explicit that v1 covers only the framework category (calendar), with mail in v1 going through the daemon `applescript` tool and merged into RPC in v2; listed Notes as a permanent non-goal |
| 2026-05-26 | v0.3 | Collaboration-oriented review: added the reader guide, RPC endpoint/port discovery, result_url + result_token, the source field, RFC 3339 time format, the TCC status mapping table, update_event patch semantics, the scope=this_and_future series-split warning, the series_master_id field, the attendees-don't-send-invitations gotcha, the all-day end boundary, enum naming normalization, concurrency & serialization, system.ping/capabilities, disconnect-cancel semantics, the deep link convention, the Phase 0 acceptance checklist |
| 2026-05-26 | v0.4 | Second-round review consistency fixes: ① §1.1/§1.3 attendee invitation promise contradicted §3.3 → changed to "metadata write, invitations not auto-sent"; ② in the §3.4 flow `calendar.permission_status` / `[calendar_permission_required]` were stale method name/error code → changed to `calendar.check_permission` / `calendar_permission_not_determined`; ③ §3.5 "Desktop sends an SSE event" had the wrong direction (Desktop is the client) → changed to going through the bidirectional WS reverse channel; ④ removed the whole v0.3 HTTP POST receipt + result_url + result_token set, unified to a **bidirectional WebSocket** (simpler, no port discovery, no token, trust model consistent with the existing localhost-only endpoints); ⑤ rewrote §5.1 as three WS frames (request/result/event) + ping/pong + frame-size constraints; ⑥ §5.2 patch added start/end not clearable + calendar_id cross-calendar move; ⑦ explained why update_event is asymmetric with no scope=all; ⑧ merged the §4.2 debugging subsection into §4.1; ⑨ added the `kocoro://` URL scheme registration item to the §7.2 Desktop-team question list |
| 2026-05-26 | v0.5 | Merged into the main doc after four round-trips of Desktop CC proposal + Daemon CC review + Desktop CC followup + Daemon CC final ack. **Core changes**: ① §1.2 table expanded the rejection rationale for daemon-direct-EventKit from a single-line "bare Go binary has no bundle" to two hard Apple-platform constraints (`LSBackgroundOnly` prevents the TCC dialog + TCC "responsible code" attribution to the parent app), and added that macOS 26 Tahoe release notes are unchanged; the Sidecar became infeasible under the same constraints. ② **the transport changed from v0.4's TCP+WebSocket to a Unix domain socket** (trust boundary aligned with TCC: 0600 file permission + 0700 parent directory, avoiding any local process impersonating Desktop to feed fake calendars), dropping port discovery (`~/.shannon/daemon.port` + `KOCORO_DAEMON_PORT` env) and the WebSocket protocol overhead, switching to length-prefixed JSON framing (≤ 4 MB body). ③ explicit deployment premise: the daemon is a child of Desktop (`DaemonManager.Process()` spawn), the launchd path serves only the npm CLI standalone install; when the Desktop UI quits it does not actively kill the daemon, which is adopted by launchd and keeps running (the semi-bound model, cloud channels like Slack can still trigger). ④ **added the §4.1.1 reconciliation flow** to handle version drift under semi-binding: on startup Desktop negotiates version via pidfile + `system.capabilities`, and on mismatch SIGTERMs the old daemon (escalating to SIGKILL after 5s, and a user-visible error if it still won't exit after 2s rather than silently continuing) then respawns. ⑤ §4.3 changed "runtime tool filtering" to "startup conditional registration" — non-daemon modes like TUI/one-shot/MCP/scheduled simply don't register the calendar tools, and users in those modes use the `applescript` + Calendar.app fallback. ⑥ §5.2 `system.capabilities` upgraded from v0.4's runtime tool-filter use to a required reconciliation version-negotiation method + diagnostics use, keeping the `methods` field (under semi-binding, version drift is an inevitable event rather than a theoretical possibility, and the diff info enriches fail-fast diagnostics). ⑦ §7.1 risk table added the sock-listen-failure fallback (non-zero exit + user-visible error, no silent retry) / version-mismatch reconciliation / multi-instance scope / PID wrap-around, four items. ⑧ deferred three independent improvements not merged into this version: the `desktop_rpc_cancel` frame (architecture-diagram placeholder, schema TBD), `calendar.request_permission` async-ification (conflicts with `timeout_ms`), and v1 attendees going through AppleScript-Calendar to actually send invitations — to be resolved successively in the v1.x patch in the order cancel > request_permission > attendees. |
| 2026-05-26 | v0.5.1 | After the Daemon Plan + Desktop Plan alignment review, made 8 spec fixes, all contract-layer clarifications that don't change the architecture established in v0.5: ① §4.1 changed sock + pidfile to **two independent CLI flags** (`--rpc-socket` + `--rpc-pidfile`), avoiding implicit coupling between a Daemon-derived path and a Desktop hard-coded path; ② §5.2 fixed v0.5's "daemon → desktop" direction typo for `system.capabilities` — the reconciliation main flow is **Desktop → Daemon**, the reverse is only the diagnostics scenario; ③ §5.2 `platform.app_version` clarifies what each responder fills in (Desktop = bundle CFBundleShortVersionString, Daemon = shan binary version); ④ §5.2 `methods` array semantics pinned as "all methods supported by the protocol version", both sides responding with a byte-identical array (not "the methods this side implements"); ⑤ **added §5.5 the formal identifier list** as the single source of truth for the two-sided contract — 10 method names / 8 error codes / 5 TCC statuses / 7 account types / 4 attendee statuses / 3 scopes / 4 desktop event types / 4 recurrence frequencies / 3 deep link paths all centralized in this section, both sides grep-checking to avoid spelling drift; ⑥ §6 added cross-team Convergence Checkpoints, the scenarios both sides must jointly verify at each phase's completion (Phase 1 bidirectional ping + version drift + sock listen failure; Phase 2 multi-source data + time zones + TCC denied; Phase 3 approval pipeline + recurring events + three-state scope + 5min request_permission timeout); ⑦ **reverted the `invitations_sent` field to always-present** (in the v0.5 phase it was once changed to "omit when there are no attendees", contradicting the example schema + §1.3 + the Phase 3 acceptance checklist in multiple places): §3.3 + the §5.2 create_event field-semantics paragraph + the §5.2 update_event field-semantics line are unified back to always-present, always returning `false` in v1; ⑧ §5.2 `calendar.request_permission` added a timeout mitigation (done in v1, not waiting for v1.x async-ification) — the Daemon side specifically overrides `timeout_ms = 5 * 60 * 1000` in the envelope, and the Desktop side cooperates by not adding a short timeout. |
