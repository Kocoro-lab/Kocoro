## Fix: final-review findings (retina + tests)

### Fix 1 — Retina half-resolution capture

**File:** `internal/tools/axserver/Sources/WindowCapture.swift` (~lines 84-96)

**Problem:** `SCStreamConfiguration.width/height` was set directly from `SCWindow.frame.width/height`, which are in **points**. On a 2× Retina display this produced a capture at half native resolution (e.g. 1440×900 instead of 2880×1800 for a fullscreen window).

**Change:** Inserted a backing-scale lookup before setting `config.width/height`. The code finds the `NSScreen` whose `frame` contains the window's center point, reads its `backingScaleFactor`, and multiplies the point dimensions by that scale before converting to `Int` via `.rounded()`. Falls back to `NSScreen.main?.backingScaleFactor` then `1.0` if no matching screen is found.

**Build verification:** `cd internal/tools/axserver && swift build` → `Build complete! (0.82s)` — clean, no warnings.

**Headless runtime note:** pixel-dimension verification (confirming the captured `CGImage.width` equals `frame.width × scale`) requires a running Quartz display session and ScreenCaptureKit TCC grant. A raw CLI binary exits early with `CGS_REQUIRE_INIT`. Runtime pixel-dim verification must be a human smoke step in the bundled app.

---

### Fix 2 — Missing coverage for invalid `app_name` → 400

**File:** `internal/daemon/screenshot_window_test.go`

**Added:** `TestScreenshotWindow_RejectsInvalidAppName` — iterates over four representative bad `app_name` values (`../etc/passwd`, `foo;rm -rf /`, `app<script>`, `name\x00null`) and asserts each returns HTTP 400. Uses the real `handleScreenshotWindow` handler directly (no stub needed — validation fires before `captureWindowVia`).

**Validation path exercised:** `handleScreenshotWindow` line 68: `tools.ValidAppNamePattern.MatchString(req.AppName)` → `writeError(w, http.StatusBadRequest, ...)`.

---

### Fix 3 — Unchecked `json.Unmarshal` in tests

**File:** `internal/daemon/screenshot_window_test.go`

**Changed:** Two bare `json.Unmarshal(...)` calls (in `TestScreenshotWindow_MapsDeniedTo403` and `TestScreenshotWindow_SuccessReturnsImage`) were silently discarding the error return. Wrapped both with `if err := json.Unmarshal(...); err != nil { t.Fatalf("unmarshal: %v", err) }`.

---

### Test + build output

```
go test ./internal/daemon/ -run 'TestScreenshotWindow|TestWireFixture' -v

=== RUN   TestScreenshotWindow_RejectsEmptyBody     --- PASS
=== RUN   TestScreenshotWindow_MapsDeniedTo403      --- PASS
=== RUN   TestScreenshotWindow_RejectsInvalidAppName --- PASS
=== RUN   TestScreenshotWindow_SuccessReturnsImage  --- PASS
[... all TestWireFixture_* tests PASS ...]
PASS
ok  github.com/Kocoro-lab/ShanClaw/internal/daemon  0.507s

go build ./...   → (no output, exit 0)

swift build (axserver) → Build complete! (0.82s)
```
