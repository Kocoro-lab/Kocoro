import ApplicationServices
import AppKit
import Foundation

enum SemanticPressRequestError: Error, CustomStringConvertible {
    case invalid(String)

    var description: String {
        switch self {
        case let .invalid(message): message
        }
    }
}

struct SemanticPressRequest {
    let pid: Int
    let windowID: Int
    let path: String
    let expectedRole: String
    let expectedFingerprint: String
    let fallbackPolicy: String

    init(
        pid: Int?,
        windowID: Int?,
        path: String?,
        expectedRole: String?,
        expectedFingerprint: String?,
        fallbackPolicy: String?
    ) throws {
        guard let pid, pid > 0 else {
            throw SemanticPressRequestError.invalid("semantic_press requires a positive pid")
        }
        guard let windowID, windowID > 0 else {
            throw SemanticPressRequestError.invalid("semantic_press requires a positive window_id")
        }
        guard let path, !path.isEmpty else {
            throw SemanticPressRequestError.invalid("semantic_press requires path")
        }
        guard let expectedRole, !expectedRole.isEmpty else {
            throw SemanticPressRequestError.invalid("semantic_press requires expected_role")
        }
        guard let expectedFingerprint, !expectedFingerprint.isEmpty else {
            throw SemanticPressRequestError.invalid("semantic_press requires expected_fingerprint")
        }
        guard fallbackPolicy == "none" else {
            throw SemanticPressRequestError.invalid("semantic_press fallback_policy must be 'none'")
        }
        self.pid = pid
        self.windowID = windowID
        self.path = path
        self.expectedRole = expectedRole
        self.expectedFingerprint = expectedFingerprint
        self.fallbackPolicy = "none"
    }
}

struct SemanticPressResult: Encodable {
    let status: String
    let pressCommitted: Bool
    let phase: String
    let failureCode: String?
    let postcondition: String?
    let retrySafe: Bool

    enum CodingKeys: String, CodingKey {
        case status, phase, postcondition
        case pressCommitted = "press_committed"
        case failureCode = "failure_code"
        case retrySafe = "retry_safe"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(status, forKey: .status)
        try container.encode(pressCommitted, forKey: .pressCommitted)
        try container.encode(phase, forKey: .phase)
        if let failureCode {
            try container.encode(failureCode, forKey: .failureCode)
        } else {
            try container.encodeNil(forKey: .failureCode)
        }
        if let postcondition {
            try container.encode(postcondition, forKey: .postcondition)
        } else {
            try container.encodeNil(forKey: .postcondition)
        }
        try container.encode(retrySafe, forKey: .retrySafe)
    }
}

struct SemanticPressTargetSignature: Equatable {
    let fingerprint: String
    let title: String?
    let description: String?
    let value: String?
    let valueRedacted: Bool
    let enabled: Bool?
    let focused: Bool?
    let selected: Bool?
    let actions: [String]
}

struct SemanticPressTarget {
    let role: String
    let fingerprint: String
    let enabled: Bool?
    let actions: [String]
    let signature: SemanticPressTargetSignature
    let performPress: () -> AXError
}

enum SemanticPressWindowResolution {
    case unique
    case missing
    case ambiguous
}

enum SemanticPressTargetObservation {
    case signature(SemanticPressTargetSignature)
    case missing
    case unavailable
}

struct SemanticPressDependencies {
    let isPIDLive: (Int) -> Bool
    let resolveWindow: (Int, Int) -> SemanticPressWindowResolution
    let resolveTarget: (Int, Int, String) -> SemanticPressTarget?
    let countFingerprint: (Int, Int, String) -> Int
    let observeTarget: (Int, Int, String) -> SemanticPressTargetObservation
    let now: () -> TimeInterval
    let sleep: (TimeInterval) -> Void
}

private func semanticPressFailure(_ code: String, phase: String = "preflight") -> SemanticPressResult {
    SemanticPressResult(
        status: "failed",
        pressCommitted: false,
        phase: phase,
        failureCode: code,
        postcondition: nil,
        retrySafe: false)
}

func runSemanticPress(
    _ request: SemanticPressRequest,
    dependencies: SemanticPressDependencies,
    timeout: TimeInterval = 0.5,
    pollInterval: TimeInterval = 0.05
) -> SemanticPressResult {
    guard dependencies.isPIDLive(request.pid) else {
        return semanticPressFailure("pid_not_live")
    }
    switch dependencies.resolveWindow(request.pid, request.windowID) {
    case .missing:
        return semanticPressFailure("window_not_found")
    case .ambiguous:
        return semanticPressFailure("window_ambiguous")
    case .unique:
        break
    }
    guard let target = dependencies.resolveTarget(request.pid, request.windowID, request.path) else {
        return semanticPressFailure("path_not_found")
    }
    guard target.role == request.expectedRole else {
        return semanticPressFailure("role_mismatch")
    }
    guard target.fingerprint == request.expectedFingerprint else {
        return semanticPressFailure("fingerprint_mismatch")
    }
    switch dependencies.countFingerprint(request.pid, request.windowID, request.expectedFingerprint) {
    case 0:
        return semanticPressFailure("fingerprint_not_found")
    case 1:
        break
    default:
        return semanticPressFailure("fingerprint_ambiguous")
    }
    guard let enabled = target.enabled else {
        return semanticPressFailure("enabled_unknown")
    }
    guard enabled else {
        return semanticPressFailure("target_disabled")
    }
    guard target.actions.contains("AXPress") else {
        return semanticPressFailure("ax_press_unavailable")
    }

    guard target.performPress() == .success else {
        return semanticPressFailure("ax_press_failed", phase: "action")
    }

    let boundedTimeout = min(max(timeout, 0), 0.5)
    let boundedInterval = min(max(pollInterval, 0.01), 0.1)
    let deadline = dependencies.now() + boundedTimeout
    var observedChange = false
    repeat {
        switch dependencies.observeTarget(request.pid, request.windowID, request.expectedFingerprint) {
        case let .signature(signature) where signature != target.signature:
            observedChange = true
        case .missing:
            observedChange = true
        case .signature, .unavailable:
            break
        }
        if observedChange { break }
        let remaining = deadline - dependencies.now()
        if remaining <= 0 { break }
        dependencies.sleep(min(boundedInterval, remaining))
    } while true

    return SemanticPressResult(
        status: "completed_unverified",
        pressCommitted: true,
        phase: "post_observation",
        failureCode: observedChange ? "postcondition_not_declared" : "postcondition_not_observed",
        postcondition: nil,
        retrySafe: false)
}

private enum AXWindowResolution {
    case unique(AXUIElement)
    case missing
    case ambiguous
}

private func resolveAXWindow(pid: Int, windowID: Int) -> AXWindowResolution {
    let candidates = currentCGWindowIdentityCandidates()
    guard candidates.contains(where: {
        $0.windowID == windowID && $0.ownerPID == pid && $0.layer == 0
    }) else {
        return .missing
    }

    let appRef = AXUIElementCreateApplication(Int32(pid))
    var matchingWindows: [AXUIElement] = []
    var ambiguous = false
    for window in axWindows(appRef) {
        guard let frameTuple = elementFrame(window) else { continue }
        let observation = WindowIdentityObservation(
            pid: pid,
            title: axString(window, "AXTitle") ?? "",
            frame: AXFrame(
                x: frameTuple.x, y: frameTuple.y,
                width: frameTuple.width, height: frameTuple.height))
        let matches = matchingWindowCandidates(observation, candidates: candidates)
        if matches.count > 1 && matches.contains(where: { $0.windowID == windowID }) {
            ambiguous = true
        } else if matches.count == 1 && matches[0].windowID == windowID {
            matchingWindows.append(window)
        }
    }
    if ambiguous || matchingWindows.count > 1 {
        return .ambiguous
    }
    if let window = matchingWindows.first {
        return .unique(window)
    }
    return .missing
}

private func semanticSnapshotAttributes(_ element: AXUIElement) -> AXElementSnapshotAttributes? {
    guard let role = axString(element, "AXRole") else { return nil }
    let subrole = axString(element, "AXSubrole")
    let identifier = axString(element, "AXIdentifier")
    let title = axString(element, "AXTitle")
    let description = axString(element, "AXDescription")
    let protectedContent = axBool(element, "AXProtectedContent") ?? false
    let valueRedacted = isSensitiveAXValue(axValueSensitivityMetadata(
        element,
        role: role,
        subrole: subrole,
        identifier: identifier,
        title: title,
        description: description,
        protectedContent: protectedContent))
    var value: String?
    if !valueRedacted, let rawValue = axValue(element, "AXValue") {
        let string = "\(rawValue)"
        value = string.count > 200 ? String(string.prefix(200)) + "..." : string
    }
    let frame = elementFrame(element).map {
        AXFrame(x: $0.x, y: $0.y, width: $0.width, height: $0.height)
    }
    return AXElementSnapshotAttributes(
        role: role,
        subrole: subrole,
        identifier: identifier,
        title: title,
        description: description,
        value: value,
        valueRedacted: valueRedacted,
        protectedContent: protectedContent,
        enabled: axBool(element, "AXEnabled") ?? true,
        focused: axBool(element, "AXFocused") ?? false,
        selected: axBool(element, "AXSelected") ?? false,
        actions: axActions(element),
        frame: frame)
}

private func semanticSignature(
    _ attributes: AXElementSnapshotAttributes,
    explicitEnabled: Bool?
) -> SemanticPressTargetSignature {
    return SemanticPressTargetSignature(
        fingerprint: stableElementFingerprint(attributes),
        title: attributes.title,
        description: attributes.description,
        value: attributes.valueRedacted ? nil : attributes.value,
        valueRedacted: attributes.valueRedacted,
        enabled: explicitEnabled,
        focused: attributes.focused,
        selected: attributes.selected,
        actions: attributes.actions.sorted())
}

private func semanticTarget(_ element: AXUIElement) -> SemanticPressTarget? {
    guard let attributes = semanticSnapshotAttributes(element) else { return nil }
    let enabled = axBool(element, "AXEnabled")
    return SemanticPressTarget(
        role: attributes.role,
        fingerprint: stableElementFingerprint(attributes),
        enabled: enabled,
        actions: attributes.actions.sorted(),
        signature: semanticSignature(attributes, explicitEnabled: enabled),
        performPress: { AXUIElementPerformAction(element, kAXPressAction as CFString) })
}

private func matchingFingerprintElements(
    in window: AXUIElement,
    fingerprint: String
) -> [SemanticPressTarget] {
    var matches: [SemanticPressTarget] = []
    func walk(_ element: AXUIElement) {
        if let target = semanticTarget(element), target.fingerprint == fingerprint {
            matches.append(target)
        }
        for child in axChildren(element) ?? [] {
            walk(child)
        }
    }
    for child in axChildren(window) ?? [] {
        walk(child)
    }
    return matches
}

func productionSemanticPressDependencies() -> SemanticPressDependencies {
    SemanticPressDependencies(
        isPIDLive: { pid in
            refreshAppKitState()
            guard let app = NSRunningApplication(processIdentifier: Int32(pid)) else { return false }
            return !app.isTerminated
        },
        resolveWindow: { pid, windowID in
            switch resolveAXWindow(pid: pid, windowID: windowID) {
            case .unique: .unique
            case .missing: .missing
            case .ambiguous: .ambiguous
            }
        },
        resolveTarget: { pid, windowID, path in
            guard case let .unique(window) = resolveAXWindow(pid: pid, windowID: windowID),
                  let element = resolveElement(in: window, path: path) else {
                return nil
            }
            return semanticTarget(element)
        },
        countFingerprint: { pid, windowID, fingerprint in
            guard case let .unique(window) = resolveAXWindow(pid: pid, windowID: windowID) else {
                return 0
            }
            return matchingFingerprintElements(in: window, fingerprint: fingerprint).count
        },
        observeTarget: { pid, windowID, fingerprint in
            guard case let .unique(window) = resolveAXWindow(pid: pid, windowID: windowID) else {
                return .unavailable
            }
            let matches = matchingFingerprintElements(in: window, fingerprint: fingerprint)
            if matches.isEmpty { return .missing }
            guard matches.count == 1 else { return .unavailable }
            return .signature(matches[0].signature)
        },
        now: { ProcessInfo.processInfo.systemUptime },
        sleep: { Thread.sleep(forTimeInterval: $0) })
}
