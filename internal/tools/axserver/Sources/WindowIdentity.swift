import ApplicationServices
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
                height: Double(bounds.height)))
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
