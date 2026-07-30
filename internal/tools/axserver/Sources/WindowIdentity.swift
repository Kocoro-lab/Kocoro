import ApplicationServices
import AppKit
import CoreGraphics
import Foundation

struct WindowIdentityObservation {
    let pid: Int
    let title: String
    let frame: AXFrame
}

struct CGWindowIdentityCandidate {
    let windowID: Int
    let ownerPID: Int
    let layer: Int
    let title: String?
    let frame: AXFrame
    let isOnScreen: Bool
    let alpha: Double

    init(
        windowID: Int,
        ownerPID: Int,
        layer: Int,
        title: String?,
        frame: AXFrame,
        isOnScreen: Bool = true,
        alpha: Double = 1
    ) {
        self.windowID = windowID
        self.ownerPID = ownerPID
        self.layer = layer
        self.title = title
        self.frame = frame
        self.isOnScreen = isOnScreen
        self.alpha = alpha
    }
}

enum WindowIdentityMatch: Equatable {
    case none
    case unique(Int)
    case ambiguous
}

private func windowFramesMatch(_ lhs: AXFrame, _ rhs: AXFrame, tolerance: Double) -> Bool {
    abs(lhs.x - rhs.x) <= tolerance &&
        abs(lhs.y - rhs.y) <= tolerance &&
        abs(lhs.width - rhs.width) <= tolerance &&
        abs(lhs.height - rhs.height) <= tolerance
}

func matchingWindowCandidates(
    _ observation: WindowIdentityObservation,
    candidates: [CGWindowIdentityCandidate],
    tolerance: Double = 2.0
) -> [CGWindowIdentityCandidate] {
    let geometryMatches = candidates.filter { candidate in
        candidate.ownerPID == observation.pid &&
            candidate.layer == 0 &&
            windowFramesMatch(candidate.frame, observation.frame, tolerance: tolerance)
    }
    // Window names may be unavailable without Screen Recording. Treat an
    // available exact name as an extra discriminator, but never make a blank
    // CG name a permission gate for AX-only semantic actions.
    let exactTitleMatches = geometryMatches.filter {
        guard let title = $0.title, !title.isEmpty else { return false }
        return title == observation.title
    }
    if !exactTitleMatches.isEmpty {
        return exactTitleMatches
    }
    return geometryMatches.filter { $0.title == nil || $0.title?.isEmpty == true }
}

func matchWindowIdentity(
    _ observation: WindowIdentityObservation,
    candidates: [CGWindowIdentityCandidate],
    tolerance: Double = 2.0
) -> WindowIdentityMatch {
    let matches = matchingWindowCandidates(observation, candidates: candidates, tolerance: tolerance)
    switch matches.count {
    case 0:
        return .none
    case 1:
        return .unique(matches[0].windowID)
    default:
        return .ambiguous
    }
}

/// A focused AX window has one additional identity witness: when its process
/// is frontmost, CGWindowList order identifies the visible topmost twin. Keep
/// ordinary/background matching fail-closed because z-order alone does not
/// prove which same-process window owns background focus.
func matchFocusedWindowIdentity(
    _ observation: WindowIdentityObservation,
    candidates: [CGWindowIdentityCandidate],
    targetProcessIsFrontmost: Bool,
    tolerance: Double = 2.0
) -> WindowIdentityMatch {
    let strictMatch = matchWindowIdentity(
        observation,
        candidates: candidates,
        tolerance: tolerance)
    guard strictMatch == .ambiguous, targetProcessIsFrontmost else {
        return strictMatch
    }
    let visibleMatches = matchingWindowCandidates(
        observation,
        candidates: candidates,
        tolerance: tolerance
    ).filter {
        $0.windowID > 0 && $0.isOnScreen && $0.alpha > 0
    }
    guard let topmost = visibleMatches.first else {
        return .ambiguous
    }
    return .unique(topmost.windowID)
}

func currentCGWindowIdentityCandidates() -> [CGWindowIdentityCandidate] {
    guard let entries = CGWindowListCopyWindowInfo(.optionAll, kCGNullWindowID) as? [[String: Any]] else {
        return []
    }
    return entries.compactMap { entry in
        guard let windowNumber = entry[kCGWindowNumber as String] as? NSNumber,
              let ownerPID = entry[kCGWindowOwnerPID as String] as? NSNumber,
              let layer = entry[kCGWindowLayer as String] as? NSNumber,
              let boundsDictionary = entry[kCGWindowBounds as String] as? NSDictionary else {
            return nil
        }
        var bounds = CGRect.zero
        guard CGRectMakeWithDictionaryRepresentation(boundsDictionary, &bounds) else {
            return nil
        }
        return CGWindowIdentityCandidate(
            windowID: windowNumber.intValue,
            ownerPID: ownerPID.intValue,
            layer: layer.intValue,
            title: entry[kCGWindowName as String] as? String,
            frame: AXFrame(
                x: Double(bounds.origin.x),
                y: Double(bounds.origin.y),
                width: Double(bounds.width),
                height: Double(bounds.height)),
            isOnScreen: (entry[kCGWindowIsOnscreen as String] as? NSNumber)?.boolValue ?? false,
            alpha: (entry[kCGWindowAlpha as String] as? NSNumber)?.doubleValue ?? 0)
    }
}

func uniqueWindowID(pid: Int, title: String, frame: AXFrame?) -> Int? {
    guard let frame else { return nil }
    let observation = WindowIdentityObservation(pid: pid, title: title, frame: frame)
    if case let .unique(windowID) = matchWindowIdentity(
        observation,
        candidates: currentCGWindowIdentityCandidates()
    ) {
        return windowID
    }
    return nil
}

func focusedWindowID(pid: Int, title: String, frame: AXFrame?) -> Int? {
    guard let frame else { return nil }
    refreshAppKitState()
    let processID = pid_t(exactly: pid)
    let targetProcessIsFrontmost =
        processID != nil &&
        NSWorkspace.shared.frontmostApplication?.processIdentifier == processID
    let observation = WindowIdentityObservation(pid: pid, title: title, frame: frame)
    if case let .unique(windowID) = matchFocusedWindowIdentity(
        observation,
        candidates: currentCGWindowIdentityCandidates(),
        targetProcessIsFrontmost: targetProcessIsFrontmost
    ) {
        return windowID
    }
    return nil
}

func frontmostNormalCGWindow(
    pid: Int,
    candidates: [CGWindowIdentityCandidate]
) -> CGWindowIdentityCandidate? {
    candidates.first {
        $0.ownerPID == pid &&
            $0.layer == 0 &&
            $0.windowID > 0 &&
            $0.isOnScreen &&
            $0.alpha > 0 &&
            $0.frame.width > 0 &&
            $0.frame.height > 0
    }
}

/// Returns a typed, coordinate-capable observation even when the target app
/// exposes no usable AX window/tree. CGWindow owns the window identity and
/// geometry; AX elements are intentionally empty rather than fabricated.
func readCoordinateWindowTarget(pid: Int) -> ReadTreeResult? {
    refreshAppKitState()
    guard pid > 0,
          let processID = pid_t(exactly: pid),
          let app = NSRunningApplication(processIdentifier: processID),
          !app.isTerminated,
          let bundleID = app.bundleIdentifier,
          !bundleID.isEmpty,
          let window = frontmostNormalCGWindow(
            pid: pid,
            candidates: currentCGWindowIdentityCandidates()
          ) else {
        return nil
    }
    let appName = app.localizedName ?? bundleID
    return ReadTreeResult(
        schemaVersion: 1,
        appName: appName,
        bundleID: bundleID,
        pid: pid,
        windowTitle: window.title ?? "",
        windowID: window.windowID,
        windowFrame: window.frame,
        focusedRef: nil,
        elements: [],
        refPaths: [:])
}
