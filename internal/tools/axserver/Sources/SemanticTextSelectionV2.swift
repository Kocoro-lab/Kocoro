import ApplicationServices
import AppKit
import Foundation

struct SemanticTextRangeV2: Codable, Equatable {
    let location: Int
    let length: Int

    init(location: Int, length: Int) {
        self.location = location
        self.length = length
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["location", "length"],
            field: "semantic_text_selection_v2 range")
        location = try container.decode(Int.self, forKey: strictMutationKey("location"))
        length = try container.decode(Int.self, forKey: strictMutationKey("length"))
        guard location >= 0, length > 0, location <= Int.max - length else {
            throw StrictMutationWireError.invalid(
                "semantic_text_selection_v2 range is invalid")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(location, forKey: strictMutationKey("location"))
        try container.encode(length, forKey: strictMutationKey("length"))
    }
}

struct SemanticTextSelectionRequestV2: Codable, Equatable {
    let schemaVersion: Int
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let ref: String
    let path: String
    let expectedRole: String
    let expectedFingerprint: String
    let range: SemanticTextRangeV2
    let fallbackPolicy: String
    let commitDeadlineAt: String

    init(
        schemaVersion: Int = 2, pid: Int, bundleID: String, windowID: UInt32,
        ref: String, path: String, expectedRole: String, expectedFingerprint: String,
        range: SemanticTextRangeV2, fallbackPolicy: String = "report_unsupported",
        commitDeadlineAt: String
    ) {
        self.schemaVersion = schemaVersion
        self.pid = pid
        self.bundleID = bundleID
        self.windowID = windowID
        self.ref = ref
        self.path = path
        self.expectedRole = expectedRole
        self.expectedFingerprint = expectedFingerprint
        self.range = range
        self.fallbackPolicy = fallbackPolicy
        self.commitDeadlineAt = commitDeadlineAt
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "pid", "bundle_id", "window_id", "ref", "path",
            "expected_role", "expected_fingerprint", "range", "fallback_policy",
            "commit_deadline_at",
        ], field: "semantic_text_selection_v2 params")
        schemaVersion = try container.decode(
            Int.self, forKey: strictMutationKey("schema_version"))
        pid = try container.decode(Int.self, forKey: strictMutationKey("pid"))
        bundleID = try container.decode(String.self, forKey: strictMutationKey("bundle_id"))
        let rawWindowID = try container.decode(Int.self, forKey: strictMutationKey("window_id"))
        ref = try container.decode(String.self, forKey: strictMutationKey("ref"))
        path = try container.decode(String.self, forKey: strictMutationKey("path"))
        expectedRole = try container.decode(
            String.self, forKey: strictMutationKey("expected_role"))
        expectedFingerprint = try container.decode(
            String.self, forKey: strictMutationKey("expected_fingerprint"))
        range = try container.decode(
            SemanticTextRangeV2.self, forKey: strictMutationKey("range"))
        fallbackPolicy = try container.decode(
            String.self, forKey: strictMutationKey("fallback_policy"))
        commitDeadlineAt = try container.decode(
            String.self, forKey: strictMutationKey("commit_deadline_at"))
        guard schemaVersion == 2, pid > 0,
              strictMutationIdentity(bundleID),
              let exactWindowID = UInt32(exactly: rawWindowID), exactWindowID > 0,
              strictMutationIdentity(ref), ref.count > 1, ref.first == "e",
              ref.dropFirst().allSatisfy(\.isNumber),
              strictMutationIdentity(path),
              path == "window[0]" || path.hasPrefix("window[0]/"),
              strictMutationIdentity(expectedRole),
              strictMutationIdentity(expectedFingerprint),
              fallbackPolicy == "report_unsupported",
              strictMutationIdentity(commitDeadlineAt),
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid(
                "invalid semantic_text_selection_v2 authority")
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
        try container.encode(
            expectedFingerprint, forKey: strictMutationKey("expected_fingerprint"))
        try container.encode(range, forKey: strictMutationKey("range"))
        try container.encode(fallbackPolicy, forKey: strictMutationKey("fallback_policy"))
        try container.encode(commitDeadlineAt, forKey: strictMutationKey("commit_deadline_at"))
    }
}

struct SemanticTextSelectionRPCRequestV2: Codable, Equatable {
    let id: Int64
    let method: String
    let params: SemanticTextSelectionRequestV2

    init(
        id: Int64, method: String = "semantic_text_selection_v2",
        params: SemanticTextSelectionRequestV2
    ) {
        self.id = id
        self.method = method
        self.params = params
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"],
            field: "semantic_text_selection_v2 envelope")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(
            SemanticTextSelectionRequestV2.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "semantic_text_selection_v2" else {
            throw StrictMutationWireError.invalid(
                "invalid semantic_text_selection_v2 envelope")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(id, forKey: strictMutationKey("id"))
        try container.encode(method, forKey: strictMutationKey("method"))
        try container.encode(params, forKey: strictMutationKey("params"))
    }
}

func decodeSemanticTextSelectionRPCRequestV2(
    _ payload: Data
) throws -> SemanticTextSelectionRPCRequestV2 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticTextSelectionRPCRequestV2.self, from: payload)
}

struct SemanticTextSelectionResultV2: Codable, Equatable {
    let schemaVersion: Int
    let status: String
    let commitState: String
    let phase: String
    let failureCode: String?
    let retrySafe: Bool
    let postcondition: String?
    let selectedRange: SemanticTextRangeV2?

    init(
        status: String, commitState: String, phase: String,
        failureCode: String?, postcondition: String? = nil,
        selectedRange: SemanticTextRangeV2? = nil
    ) {
        schemaVersion = 2
        self.status = status
        self.commitState = commitState
        self.phase = phase
        self.failureCode = failureCode
        retrySafe = false
        self.postcondition = postcondition
        self.selectedRange = selectedRange
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "status", "commit_state", "phase", "failure_code",
            "retry_safe", "postcondition", "selected_range",
        ], field: "semantic_text_selection_v2 result")
        schemaVersion = try container.decode(
            Int.self, forKey: strictMutationKey("schema_version"))
        status = try container.decode(String.self, forKey: strictMutationKey("status"))
        commitState = try container.decode(
            String.self, forKey: strictMutationKey("commit_state"))
        phase = try container.decode(String.self, forKey: strictMutationKey("phase"))
        failureCode = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("failure_code"))
        retrySafe = try container.decode(Bool.self, forKey: strictMutationKey("retry_safe"))
        postcondition = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("postcondition"))
        selectedRange = try container.decodeIfPresent(
            SemanticTextRangeV2.self, forKey: strictMutationKey("selected_range"))
        guard validateSemanticTextSelectionResultV2(self) else {
            throw StrictMutationWireError.invalid(
                "invalid semantic_text_selection_v2 result tagged union")
        }
    }

    func encode(to encoder: Encoder) throws {
        guard validateSemanticTextSelectionResultV2(self) else {
            throw StrictMutationWireError.invalid(
                "invalid semantic_text_selection_v2 result tagged union")
        }
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(status, forKey: strictMutationKey("status"))
        try container.encode(commitState, forKey: strictMutationKey("commit_state"))
        try container.encode(phase, forKey: strictMutationKey("phase"))
        try container.encode(failureCode, forKey: strictMutationKey("failure_code"))
        try container.encode(retrySafe, forKey: strictMutationKey("retry_safe"))
        try container.encode(postcondition, forKey: strictMutationKey("postcondition"))
        try container.encode(selectedRange, forKey: strictMutationKey("selected_range"))
    }
}

func decodeSemanticTextSelectionResultV2(
    _ payload: Data
) throws -> SemanticTextSelectionResultV2 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticTextSelectionResultV2.self, from: payload)
}

private func validateSemanticTextSelectionResultV2(
    _ result: SemanticTextSelectionResultV2
) -> Bool {
    guard result.schemaVersion == 2, !result.retrySafe else { return false }
    switch result.status {
    case "verified":
        return result.commitState == "committed" &&
            result.phase == "post_verification" && result.failureCode == nil &&
            result.postcondition == "selected_range_matches" && result.selectedRange != nil
    case "completed_unverified":
        if result.commitState == "unknown" {
            return result.phase == "action" &&
                result.failureCode == "ax_selection_commit_unknown" &&
                result.postcondition == nil && result.selectedRange == nil
        }
        guard result.commitState == "committed", result.phase == "post_verification",
              result.postcondition == nil else { return false }
        if result.failureCode == "selected_range_mismatch" {
            return result.selectedRange != nil
        }
        return Set([
            "selected_range_not_observed", "interference_detection_unavailable",
            "ax_messaging_timeout_restore_failed",
        ]).contains(result.failureCode ?? "") && result.selectedRange == nil
    case "user_interference":
        return Set(["not_committed", "committed", "unknown"]).contains(result.commitState) &&
            result.phase == "user_interference" &&
            result.failureCode == "physical_input_interference" &&
            result.postcondition == nil && result.selectedRange == nil
    case "fallback_required":
        return result.commitState == "not_committed" && result.phase == "preflight" &&
            result.failureCode == "ax_text_range_unsupported" &&
            result.postcondition == nil && result.selectedRange == nil
    case "failed":
        let preflight = Set([
            "invalid_request", "request_expired", "process_not_live",
            "process_identity_mismatch", "window_not_found", "window_ambiguous",
            "path_not_found", "role_mismatch", "fingerprint_mismatch",
            "fingerprint_not_found", "fingerprint_ambiguous", "sensitive_target",
            "enabled_unknown", "target_disabled", "interference_detection_unavailable",
            "ax_messaging_timeout_unavailable",
        ])
        let exactPhase = result.failureCode.map { code in
            preflight.contains(code) ? result.phase == "preflight" :
                (code == "ax_selection_failed" && result.phase == "action")
        } ?? false
        return result.commitState == "not_committed" && exactPhase &&
            result.postcondition == nil && result.selectedRange == nil
    default:
        return false
    }
}

enum SemanticTextSelectionWindowResolutionV2 {
    case unique
    case missing
    case ambiguous
}

enum SemanticTextRangeObservationV2 {
    case range(SemanticTextRangeV2)
    case unavailable
    case unsupported
}

enum SemanticTextSelectionAXCallResultV2 {
    case completed(error: AXError, timeoutRestored: Bool)
    case timeoutUnavailable
}

enum SemanticTextSelectionObservationCallResultV2 {
    case completed(observation: SemanticTextRangeObservationV2, timeoutRestored: Bool)
    case timeoutUnavailable
}

struct SemanticTextSelectionTargetV2 {
    let role: String
    let fingerprint: String
    let enabled: Bool?
    let sensitive: Bool
    let supportsParameterizedTextRange: Bool
    let selectedTextRangeSettable: Bool
    let setSelectedRange: (SemanticTextRangeV2, TimeInterval) ->
        SemanticTextSelectionAXCallResultV2
    let observeSelectedRange: (TimeInterval) ->
        SemanticTextSelectionObservationCallResultV2
}

struct SemanticTextSelectionDependenciesV2 {
    let isPIDLive: (Int) -> Bool
    let bundleIDForPID: (Int) -> String?
    let resolveWindow: (Int, UInt32) -> SemanticTextSelectionWindowResolutionV2
    let resolveTarget: (Int, UInt32, String) -> SemanticTextSelectionTargetV2?
    let countFingerprint: (Int, UInt32, String) -> Int
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let now: () -> Date
    let sleep: (TimeInterval) -> Void
}

private func semanticSelectionFailureV2(
    _ code: String, phase: String = "preflight"
) -> SemanticTextSelectionResultV2 {
    .init(
        status: "failed", commitState: "not_committed", phase: phase,
        failureCode: code)
}

private func semanticSelectionInterferenceV2(
    commitState: String
) -> SemanticTextSelectionResultV2 {
    .init(
        status: "user_interference", commitState: commitState,
        phase: "user_interference", failureCode: "physical_input_interference")
}

private func semanticSelectionMonitoringUnavailableV2(
    commitState: String
) -> SemanticTextSelectionResultV2 {
    if commitState == "unknown" {
        return .init(
            status: "completed_unverified", commitState: "unknown",
            phase: "action", failureCode: "ax_selection_commit_unknown")
    }
    return .init(
        status: commitState == "committed" ? "completed_unverified" : "failed",
        commitState: commitState,
        phase: commitState == "committed" ? "post_verification" : "preflight",
        failureCode: "interference_detection_unavailable")
}

func runSemanticTextSelectionV2(
    request: SemanticTextSelectionRequestV2,
    dependencies: SemanticTextSelectionDependenciesV2
) -> SemanticTextSelectionResultV2 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return semanticSelectionFailureV2("invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else { return semanticSelectionFailureV2("request_expired") }
    guard horizon <= 3 else { return semanticSelectionFailureV2("invalid_request") }
    guard dependencies.isPIDLive(request.pid) else {
        return semanticSelectionFailureV2("process_not_live")
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return semanticSelectionFailureV2("process_identity_mismatch")
    }
    switch dependencies.resolveWindow(request.pid, request.windowID) {
    case .missing: return semanticSelectionFailureV2("window_not_found")
    case .ambiguous: return semanticSelectionFailureV2("window_ambiguous")
    case .unique: break
    }
    guard let target = dependencies.resolveTarget(
        request.pid, request.windowID, request.path) else {
        return semanticSelectionFailureV2("path_not_found")
    }
    guard target.role == request.expectedRole else {
        return semanticSelectionFailureV2("role_mismatch")
    }
    guard target.fingerprint == request.expectedFingerprint else {
        return semanticSelectionFailureV2("fingerprint_mismatch")
    }
    switch dependencies.countFingerprint(
        request.pid, request.windowID, request.expectedFingerprint) {
    case 0: return semanticSelectionFailureV2("fingerprint_not_found")
    case 1: break
    default: return semanticSelectionFailureV2("fingerprint_ambiguous")
    }
    guard !target.sensitive else {
        return semanticSelectionFailureV2("sensitive_target")
    }
    guard target.enabled == true else {
        return semanticSelectionFailureV2(
            target.enabled == nil ? "enabled_unknown" : "target_disabled")
    }
    guard target.supportsParameterizedTextRange,
          target.selectedTextRangeSettable else {
        return .init(
            status: "fallback_required", commitState: "not_committed",
            phase: "preflight", failureCode: "ax_text_range_unsupported")
    }
    guard dependencies.now() < deadline else {
        return semanticSelectionFailureV2("request_expired")
    }

    guard let initialPhysicalInput = dependencies.observePhysicalInput() else {
        return semanticSelectionMonitoringUnavailableV2(commitState: "not_committed")
    }
    switch assessPhysicalInputInterferenceV1(
        baseline: initialPhysicalInput, current: initialPhysicalInput,
        expectedPointer: initialPhysicalInput.pointer, expectedSyntheticEvents: []) {
    case .interference:
        return semanticSelectionInterferenceV2(commitState: "not_committed")
    case .unavailable:
        return semanticSelectionMonitoringUnavailableV2(commitState: "not_committed")
    case .unchanged:
        break
    }
    var physicalInputBaseline: PhysicalInputInterferenceSnapshotV1? = initialPhysicalInput
    var physicalAssessment: PhysicalInputInterferenceAssessmentV1 = .unchanged
    let assessCheckpoint: () -> PhysicalInputInterferenceAssessmentV1 = {
        guard physicalAssessment == .unchanged else { return physicalAssessment }
        let current = dependencies.observePhysicalInput()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: physicalInputBaseline, current: current,
            expectedPointer: physicalInputBaseline?.pointer,
            // AXUIElementSetAttributeValue posts no event through Kocoro's
            // private CGEventSource.
            expectedSyntheticEvents: [])
        if assessment == .unchanged {
            physicalInputBaseline = current
        } else {
            physicalAssessment = assessment
        }
        return assessment
    }

    let actionTimeout = min(
        targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
        max(0, deadline.timeIntervalSince(dependencies.now())))
    guard actionTimeout > 0 else {
        return semanticSelectionFailureV2("request_expired")
    }
    let action = target.setSelectedRange(request.range, actionTimeout)
    let commitState: String
    let actionTimeoutRestored: Bool
    var fallbackRequired = false
    switch action {
    case .timeoutUnavailable:
        return semanticSelectionFailureV2("ax_messaging_timeout_unavailable")
    case let .completed(error, restored):
        switch error {
        case .success:
            commitState = "committed"
        case .attributeUnsupported, .parameterizedAttributeUnsupported:
            commitState = "not_committed"
            fallbackRequired = true
        case .illegalArgument, .invalidUIElement, .notImplemented, .apiDisabled:
            commitState = "not_committed"
        default:
            // Messaging failure does not prove the remote application failed
            // before applying AXSelectedTextRange.
            commitState = "unknown"
        }
        actionTimeoutRestored = restored
    }
    switch assessCheckpoint() {
    case .interference:
        return semanticSelectionInterferenceV2(commitState: commitState)
    case .unavailable:
        return semanticSelectionMonitoringUnavailableV2(commitState: commitState)
    case .unchanged:
        break
    }
    if commitState == "unknown" {
        _ = assessCheckpoint()
        switch physicalAssessment {
        case .interference:
            return semanticSelectionInterferenceV2(commitState: "unknown")
        case .unavailable:
            return semanticSelectionMonitoringUnavailableV2(commitState: "unknown")
        case .unchanged:
            return .init(
                status: "completed_unverified", commitState: "unknown",
                phase: "action", failureCode: "ax_selection_commit_unknown")
        }
    }
    if fallbackRequired {
        return .init(
            status: "fallback_required", commitState: "not_committed",
            phase: "preflight", failureCode: "ax_text_range_unsupported")
    }
    guard commitState == "committed" else {
        return semanticSelectionFailureV2("ax_selection_failed", phase: "action")
    }

    let verification: TargetedAXPostconditionOutcomeV1<SemanticTextRangeV2> =
        runTargetedAXPostconditionVerificationV1(
            now: dependencies.now,
            sleep: dependencies.sleep,
            observeWithTimeout: { timeout in
                switch assessCheckpoint() {
                case .interference:
                    return .terminal(
                        failureCode: "physical_input_interference", observation: nil)
                case .unavailable:
                    return .terminal(
                        failureCode: "interference_detection_unavailable", observation: nil)
                case .unchanged:
                    break
                }
                let observation = target.observeSelectedRange(timeout)
                switch assessCheckpoint() {
                case .interference:
                    return .terminal(
                        failureCode: "physical_input_interference", observation: nil)
                case .unavailable:
                    return .terminal(
                        failureCode: "interference_detection_unavailable", observation: nil)
                case .unchanged:
                    break
                }
                switch observation {
                case .timeoutUnavailable:
                    return .terminal(
                        failureCode: "selected_range_not_observed", observation: nil)
                case let .completed(rangeObservation, timeoutRestored):
                    guard timeoutRestored else {
                        return .terminal(
                            failureCode: "ax_messaging_timeout_restore_failed",
                            observation: nil)
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
                        return .terminal(
                            failureCode: "selected_range_not_observed", observation: nil)
                    }
                }
            })
    _ = assessCheckpoint()
    switch physicalAssessment {
    case .interference:
        return semanticSelectionInterferenceV2(commitState: "committed")
    case .unavailable:
        return semanticSelectionMonitoringUnavailableV2(commitState: "committed")
    case .unchanged:
        break
    }
    if !actionTimeoutRestored {
        return .init(
            status: "completed_unverified", commitState: "committed",
            phase: "post_verification",
            failureCode: "ax_messaging_timeout_restore_failed")
    }
    switch verification {
    case let .verified(observed, _):
        return .init(
            status: "verified", commitState: "committed",
            phase: "post_verification", failureCode: nil,
            postcondition: "selected_range_matches", selectedRange: observed)
    case let .inconclusive(failureCode, observed, _):
        if failureCode == "physical_input_interference" {
            return semanticSelectionInterferenceV2(commitState: "committed")
        }
        if failureCode == "interference_detection_unavailable" {
            return semanticSelectionMonitoringUnavailableV2(commitState: "committed")
        }
        return .init(
            status: "completed_unverified", commitState: "committed",
            phase: "post_verification", failureCode: failureCode,
            selectedRange: failureCode == "selected_range_mismatch" ? observed : nil)
    }
}

private enum SemanticSelectionResolvedWindowV2 {
    case unique(AXUIElement)
    case missing
    case ambiguous
}

private func resolveSemanticSelectionWindowV2(
    pid: Int, windowID: UInt32
) -> SemanticSelectionResolvedWindowV2 {
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
                frame: .init(
                    x: frame.x, y: frame.y, width: frame.width, height: frame.height)),
            candidates: candidates)
        if matches.count > 1 && matches.contains(where: { $0.windowID == Int(windowID) }) {
            ambiguous = true
        } else if matches.count == 1, matches[0].windowID == Int(windowID) {
            exact.append(window)
        }
    }
    if ambiguous || exact.count > 1 { return .ambiguous }
    return exact.first.map(SemanticSelectionResolvedWindowV2.unique) ?? .missing
}

private func semanticSelectionFingerprintV2(_ element: AXUIElement) -> String? {
    guard let role = axString(element, "AXRole") else { return nil }
    return stableElementFingerprint(AXElementSnapshotAttributes(
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
        actions: axActions(element), frame: nil))
}

private func matchingSemanticSelectionFingerprintV2(
    in window: AXUIElement, fingerprint: String
) -> [AXUIElement] {
    var matches: [AXUIElement] = []
    func walk(_ element: AXUIElement) {
        if semanticSelectionFingerprintV2(element) == fingerprint { matches.append(element) }
        for child in axChildren(element) ?? [] { walk(child) }
    }
    for child in axChildren(window) ?? [] { walk(child) }
    return matches
}

private func performSemanticSelectionAXCallV2(
    element: AXUIElement, range: SemanticTextRangeV2, timeout: TimeInterval
) -> SemanticTextSelectionAXCallResultV2 {
    var raw = CFRange(location: range.location, length: range.length)
    guard let value = AXValueCreate(.cfRange, &raw) else {
        return .completed(error: .illegalArgument, timeoutRestored: true)
    }
    guard timeout > 0, timeout.isFinite,
          AXUIElementSetMessagingTimeout(element, Float(timeout)) == .success else {
        return .timeoutUnavailable
    }
    let error = AXUIElementSetAttributeValue(
        element, kAXSelectedTextRangeAttribute as CFString, value)
    let restored = AXUIElementSetMessagingTimeout(element, 0) == .success
    return .completed(error: error, timeoutRestored: restored)
}

private func observeSemanticSelectionRangeV2(
    element: AXUIElement, timeout: TimeInterval
) -> SemanticTextSelectionObservationCallResultV2 {
    guard timeout > 0, timeout.isFinite,
          AXUIElementSetMessagingTimeout(element, Float(timeout)) == .success else {
        return .timeoutUnavailable
    }
    var raw: CFTypeRef?
    let error = AXUIElementCopyAttributeValue(
        element, kAXSelectedTextRangeAttribute as CFString, &raw)
    let observation: SemanticTextRangeObservationV2
    if error == .attributeUnsupported {
        observation = .unsupported
    } else if error != .success || raw == nil || CFGetTypeID(raw!) != AXValueGetTypeID() {
        observation = .unavailable
    } else {
        var selectedRange = CFRange()
        if AXValueGetValue(raw as! AXValue, .cfRange, &selectedRange),
           selectedRange.location >= 0, selectedRange.length > 0 {
            observation = .range(.init(
                location: selectedRange.location, length: selectedRange.length))
        } else {
            observation = .unavailable
        }
    }
    let restored = AXUIElementSetMessagingTimeout(element, 0) == .success
    return .completed(observation: observation, timeoutRestored: restored)
}

private func semanticSelectionTargetV2(
    _ element: AXUIElement
) -> SemanticTextSelectionTargetV2? {
    guard let role = axString(element, "AXRole"),
          let fingerprint = semanticSelectionFingerprintV2(element) else { return nil }
    let metadata = axValueSensitivityMetadata(element, role: role)
    var parameterized: CFArray?
    let parameterizedResult = AXUIElementCopyParameterizedAttributeNames(
        element, &parameterized)
    let names = parameterized as? [String] ?? []
    let supportsRange = parameterizedResult == .success &&
        (names.contains(kAXStringForRangeParameterizedAttribute as String) ||
         names.contains(kAXBoundsForRangeParameterizedAttribute as String))
    var settable = DarwinBoolean(false)
    let settableResult = AXUIElementIsAttributeSettable(
        element, kAXSelectedTextRangeAttribute as CFString, &settable)
    return .init(
        role: role,
        fingerprint: fingerprint,
        enabled: axBool(element, "AXEnabled"),
        sensitive: isSensitiveAXValue(metadata),
        supportsParameterizedTextRange: supportsRange,
        selectedTextRangeSettable: settableResult == .success && settable.boolValue,
        setSelectedRange: { range, timeout in
            performSemanticSelectionAXCallV2(
                element: element, range: range, timeout: timeout)
        },
        observeSelectedRange: { timeout in
            observeSemanticSelectionRangeV2(element: element, timeout: timeout)
        })
}

let productionSemanticTextSelectionDependenciesV2 = SemanticTextSelectionDependenciesV2(
    isPIDLive: { pid in
        refreshAppKitState()
        guard let processID = pid_t(exactly: pid),
              let app = NSRunningApplication(processIdentifier: processID) else { return false }
        return !app.isTerminated
    },
    bundleIDForPID: { pid in
        guard let processID = pid_t(exactly: pid) else { return nil }
        return NSRunningApplication(processIdentifier: processID)?.bundleIdentifier
    },
    resolveWindow: { pid, windowID in
        switch resolveSemanticSelectionWindowV2(pid: pid, windowID: windowID) {
        case .unique: return .unique
        case .missing: return .missing
        case .ambiguous: return .ambiguous
        }
    },
    resolveTarget: { pid, windowID, path in
        guard case let .unique(window) = resolveSemanticSelectionWindowV2(
            pid: pid, windowID: windowID),
            let element = resolveElement(in: window, path: path) else { return nil }
        return semanticSelectionTargetV2(element)
    },
    countFingerprint: { pid, windowID, fingerprint in
        guard case let .unique(window) = resolveSemanticSelectionWindowV2(
            pid: pid, windowID: windowID) else { return 0 }
        return matchingSemanticSelectionFingerprintV2(
            in: window, fingerprint: fingerprint).count
    },
    observePhysicalInput: observePhysicalInputInterferenceV1,
    now: Date.init,
    sleep: Thread.sleep(forTimeInterval:))
