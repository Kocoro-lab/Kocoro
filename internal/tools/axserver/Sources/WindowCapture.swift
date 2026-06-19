import Foundation
import AppKit
import CoreGraphics
import ScreenCaptureKit

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

/// Capture a single window's pixels for the given pid (or app name), via
/// ScreenCaptureKit. Synchronous: bridges SCK's async APIs with a semaphore so
/// it works inside the ax_server NDJSON request loop.
///
/// Window selection: if windowTitle is given, the first on-screen window of the
/// pid whose title contains it (case-insensitive); else the largest on-screen
/// window of the pid.
func captureWindow(pid: Int?, appName: String?, windowTitle: String?) -> CaptureWindowResult {
    // Permission gate first — passive check, never prompts (the Desktop drives
    // the explicit request flow through PermissionsTab → request_permission).
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

    // Fetch shareable content (async → sync).
    var shareable: SCShareableContent?
    let contentSem = DispatchSemaphore(value: 0)
    SCShareableContent.getExcludingDesktopWindows(false, onScreenWindowsOnly: true) { content, _ in
        // Error intentionally dropped: nil content is handled below as screen_recording_denied.
        shareable = content
        contentSem.signal()
    }
    _ = contentSem.wait(timeout: .now() + 5.0)
    guard let content = shareable else {
        return .failure("screen_recording_denied")
    }

    // Pick the window.
    let candidates = content.windows.filter {
        // Int32(pid_t) → Int widening is intentional and safe on 64-bit platforms.
        Int($0.owningApplication?.processID ?? -1) == targetPID && $0.isOnScreen
    }
    guard !candidates.isEmpty else {
        return .failure("window_not_found")
    }
    let chosen: SCWindow
    if let want = windowTitle, !want.isEmpty {
        guard let match = candidates.first(where: {
            ($0.title ?? "").range(of: want, options: .caseInsensitive) != nil
        }) else {
            return .failure("window_not_found")
        }
        chosen = match
    } else {
        chosen = candidates.max(by: { ($0.frame.width * $0.frame.height) < ($1.frame.width * $1.frame.height) })!
    }

    // Capture the chosen window (async → sync).
    let filter = SCContentFilter(desktopIndependentWindow: chosen)
    let config = SCStreamConfiguration()
    // SCWindow.frame is in points; SCStreamConfiguration.width/height are in pixels.
    // Multiply by the backing scale factor of the display the window is on (2.0 on
    // Retina) so the capture is at native resolution rather than half-res.
    let windowCenter = CGPoint(
        x: chosen.frame.midX,
        y: chosen.frame.midY
    )
    let scale: CGFloat = NSScreen.screens.first(where: { $0.frame.contains(windowCenter) })?.backingScaleFactor
        ?? NSScreen.main?.backingScaleFactor
        ?? 1.0
    config.width = Int((chosen.frame.width * scale).rounded())
    config.height = Int((chosen.frame.height * scale).rounded())
    config.showsCursor = false

    var captured: CGImage?
    let shotSem = DispatchSemaphore(value: 0)
    SCScreenshotManager.captureImage(contentFilter: filter, configuration: config) { image, _ in
        captured = image
        shotSem.signal()
    }
    _ = shotSem.wait(timeout: .now() + 5.0)
    guard let cgImage = captured else {
        // Re-check the grant: a nil capture can mean the permission was revoked
        // between the preflight check above and the actual SCK call (a race).
        guard CGPreflightScreenCaptureAccess() else {
            return .failure("screen_recording_denied")
        }
        // Grant still present → window vanished between enumeration and capture,
        // or an internal SCK failure. The 3-code contract has no generic
        // capture-failure code, so collapse to window_not_found.
        return .failure("window_not_found")
    }

    // CGImage → PNG → base64.
    let rep = NSBitmapImageRep(cgImage: cgImage)
    guard let pngData = rep.representation(using: .png, properties: [:]) else {
        // PNG encoding failure collapsed to window_not_found — documented
        // limitation of the 3-code contract (no encoding/internal-error code).
        return .failure("window_not_found")
    }
    return .success(pngData.base64EncodedString(), cgImage.width, cgImage.height)
}
