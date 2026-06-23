import Foundation
import AppKit
import CoreGraphics

/// Result of capture_window. Encoded into the NDJSON `result` field via AnyCodable.
struct CaptureWindowResult: Codable {
    let ok: Bool
    let code: String?
    let imageBase64: String?
    let width: Int?
    let height: Int?

    enum CodingKeys: String, CodingKey {
        case ok, code
        case imageBase64 = "image_base64"
        case width, height
    }

    static func failure(_ code: String) -> CaptureWindowResult {
        CaptureWindowResult(ok: false, code: code, imageBase64: nil, width: nil, height: nil)
    }
    static func success(_ base64: String, _ w: Int, _ h: Int) -> CaptureWindowResult {
        CaptureWindowResult(ok: true, code: nil, imageBase64: base64, width: w, height: h)
    }
}

/// Capture a single window's pixels for the given pid (or app name).
///
/// Implementation note — why NOT ScreenCaptureKit:
/// `SCShareableContent` / `SCScreenshotManager` require a window-server GUI
/// session that the LSUIElement `Kocoro AX.app` helper does not have, and abort
/// the process (`CGS_REQUIRE_INIT`) → the daemon sees "unexpected EOF". Instead
/// we look up the window id via the lightweight `CGWindowList` query and shell
/// the system `/usr/sbin/screencapture -l<id>` binary, which runs as its own
/// process (its own session) and works with ax_server's existing Screen
/// Recording grant.
///
/// Window selection: if `windowTitle` is given, the first on-screen normal
/// window of the pid whose title contains it (case-insensitive); else the
/// largest on-screen normal window of the pid.
func captureWindow(pid: Int?, appName: String?, windowTitle: String?) -> CaptureWindowResult {
    // Passive grant check — never prompts (the Desktop drives request_permission).
    guard CGPreflightScreenCaptureAccess() else {
        return .failure("screen_recording_denied")
    }

    // Resolve the target pid.
    var targetPID = pid ?? 0
    if targetPID <= 0 {
        guard let name = appName, let resolved = resolvePID(appName: name) else {
            return .failure("app_not_found")
        }
        targetPID = resolved
    }

    // Enumerate windows (a lightweight CG query — no SCK, no crash). Use
    // .optionAll (NOT .optionOnScreenOnly) so an app the user keeps on another
    // Space (or minimized) is still found — screencapture -l can grab another
    // Space's window. We prefer an on-screen window in selection below.
    let options: CGWindowListOption = [.optionAll, .excludeDesktopElements]
    guard let infoList = CGWindowListCopyWindowInfo(options, kCGNullWindowID) as? [[String: Any]] else {
        return .failure("window_not_found")
    }

    struct Candidate { let id: CGWindowID; let area: CGFloat; let title: String; let onScreen: Bool }
    var candidates: [Candidate] = []
    for info in infoList {
        guard let owner = (info[kCGWindowOwnerPID as String] as? NSNumber)?.intValue,
              owner == targetPID else { continue }
        // Layer 0 = normal app windows (excludes menubar/dock/overlays).
        let layer = (info[kCGWindowLayer as String] as? NSNumber)?.intValue ?? -1
        guard layer == 0 else { continue }
        guard let number = (info[kCGWindowNumber as String] as? NSNumber)?.uint32Value else { continue }
        let title = info[kCGWindowName as String] as? String ?? ""
        let onScreen = (info[kCGWindowIsOnscreen as String] as? NSNumber)?.boolValue ?? false
        var area: CGFloat = 0
        if let boundsDict = info[kCGWindowBounds as String] as? NSDictionary,
           let rect = CGRect(dictionaryRepresentation: boundsDict as CFDictionary) {
            area = rect.width * rect.height
        }
        candidates.append(Candidate(id: number, area: area, title: title, onScreen: onScreen))
    }
    guard !candidates.isEmpty else {
        return .failure("window_not_found")
    }

    // Prefer an on-screen window, then the largest by area (the main window is
    // almost always the biggest; thin toolbar/strip windows rank below it).
    let better: (Candidate, Candidate) -> Bool = { a, b in
        if a.onScreen != b.onScreen { return !a.onScreen }   // off-screen ranks lower
        return a.area < b.area
    }
    let chosen: Candidate
    if let want = windowTitle, !want.isEmpty {
        guard let match = candidates
            .filter({ $0.title.range(of: want, options: .caseInsensitive) != nil })
            .max(by: better) else {
            return .failure("window_not_found")
        }
        chosen = match
    } else {
        chosen = candidates.max(by: better)!
    }

    // Capture the chosen window by id via the system screencapture binary.
    let tmpPath = (NSTemporaryDirectory() as NSString)
        .appendingPathComponent("kocoro-capwin-\(UUID().uuidString).png")
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
    // -x: silent · -o: omit window shadow · -l<id>: capture that window's content
    proc.arguments = ["-x", "-o", "-l\(chosen.id)", tmpPath]
    do {
        try proc.run()
    } catch {
        return .failure("window_not_found")
    }
    defer { try? FileManager.default.removeItem(atPath: tmpPath) }
    // Watchdog: screencapture can hang (window-server stall, target window vanished
    // mid-capture, heavy load). ax_server serves requests serially, so an unbounded
    // waitUntilExit() here would wedge EVERY subsequent AX call until the helper is
    // killed by hand. Bound it, then SIGTERM and fail closed (the 3-code contract has
    // no generic capture-failure code, so collapse to window_not_found).
    let captureDeadline = Date().addingTimeInterval(8)
    while proc.isRunning && Date() < captureDeadline {
        Thread.sleep(forTimeInterval: 0.05)
    }
    if proc.isRunning {
        proc.terminate()
        return .failure("window_not_found")
    }

    guard let data = FileManager.default.contents(atPath: tmpPath), !data.isEmpty else {
        // No/empty output: re-check the grant (revoked mid-flight) else the
        // window vanished. The 3-code contract has no generic capture-failure
        // code, so collapse the latter to window_not_found.
        guard CGPreflightScreenCaptureAccess() else {
            return .failure("screen_recording_denied")
        }
        return .failure("window_not_found")
    }

    guard let rep = NSBitmapImageRep(data: data) else {
        return .failure("window_not_found")
    }
    return .success(data.base64EncodedString(), rep.pixelsWide, rep.pixelsHigh)
}
