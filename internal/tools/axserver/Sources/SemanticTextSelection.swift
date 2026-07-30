import ApplicationServices
import AppKit
import Foundation

struct SemanticTextRangeV1: Codable, Equatable {
    let location: Int
    let length: Int

    init(location: Int, length: Int) {
        self.location = location
        self.length = length
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["location", "length"], field: "semantic text range")
        location = try container.decode(Int.self, forKey: strictMutationKey("location"))
        length = try container.decode(Int.self, forKey: strictMutationKey("length"))
        guard location >= 0, length > 0,
              location <= Int.max - length else {
            throw StrictMutationWireError.invalid("semantic text range is invalid")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(location, forKey: strictMutationKey("location"))
        try container.encode(length, forKey: strictMutationKey("length"))
    }
}

struct SemanticTextSelectionRequestV1: Codable, Equatable {
    let schemaVersion: Int
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let ref: String
    let path: String
    let expectedRole: String
    let expectedFingerprint: String
    let range: SemanticTextRangeV1
    let fallbackPolicy: String
    let commitDeadlineAt: String

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "pid", "bundle_id", "window_id", "ref", "path",
            "expected_role", "expected_fingerprint", "range", "fallback_policy",
            "commit_deadline_at",
        ], field: "semantic_text_selection params")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        pid = try container.decode(Int.self, forKey: strictMutationKey("pid"))
        bundleID = try container.decode(String.self, forKey: strictMutationKey("bundle_id"))
        let rawWindowID = try container.decode(Int.self, forKey: strictMutationKey("window_id"))
        ref = try container.decode(String.self, forKey: strictMutationKey("ref"))
        path = try container.decode(String.self, forKey: strictMutationKey("path"))
        expectedRole = try container.decode(String.self, forKey: strictMutationKey("expected_role"))
        expectedFingerprint = try container.decode(
            String.self, forKey: strictMutationKey("expected_fingerprint"))
        range = try container.decode(SemanticTextRangeV1.self, forKey: strictMutationKey("range"))
        fallbackPolicy = try container.decode(
            String.self, forKey: strictMutationKey("fallback_policy"))
        commitDeadlineAt = try container.decode(
            String.self, forKey: strictMutationKey("commit_deadline_at"))
        guard schemaVersion == 1, pid > 0,
              strictMutationIdentity(bundleID),
              let exactWindowID = UInt32(exactly: rawWindowID), exactWindowID > 0,
              strictMutationIdentity(ref), ref.count > 1, ref.first == "e",
              ref.dropFirst().allSatisfy(\.isNumber),
              strictMutationIdentity(path),
              path == "window[0]" || path.hasPrefix("window[0]/"),
              strictMutationIdentity(expectedRole),
              strictMutationIdentity(expectedFingerprint),
              fallbackPolicy == "coordinate_drag",
              strictMutationIdentity(commitDeadlineAt),
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid("invalid semantic_text_selection authority")
        }
        windowID = exactWindowID
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(pid, forKey: strictMutationKey("pid"))
        try container.encode(bundleID, forKey: strictMutationKey("bundle_id"))
        try container.encode(windowID, forKey: strictMutationKey("window_id"))
        try container.encode(ref, forKey: strictMutationKey("ref"))
        try container.encode(path, forKey: strictMutationKey("path"))
        try container.encode(expectedRole, forKey: strictMutationKey("expected_role"))
        try container.encode(expectedFingerprint, forKey: strictMutationKey("expected_fingerprint"))
        try container.encode(range, forKey: strictMutationKey("range"))
        try container.encode(fallbackPolicy, forKey: strictMutationKey("fallback_policy"))
        try container.encode(commitDeadlineAt, forKey: strictMutationKey("commit_deadline_at"))
    }
}

struct SemanticTextSelectionRPCRequestV1: Codable, Equatable {
    let id: Int64
    let method: String
    let params: SemanticTextSelectionRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"],
            field: "semantic_text_selection envelope")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(
            SemanticTextSelectionRequestV1.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "semantic_text_selection" else {
            throw StrictMutationWireError.invalid("invalid semantic_text_selection envelope")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(id, forKey: strictMutationKey("id"))
        try container.encode(method, forKey: strictMutationKey("method"))
        try container.encode(params, forKey: strictMutationKey("params"))
    }
}

func decodeSemanticTextSelectionRPCRequestV1(
    _ payload: Data
) throws -> SemanticTextSelectionRPCRequestV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticTextSelectionRPCRequestV1.self, from: payload)
}

struct SemanticTextSelectionResultV1: Codable, Equatable {
    let schemaVersion: Int
    let status: String
    let selectionCommitted: Bool
    let phase: String
    let failureCode: String?
    let retrySafe: Bool
    let postcondition: String?
    let selectedRange: SemanticTextRangeV1?

    init(
        status: String, selectionCommitted: Bool, phase: String,
        failureCode: String?, postcondition: String?, selectedRange: SemanticTextRangeV1?
    ) {
        schemaVersion = 1
        self.status = status
        self.selectionCommitted = selectionCommitted
        self.phase = phase
        self.failureCode = failureCode
        retrySafe = false
        self.postcondition = postcondition
        self.selectedRange = selectedRange
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "status", "selection_committed", "phase",
            "failure_code", "retry_safe", "postcondition", "selected_range",
        ], field: "semantic_text_selection result")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        status = try container.decode(String.self, forKey: strictMutationKey("status"))
        selectionCommitted = try container.decode(
            Bool.self, forKey: strictMutationKey("selection_committed"))
        phase = try container.decode(String.self, forKey: strictMutationKey("phase"))
        failureCode = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("failure_code"))
        retrySafe = try container.decode(Bool.self, forKey: strictMutationKey("retry_safe"))
        postcondition = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("postcondition"))
        selectedRange = try container.decodeIfPresent(
            SemanticTextRangeV1.self, forKey: strictMutationKey("selected_range"))
        guard validateSemanticTextSelectionResult(self) else {
            throw StrictMutationWireError.invalid("invalid semantic_text_selection result tagged union")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(status, forKey: strictMutationKey("status"))
        try container.encode(selectionCommitted, forKey: strictMutationKey("selection_committed"))
        try container.encode(phase, forKey: strictMutationKey("phase"))
        try container.encode(failureCode, forKey: strictMutationKey("failure_code"))
        try container.encode(retrySafe, forKey: strictMutationKey("retry_safe"))
        try container.encode(postcondition, forKey: strictMutationKey("postcondition"))
        try container.encode(selectedRange, forKey: strictMutationKey("selected_range"))
    }
}

func decodeSemanticTextSelectionResultV1(_ payload: Data) throws -> SemanticTextSelectionResultV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticTextSelectionResultV1.self, from: payload)
}

private func validateSemanticTextSelectionResult(_ result: SemanticTextSelectionResultV1) -> Bool {
    guard result.schemaVersion == 1, !result.retrySafe else { return false }
    switch result.status {
    case "verified":
        return result.selectionCommitted && result.phase == "post_verification" &&
            result.failureCode == nil && result.postcondition == "selected_range_matches" &&
            result.selectedRange != nil
    case "completed_unverified":
        guard result.selectionCommitted && result.phase == "post_verification" &&
                result.postcondition == nil else { return false }
        if result.failureCode == "selected_range_mismatch" {
            return result.selectedRange != nil
        }
        return Set([
            "selected_range_not_observed", "interference_detection_unavailable",
        ]).contains(result.failureCode ?? "") && result.selectedRange == nil
    case "user_interference":
        return result.phase == "user_interference" &&
            result.failureCode == "physical_input_interference" &&
            result.postcondition == nil && result.selectedRange == nil
    case "fallback_required":
        return !result.selectionCommitted && result.phase == "preflight" &&
            result.failureCode == "ax_text_range_unsupported" &&
            result.postcondition == nil && result.selectedRange == nil
    case "failed":
        let preflight = Set([
            "invalid_request", "request_expired", "process_not_live",
            "process_identity_mismatch", "window_not_found", "window_ambiguous",
            "path_not_found", "role_mismatch", "fingerprint_mismatch",
            "fingerprint_not_found", "fingerprint_ambiguous", "sensitive_target",
            "enabled_unknown", "target_disabled",
            "interference_detection_unavailable",
        ])
        let exactPhase = result.failureCode.map { code in
            preflight.contains(code) ? result.phase == "preflight" :
                (code == "ax_selection_failed" && result.phase == "action")
        } ?? false
        return !result.selectionCommitted && exactPhase &&
            result.postcondition == nil && result.selectedRange == nil
    default:
        return false
    }
}

enum SemanticTextSelectionWindowResolution {
    case unique
    case missing
    case ambiguous
}

enum SemanticTextRangeObservation {
    case range(SemanticTextRangeV1)
    case unavailable
    case unsupported
}

struct SemanticTextSelectionTarget {
    let role: String
    let fingerprint: String
    let enabled: Bool?
    let sensitive: Bool
    let supportsParameterizedTextRange: Bool
    let selectedTextRangeSettable: Bool
    let setSelectedRange: (SemanticTextRangeV1) -> AXError
    let observeSelectedRange: (TimeInterval) -> SemanticTextRangeObservation
}

struct SemanticTextSelectionDependencies {
    let isPIDLive: (Int) -> Bool
    let bundleIDForPID: (Int) -> String?
    let resolveWindow: (Int, UInt32) -> SemanticTextSelectionWindowResolution
    let resolveTarget: (Int, UInt32, String) -> SemanticTextSelectionTarget?
    let countFingerprint: (Int, UInt32, String) -> Int
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let now: () -> Date
    let sleep: (TimeInterval) -> Void
}

private func semanticSelectionFailure(
    _ code: String, phase: String = "preflight"
) -> SemanticTextSelectionResultV1 {
    .init(
        status: "failed", selectionCommitted: false, phase: phase,
        failureCode: code, postcondition: nil, selectedRange: nil)
}

private func semanticSelectionUserInterference(
    selectionCommitted: Bool
) -> SemanticTextSelectionResultV1 {
    .init(
        status: "user_interference", selectionCommitted: selectionCommitted,
        phase: "user_interference", failureCode: "physical_input_interference",
        postcondition: nil, selectedRange: nil)
}

private func semanticSelectionMonitoringUnavailable(
    selectionCommitted: Bool
) -> SemanticTextSelectionResultV1 {
    .init(
        status: selectionCommitted ? "completed_unverified" : "failed",
        selectionCommitted: selectionCommitted,
        phase: selectionCommitted ? "post_verification" : "preflight",
        failureCode: "interference_detection_unavailable",
        postcondition: nil, selectedRange: nil)
}

func runSemanticTextSelection(
    request: SemanticTextSelectionRequestV1,
    dependencies: SemanticTextSelectionDependencies
) -> SemanticTextSelectionResultV1 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return semanticSelectionFailure("invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else { return semanticSelectionFailure("request_expired") }
    guard horizon <= 3 else { return semanticSelectionFailure("invalid_request") }
    guard dependencies.isPIDLive(request.pid) else {
        return semanticSelectionFailure("process_not_live")
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return semanticSelectionFailure("process_identity_mismatch")
    }
    switch dependencies.resolveWindow(request.pid, request.windowID) {
    case .missing: return semanticSelectionFailure("window_not_found")
    case .ambiguous: return semanticSelectionFailure("window_ambiguous")
    case .unique: break
    }
    guard let target = dependencies.resolveTarget(request.pid, request.windowID, request.path) else {
        return semanticSelectionFailure("path_not_found")
    }
    guard target.role == request.expectedRole else {
        return semanticSelectionFailure("role_mismatch")
    }
    guard target.fingerprint == request.expectedFingerprint else {
        return semanticSelectionFailure("fingerprint_mismatch")
    }
    switch dependencies.countFingerprint(request.pid, request.windowID, request.expectedFingerprint) {
    case 0: return semanticSelectionFailure("fingerprint_not_found")
    case 1: break
    default: return semanticSelectionFailure("fingerprint_ambiguous")
    }
    guard target.sensitive == false else {
        return semanticSelectionFailure("sensitive_target")
    }
    guard target.enabled == true else {
        return semanticSelectionFailure(target.enabled == nil ? "enabled_unknown" : "target_disabled")
    }
    guard target.supportsParameterizedTextRange,
          target.selectedTextRangeSettable else {
        return .init(
            status: "fallback_required", selectionCommitted: false,
            phase: "preflight", failureCode: "ax_text_range_unsupported",
            postcondition: nil, selectedRange: nil)
    }
    guard dependencies.now() < deadline else {
        return semanticSelectionFailure("request_expired")
    }
    guard let initialPhysicalInput = dependencies.observePhysicalInput() else {
        return semanticSelectionMonitoringUnavailable(selectionCommitted: false)
    }
    let initialPhysicalAssessment = assessPhysicalInputInterferenceV1(
        baseline: initialPhysicalInput,
        current: initialPhysicalInput,
        expectedPointer: initialPhysicalInput.pointer,
        expectedSyntheticEvents: [])
    switch initialPhysicalAssessment {
    case .interference:
        return semanticSelectionUserInterference(selectionCommitted: false)
    case .unavailable:
        return semanticSelectionMonitoringUnavailable(selectionCommitted: false)
    case .unchanged:
        break
    }
    var physicalInputBaseline: PhysicalInputInterferenceSnapshotV1? = initialPhysicalInput
    var postCommitPhysicalAssessment: PhysicalInputInterferenceAssessmentV1 = .unchanged
    let assessPhysicalCheckpoint: () -> PhysicalInputInterferenceAssessmentV1 = {
        guard postCommitPhysicalAssessment == .unchanged else {
            return postCommitPhysicalAssessment
        }
        let current = dependencies.observePhysicalInput()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: physicalInputBaseline,
            current: current,
            expectedPointer: physicalInputBaseline?.pointer,
            // AXUIElementSetAttributeValue does not post through Kocoro's
            // private CGEventSource, so the only admissible own-event delta is
            // exactly empty.
            expectedSyntheticEvents: [])
        if assessment == .unchanged {
            physicalInputBaseline = current
        } else {
            postCommitPhysicalAssessment = assessment
        }
        return assessment
    }
    let setResult = target.setSelectedRange(request.range)
    let postSetPhysicalAssessment = assessPhysicalCheckpoint()
    let selectionCommitted = setResult == .success
    switch postSetPhysicalAssessment {
    case .interference:
        return semanticSelectionUserInterference(
            selectionCommitted: selectionCommitted)
    case .unavailable:
        return semanticSelectionMonitoringUnavailable(
            selectionCommitted: selectionCommitted)
    case .unchanged:
        break
    }
    if setResult == .attributeUnsupported || setResult == .parameterizedAttributeUnsupported {
        return .init(
            status: "fallback_required", selectionCommitted: false,
            phase: "preflight", failureCode: "ax_text_range_unsupported",
            postcondition: nil, selectedRange: nil)
    }
    guard setResult == .success else {
        return semanticSelectionFailure("ax_selection_failed", phase: "action")
    }
    let verification: TargetedAXPostconditionOutcomeV1<SemanticTextRangeV1> =
        runTargetedAXPostconditionVerificationV1(
            now: dependencies.now,
            sleep: dependencies.sleep,
            observeWithTimeout: { timeout in
                switch assessPhysicalCheckpoint() {
                case .interference:
                    return .terminal(
                        failureCode: "physical_input_interference", observation: nil)
                case .unavailable:
                    return .terminal(
                        failureCode: "interference_detection_unavailable", observation: nil)
                case .unchanged:
                    break
                }
                let rangeObservation = target.observeSelectedRange(timeout)
                switch assessPhysicalCheckpoint() {
                case .interference:
                    return .terminal(
                        failureCode: "physical_input_interference", observation: nil)
                case .unavailable:
                    return .terminal(
                        failureCode: "interference_detection_unavailable", observation: nil)
                case .unchanged:
                    break
                }
                switch rangeObservation {
                case let .range(observed) where observed == request.range:
                    return .matched(observed)
                case let .range(observed):
                    return .retryable(
                        failureCode: "selected_range_mismatch", observation: observed)
                case .unavailable:
                    return .retryable(
                        failureCode: "selected_range_not_observed", observation: nil)
                case .unsupported:
                    // The setter already committed successfully. Falling back now
                    // could duplicate a selection, so stop at an honest unverified
                    // result instead of silently switching to coordinate drag.
                    return .terminal(
                        failureCode: "selected_range_not_observed", observation: nil)
                }
            })
    // The last targeted read can match while physical input races the return
    // path. Take one final checkpoint before publishing either a verified or
    // ordinary unverified result, and let physical safety take precedence.
    _ = assessPhysicalCheckpoint()
    switch postCommitPhysicalAssessment {
    case .interference:
        return semanticSelectionUserInterference(selectionCommitted: true)
    case .unavailable:
        return semanticSelectionMonitoringUnavailable(selectionCommitted: true)
    case .unchanged:
        break
    }
    switch verification {
    case let .verified(observed, _):
        return .init(
            status: "verified", selectionCommitted: true, phase: "post_verification",
            failureCode: nil, postcondition: "selected_range_matches",
            selectedRange: observed)
    case let .inconclusive(failureCode, observed, _):
        return .init(
            status: "completed_unverified", selectionCommitted: true,
            phase: "post_verification", failureCode: failureCode,
            postcondition: nil, selectedRange: observed)
    }
}

private enum SemanticSelectionResolvedWindow {
    case unique(AXUIElement)
    case missing
    case ambiguous
}

private func resolveSemanticSelectionWindow(pid: Int, windowID: UInt32) -> SemanticSelectionResolvedWindow {
    let candidates = currentCGWindowIdentityCandidates()
    guard candidates.contains(where: {
        $0.windowID == Int(windowID) && $0.ownerPID == pid && $0.layer == 0
    }) else { return .missing }
    let app = AXUIElementCreateApplication(Int32(pid))
    var exact: [AXUIElement] = []
    var ambiguous = false
    for window in axWindows(app) {
        guard let frame = elementFrame(window) else { continue }
        let matches = matchingWindowCandidates(
            .init(
                pid: pid, title: axString(window, "AXTitle") ?? "",
                frame: .init(x: frame.x, y: frame.y, width: frame.width, height: frame.height)),
            candidates: candidates)
        if matches.count > 1 && matches.contains(where: { $0.windowID == Int(windowID) }) {
            ambiguous = true
        } else if matches.count == 1, matches[0].windowID == Int(windowID) {
            exact.append(window)
        }
    }
    if ambiguous || exact.count > 1 { return .ambiguous }
    return exact.first.map(SemanticSelectionResolvedWindow.unique) ?? .missing
}

private func semanticSelectionFingerprint(_ element: AXUIElement) -> String? {
    guard let role = axString(element, "AXRole") else { return nil }
    let attributes = AXElementSnapshotAttributes(
        role: role,
        subrole: axString(element, "AXSubrole"),
        identifier: axString(element, "AXIdentifier"),
        title: axString(element, "AXTitle"),
        description: axString(element, "AXDescription"),
        value: nil,
        valueRedacted: true,
        protectedContent: axBool(element, "AXProtectedContent") ?? false,
        enabled: axBool(element, "AXEnabled") ?? true,
        focused: axBool(element, "AXFocused") ?? false,
        selected: axBool(element, "AXSelected") ?? false,
        actions: axActions(element),
        frame: nil)
    return stableElementFingerprint(attributes)
}

private func matchingSemanticSelectionFingerprint(
    in window: AXUIElement, fingerprint: String
) -> [AXUIElement] {
    var matches: [AXUIElement] = []
    func walk(_ element: AXUIElement) {
        if semanticSelectionFingerprint(element) == fingerprint { matches.append(element) }
        for child in axChildren(element) ?? [] { walk(child) }
    }
    for child in axChildren(window) ?? [] { walk(child) }
    return matches
}

private func semanticSelectionTarget(_ element: AXUIElement) -> SemanticTextSelectionTarget? {
    guard let role = axString(element, "AXRole"),
          let fingerprint = semanticSelectionFingerprint(element) else { return nil }
    let metadata = axValueSensitivityMetadata(element, role: role)
    var parameterized: CFArray?
    let parameterizedResult = AXUIElementCopyParameterizedAttributeNames(element, &parameterized)
    let names = parameterized as? [String] ?? []
    let supportsRange = parameterizedResult == .success &&
        (names.contains(kAXStringForRangeParameterizedAttribute as String) ||
         names.contains(kAXBoundsForRangeParameterizedAttribute as String))
    var settable = DarwinBoolean(false)
    let settableResult = AXUIElementIsAttributeSettable(
        element, kAXSelectedTextRangeAttribute as CFString, &settable)
    return .init(
        role: role, fingerprint: fingerprint,
        enabled: axBool(element, "AXEnabled"),
        sensitive: isSensitiveAXValue(metadata),
        supportsParameterizedTextRange: supportsRange,
        selectedTextRangeSettable: settableResult == .success && settable.boolValue,
        setSelectedRange: { range in
            var raw = CFRange(location: range.location, length: range.length)
            guard let value = AXValueCreate(.cfRange, &raw) else { return .illegalArgument }
            return AXUIElementSetAttributeValue(
                element, kAXSelectedTextRangeAttribute as CFString, value)
        },
        observeSelectedRange: { timeout in
            let outcome = withTargetedAXMessagingTimeoutV1(
                element: element,
                timeout: timeout,
                operation: { () -> SemanticTextRangeObservation in
                    var raw: CFTypeRef?
                    let error = AXUIElementCopyAttributeValue(
                        element, kAXSelectedTextRangeAttribute as CFString, &raw)
                    if error == .attributeUnsupported { return .unsupported }
                    guard error == .success, let raw,
                          CFGetTypeID(raw) == AXValueGetTypeID() else { return .unavailable }
                    var range = CFRange()
                    guard AXValueGetValue(raw as! AXValue, .cfRange, &range),
                          range.location >= 0, range.length > 0 else { return .unavailable }
                    return .range(.init(location: range.location, length: range.length))
                })
            guard case let .completed(observation) = outcome else { return .unavailable }
            return observation
        })
}

let productionSemanticTextSelectionDependencies = SemanticTextSelectionDependencies(
    isPIDLive: { pid in
        refreshAppKitState()
        guard let app = NSRunningApplication(processIdentifier: Int32(pid)) else { return false }
        return !app.isTerminated
    },
    bundleIDForPID: { pid in
        NSRunningApplication(processIdentifier: Int32(pid))?.bundleIdentifier
    },
    resolveWindow: { pid, windowID in
        switch resolveSemanticSelectionWindow(pid: pid, windowID: windowID) {
        case .unique: return .unique
        case .missing: return .missing
        case .ambiguous: return .ambiguous
        }
    },
    resolveTarget: { pid, windowID, path in
        guard case let .unique(window) = resolveSemanticSelectionWindow(pid: pid, windowID: windowID),
              let element = resolveElement(in: window, path: path) else { return nil }
        return semanticSelectionTarget(element)
    },
    countFingerprint: { pid, windowID, fingerprint in
        guard case let .unique(window) = resolveSemanticSelectionWindow(pid: pid, windowID: windowID) else {
            return 0
        }
        return matchingSemanticSelectionFingerprint(in: window, fingerprint: fingerprint).count
    },
    observePhysicalInput: observePhysicalInputInterferenceV1,
    now: Date.init,
    sleep: Thread.sleep(forTimeInterval:))
