import Foundation
import AppKit
import CoreGraphics

struct WindowCaptureCandidate: Equatable {
    let id: CGWindowID
    let ownerPID: Int
    let layer: Int
    let area: CGFloat
    let title: String
    let onScreen: Bool
    let frame: AXFrame
}

private func captureWindowFramesMatch(_ lhs: AXFrame, _ rhs: AXFrame, tolerance: Double = 2) -> Bool {
    abs(lhs.x - rhs.x) <= tolerance &&
        abs(lhs.y - rhs.y) <= tolerance &&
        abs(lhs.width - rhs.width) <= tolerance &&
        abs(lhs.height - rhs.height) <= tolerance
}

func selectExactWindowCaptureCandidate(
    pid: Int,
    windowID: Int,
    expectedBounds: AXFrame,
    candidates: [WindowCaptureCandidate]
) -> WindowCaptureCandidate? {
    let matches = candidates.filter {
        Int($0.id) == windowID && $0.ownerPID == pid && $0.layer == 0 &&
            $0.onScreen && captureWindowFramesMatch($0.frame, expectedBounds)
    }
    return matches.count == 1 ? matches[0] : nil
}

private func currentWindowCaptureCandidates() -> [WindowCaptureCandidate]? {
    let options: CGWindowListOption = [.optionAll, .excludeDesktopElements]
    guard let infoList = CGWindowListCopyWindowInfo(options, kCGNullWindowID) as? [[String: Any]] else {
        return nil
    }
    return infoList.compactMap { info in
        guard let owner = (info[kCGWindowOwnerPID as String] as? NSNumber)?.intValue,
              let number = (info[kCGWindowNumber as String] as? NSNumber)?.uint32Value,
              let boundsDict = info[kCGWindowBounds as String] as? NSDictionary,
              let rect = CGRect(dictionaryRepresentation: boundsDict as CFDictionary) else {
            return nil
        }
        let layer = (info[kCGWindowLayer as String] as? NSNumber)?.intValue ?? -1
        let title = info[kCGWindowName as String] as? String ?? ""
        let onScreen = (info[kCGWindowIsOnscreen as String] as? NSNumber)?.boolValue ?? false
        return WindowCaptureCandidate(
            id: number,
            ownerPID: owner,
            layer: layer,
            area: rect.width * rect.height,
            title: title,
            onScreen: onScreen,
            frame: AXFrame(
                x: Double(rect.origin.x), y: Double(rect.origin.y),
                width: Double(rect.width), height: Double(rect.height)))
    }
}

/// Result of capture_window. Encoded into the NDJSON `result` field via AnyCodable.
struct CaptureWindowResult: Codable {
    let ok: Bool
    let code: String?
    let imageBase64: String?
    let width: Int?
    let height: Int?
    let contentSignature: String?

    enum CodingKeys: String, CodingKey {
        case ok, code
        case imageBase64 = "image_base64"
        case contentSignature = "content_signature"
        case width, height
    }

    static func failure(_ code: String) -> CaptureWindowResult {
        CaptureWindowResult(
            ok: false, code: code, imageBase64: nil,
            width: nil, height: nil, contentSignature: nil)
    }
    static func success(
        _ base64: String,
        _ w: Int,
        _ h: Int,
        contentSignature: String? = nil
    ) -> CaptureWindowResult {
        CaptureWindowResult(
            ok: true, code: nil, imageBase64: base64,
            width: w, height: h, contentSignature: contentSignature)
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
/// Window selection: callers that supply `windowID` and `expectedBounds` get
/// exact PID/ID/bounds admission with an on-screen requirement and a post-
/// capture recheck. Legacy callers without exact identity retain title/largest
/// selection for compatibility.
func captureWindow(
    pid: Int?,
    appName: String?,
    windowTitle: String?,
    windowID: Int? = nil,
    expectedBounds: AXFrame? = nil,
    signatureRoles: [String]? = nil,
    signatureMaxLabels: Int = 50
) -> CaptureWindowResult {
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
    guard let allCandidates = currentWindowCaptureCandidates() else {
        return .failure("window_not_found")
    }
    let candidates = allCandidates.filter { $0.ownerPID == targetPID && $0.layer == 0 }
    guard !candidates.isEmpty else {
        return .failure("window_not_found")
    }

    // Prefer an on-screen window, then the largest by area (the main window is
    // almost always the biggest; thin toolbar/strip windows rank below it).
    let better: (WindowCaptureCandidate, WindowCaptureCandidate) -> Bool = { a, b in
        if a.onScreen != b.onScreen { return !a.onScreen }   // off-screen ranks lower
        return a.area < b.area
    }
    let chosen: WindowCaptureCandidate
    if let exactID = windowID {
        guard let expectedBounds,
              let exact = selectExactWindowCaptureCandidate(
                pid: targetPID,
                windowID: exactID,
                expectedBounds: expectedBounds,
                candidates: allCandidates) else {
            return .failure("window_not_found")
        }
        chosen = exact
    } else if let want = windowTitle, !want.isEmpty {
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
    defer { try? FileManager.default.removeItem(atPath: tmpPath) }
    // Exact annotation captures must exclude attached surfaces because their
    // union no longer shares the selected window's coordinate frame. Legacy
    // best-window captures retain their historical, presentation-oriented
    // behavior.
    let captureArguments: [String]
    if windowID != nil {
        guard let exactWindowID = UInt32(exactly: chosen.id) else {
            return .failure("window_not_found")
        }
        captureArguments = coordinateWindowScreencaptureArguments(
            windowID: exactWindowID,
            outputURL: URL(fileURLWithPath: tmpPath))
    } else {
        // -x: silent · -o: omit window shadow · -l<id>: capture that window's content
        captureArguments = ["-x", "-o", "-l\(chosen.id)", tmpPath]
    }
    do {
        try runCoordinateWindowCaptureProcess(
            executableURL: URL(fileURLWithPath: "/usr/sbin/screencapture"),
            arguments: captureArguments,
            timeout: 8)
    } catch {
        return .failure("window_not_found")
    }

    // Every exact capture is bound to the same PID/window/bounds before and
    // after screencapture. Annotation callers additionally request a content
    // signature. A max-label budget of zero is the explicit visual-only path:
    // it returns exact current pixels without claiming coordinate authority.
    var postCaptureContentSignature: String?
    if let exactID = windowID, let expectedBounds {
        guard let postCandidates = currentWindowCaptureCandidates(),
              selectExactWindowCaptureCandidate(
                pid: targetPID,
                windowID: exactID,
                expectedBounds: expectedBounds,
                candidates: postCandidates) != nil else {
            return .failure("window_not_found")
        }
        if signatureMaxLabels > 0 {
            guard let postAnnotation = annotateElements(
                    pid: targetPID,
                    roles: signatureRoles,
                    maxLabels: signatureMaxLabels),
                  postAnnotation.windowID == exactID,
                  let postFrame = postAnnotation.windowFrame,
                  captureWindowFramesMatch(postFrame, expectedBounds),
                  !postAnnotation.contentSignature.isEmpty else {
                return .failure("window_content_unavailable")
            }
            postCaptureContentSignature = postAnnotation.contentSignature
        }
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
    return .success(
        data.base64EncodedString(), rep.pixelsWide, rep.pixelsHigh,
        contentSignature: postCaptureContentSignature)
}
