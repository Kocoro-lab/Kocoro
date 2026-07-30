import ApplicationServices
import AppKit
import Foundation

struct SemanticPressRiskDestinationAssertionV2: Codable, Equatable {
    let kind: String
    let expectedWindowTitle: String

    init(kind: String = "exact_window_title", expectedWindowTitle: String) {
        self.kind = kind
        self.expectedWindowTitle = expectedWindowTitle
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "kind", "expected_window_title",
        ], field: "semantic_press_v2 risk_destination_assertion")
        kind = try container.decode(String.self, forKey: strictMutationKey("kind"))
        expectedWindowTitle = try container.decode(
            String.self, forKey: strictMutationKey("expected_window_title"))
        guard kind == "exact_window_title",
              validSemanticPressRiskLabelV2(expectedWindowTitle) else {
            throw StrictMutationWireError.invalid(
                "invalid semantic_press_v2 risk destination assertion")
        }
    }

    func encode(to encoder: Encoder) throws {
        guard kind == "exact_window_title",
              validSemanticPressRiskLabelV2(expectedWindowTitle) else {
            throw StrictMutationWireError.invalid(
                "invalid semantic_press_v2 risk destination assertion")
        }
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(kind, forKey: strictMutationKey("kind"))
        try container.encode(
            expectedWindowTitle, forKey: strictMutationKey("expected_window_title"))
    }
}

private func validSemanticPressRiskLabelV2(_ value: String) -> Bool {
    guard strictMutationIdentity(value), value.unicodeScalars.count <= 128 else { return false }
    return value.unicodeScalars.allSatisfy { scalar in
        !CharacterSet.controlCharacters.contains(scalar) &&
            scalar.properties.generalCategory != .format
    }
}

struct SemanticPressRequestV2: Codable, Equatable {
    let schemaVersion: Int
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let ref: String
    let path: String
    let expectedRole: String
    let expectedFingerprint: String
    let fallbackPolicy: String
    let interferencePolicy: String
    let commitDeadlineAt: String
    let riskDestinationAssertion: SemanticPressRiskDestinationAssertionV2?

    init(
        schemaVersion: Int = 2, pid: Int, bundleID: String, windowID: UInt32,
        ref: String, path: String, expectedRole: String, expectedFingerprint: String,
        fallbackPolicy: String = "none",
        interferencePolicy: String = "global_physical",
        commitDeadlineAt: String,
        riskDestinationAssertion: SemanticPressRiskDestinationAssertionV2? = nil
    ) {
        self.schemaVersion = schemaVersion
        self.pid = pid
        self.bundleID = bundleID
        self.windowID = windowID
        self.ref = ref
        self.path = path
        self.expectedRole = expectedRole
        self.expectedFingerprint = expectedFingerprint
        self.fallbackPolicy = fallbackPolicy
        self.interferencePolicy = interferencePolicy
        self.commitDeadlineAt = commitDeadlineAt
        self.riskDestinationAssertion = riskDestinationAssertion
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "pid", "bundle_id", "window_id", "ref", "path",
            "expected_role", "expected_fingerprint", "fallback_policy",
            "interference_policy", "commit_deadline_at",
            "risk_destination_assertion",
        ], field: "semantic_press_v2 params")
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
        fallbackPolicy = try container.decode(
            String.self, forKey: strictMutationKey("fallback_policy"))
        interferencePolicy = try container.decode(
            String.self, forKey: strictMutationKey("interference_policy"))
        commitDeadlineAt = try container.decode(
            String.self, forKey: strictMutationKey("commit_deadline_at"))
        riskDestinationAssertion = try container.decodeIfPresent(
            SemanticPressRiskDestinationAssertionV2.self,
            forKey: strictMutationKey("risk_destination_assertion"))
        guard schemaVersion == 2, pid > 0,
              strictMutationIdentity(bundleID),
              let exactWindowID = UInt32(exactly: rawWindowID), exactWindowID > 0,
              strictMutationIdentity(ref), ref.count > 1, ref.first == "e",
              ref.dropFirst().allSatisfy(\.isNumber),
              strictMutationIdentity(path),
              path == "window[0]" || path.hasPrefix("window[0]/"),
              strictMutationIdentity(expectedRole),
              strictMutationIdentity(expectedFingerprint),
              fallbackPolicy == "none",
              Set(["global_physical", "target_foreground"]).contains(
                interferencePolicy),
              strictMutationIdentity(commitDeadlineAt),
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid("invalid semantic_press_v2 authority")
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
        try container.encode(fallbackPolicy, forKey: strictMutationKey("fallback_policy"))
        try container.encode(
            interferencePolicy, forKey: strictMutationKey("interference_policy"))
        try container.encode(commitDeadlineAt, forKey: strictMutationKey("commit_deadline_at"))
        try container.encode(
            riskDestinationAssertion,
            forKey: strictMutationKey("risk_destination_assertion"))
    }
}

struct SemanticPressRPCRequestV2: Codable, Equatable {
    let id: Int64
    let method: String
    let params: SemanticPressRequestV2

    init(id: Int64, method: String = "semantic_press_v2", params: SemanticPressRequestV2) {
        self.id = id
        self.method = method
        self.params = params
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"],
            field: "semantic_press_v2 envelope")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(
            SemanticPressRequestV2.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "semantic_press_v2" else {
            throw StrictMutationWireError.invalid("invalid semantic_press_v2 envelope")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(id, forKey: strictMutationKey("id"))
        try container.encode(method, forKey: strictMutationKey("method"))
        try container.encode(params, forKey: strictMutationKey("params"))
    }
}

func decodeSemanticPressRPCRequestV2(_ payload: Data) throws -> SemanticPressRPCRequestV2 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticPressRPCRequestV2.self, from: payload)
}

struct SemanticPressResultV2: Codable, Equatable {
    let schemaVersion: Int
    let status: String
    let commitState: String
    let phase: String
    let failureCode: String?
    let postcondition: String?
    let retrySafe: Bool

    init(
        status: String, commitState: String, phase: String,
        failureCode: String?, postcondition: String? = nil
    ) {
        schemaVersion = 2
        self.status = status
        self.commitState = commitState
        self.phase = phase
        self.failureCode = failureCode
        self.postcondition = postcondition
        retrySafe = false
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "status", "commit_state", "phase",
            "failure_code", "postcondition", "retry_safe",
        ], field: "semantic_press_v2 result")
        schemaVersion = try container.decode(
            Int.self, forKey: strictMutationKey("schema_version"))
        status = try container.decode(String.self, forKey: strictMutationKey("status"))
        commitState = try container.decode(
            String.self, forKey: strictMutationKey("commit_state"))
        phase = try container.decode(String.self, forKey: strictMutationKey("phase"))
        failureCode = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("failure_code"))
        postcondition = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("postcondition"))
        retrySafe = try container.decode(Bool.self, forKey: strictMutationKey("retry_safe"))
        guard validateSemanticPressResultV2(self) else {
            throw StrictMutationWireError.invalid("invalid semantic_press_v2 result tagged union")
        }
    }

    func encode(to encoder: Encoder) throws {
        guard validateSemanticPressResultV2(self) else {
            throw StrictMutationWireError.invalid("invalid semantic_press_v2 result tagged union")
        }
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(status, forKey: strictMutationKey("status"))
        try container.encode(commitState, forKey: strictMutationKey("commit_state"))
        try container.encode(phase, forKey: strictMutationKey("phase"))
        try container.encode(failureCode, forKey: strictMutationKey("failure_code"))
        try container.encode(postcondition, forKey: strictMutationKey("postcondition"))
        try container.encode(retrySafe, forKey: strictMutationKey("retry_safe"))
    }
}

func decodeSemanticPressResultV2(_ payload: Data) throws -> SemanticPressResultV2 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticPressResultV2.self, from: payload)
}

private func validateSemanticPressResultV2(_ result: SemanticPressResultV2) -> Bool {
    guard result.schemaVersion == 2, !result.retrySafe,
          result.postcondition == nil else { return false }
    switch result.status {
    case "completed_unverified":
        if result.commitState == "unknown" {
            return result.phase == "action" &&
                result.failureCode == "ax_press_commit_unknown"
        }
        return result.commitState == "committed" &&
            result.phase == "post_verification" && Set([
                "postcondition_not_declared", "interference_detection_unavailable",
                "ax_messaging_timeout_restore_failed",
            ]).contains(result.failureCode ?? "")
    case "user_interference":
        return Set(["not_committed", "committed", "unknown"]).contains(result.commitState) &&
            result.phase == "user_interference" &&
            Set([
                "physical_input_interference",
                "target_foreground_interference",
            ]).contains(result.failureCode ?? "")
    case "failed":
        let preflight = Set([
            "invalid_request", "request_expired", "process_not_live",
            "process_identity_mismatch", "window_not_found", "window_ambiguous",
            "path_not_found", "role_mismatch", "fingerprint_mismatch",
            "fingerprint_not_found", "fingerprint_ambiguous", "sensitive_target",
            "enabled_unknown", "target_disabled", "ax_press_unavailable",
            "interference_detection_unavailable", "ax_messaging_timeout_unavailable",
            "risk_destination_drift",
            "risk_destination_unavailable",
            "target_became_frontmost",
        ])
        let exactPhase = result.failureCode.map { code in
            preflight.contains(code) ? result.phase == "preflight" :
                (code == "ax_press_failed" && result.phase == "action")
        } ?? false
        return result.commitState == "not_committed" && exactPhase
    default:
        // This request declares no causal postcondition, so v2 can never
        // honestly report a verified press.
        return false
    }
}

enum SemanticPressWindowResolutionV2 {
    case unique
    case missing
    case ambiguous
}

enum SemanticPressTargetObservationV2 {
    case present
    case missing
    case unavailable
}

enum SemanticPressRiskWindowTitleObservationV2 {
    case value(String?)
    case unavailable
}

enum SemanticPressAXCallResultV2 {
    case completed(error: AXError, timeoutRestored: Bool)
    case timeoutUnavailable
}

struct SemanticPressTargetV2 {
    let role: String
    let fingerprint: String
    let enabled: Bool?
    let sensitive: Bool
    let actions: [String]
    let performPress: (TimeInterval) -> SemanticPressAXCallResultV2
    let observeTarget: (TimeInterval) -> SemanticPressTargetObservationV2
}

struct SemanticPressDependenciesV2 {
    let isPIDLive: (Int) -> Bool
    let bundleIDForPID: (Int) -> String?
    let resolveWindow: (Int, UInt32) -> SemanticPressWindowResolutionV2
    let resolveTarget: (Int, UInt32, String) -> SemanticPressTargetV2?
    let countFingerprint: (Int, UInt32, String) -> Int
    let windowTitle: (Int, UInt32, TimeInterval) -> SemanticPressRiskWindowTitleObservationV2
    let frontmostPID: () -> Int?
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let now: () -> Date
    let sleep: (TimeInterval) -> Void
}

private func semanticPressFailureV2(
    _ code: String, phase: String = "preflight"
) -> SemanticPressResultV2 {
    .init(
        status: "failed", commitState: "not_committed",
        phase: phase, failureCode: code)
}

private func semanticPressInterferenceV2(
    commitState: String,
    failureCode: String = "physical_input_interference"
) -> SemanticPressResultV2 {
    .init(
        status: "user_interference", commitState: commitState,
        phase: "user_interference", failureCode: failureCode)
}

private func semanticPressMonitoringUnavailableV2(
    commitState: String
) -> SemanticPressResultV2 {
    if commitState == "unknown" {
        return .init(
            status: "completed_unverified", commitState: "unknown",
            phase: "action", failureCode: "ax_press_commit_unknown")
    }
    return .init(
        status: commitState == "committed" ? "completed_unverified" : "failed",
        commitState: commitState,
        phase: commitState == "committed" ? "post_verification" : "preflight",
        failureCode: "interference_detection_unavailable")
}

func runSemanticPressV2(
    request: SemanticPressRequestV2,
    dependencies: SemanticPressDependenciesV2
) -> SemanticPressResultV2 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return semanticPressFailureV2("invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else { return semanticPressFailureV2("request_expired") }
    guard horizon <= 3 else { return semanticPressFailureV2("invalid_request") }
    guard dependencies.isPIDLive(request.pid) else {
        return semanticPressFailureV2("process_not_live")
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return semanticPressFailureV2("process_identity_mismatch")
    }
    switch dependencies.resolveWindow(request.pid, request.windowID) {
    case .missing: return semanticPressFailureV2("window_not_found")
    case .ambiguous: return semanticPressFailureV2("window_ambiguous")
    case .unique: break
    }
    guard let target = dependencies.resolveTarget(
        request.pid, request.windowID, request.path) else {
        return semanticPressFailureV2("path_not_found")
    }
    guard target.role == request.expectedRole else {
        return semanticPressFailureV2("role_mismatch")
    }
    guard target.fingerprint == request.expectedFingerprint else {
        return semanticPressFailureV2("fingerprint_mismatch")
    }
    switch dependencies.countFingerprint(
        request.pid, request.windowID, request.expectedFingerprint) {
    case 0: return semanticPressFailureV2("fingerprint_not_found")
    case 1: break
    default: return semanticPressFailureV2("fingerprint_ambiguous")
    }
    guard !target.sensitive else { return semanticPressFailureV2("sensitive_target") }
    guard target.enabled == true else {
        return semanticPressFailureV2(
            target.enabled == nil ? "enabled_unknown" : "target_disabled")
    }
    guard target.actions.contains("AXPress") else {
        return semanticPressFailureV2("ax_press_unavailable")
    }
    guard dependencies.now() < deadline else {
        return semanticPressFailureV2("request_expired")
    }

    let targetForegroundPolicy = request.interferencePolicy == "target_foreground"
    var physicalInputBaseline: PhysicalInputInterferenceSnapshotV1?
    var physicalAssessment: PhysicalInputInterferenceAssessmentV1 = .unchanged
    let assessCheckpoint: () -> PhysicalInputInterferenceAssessmentV1 = {
        guard physicalAssessment == .unchanged else { return physicalAssessment }
        if targetForegroundPolicy {
            guard let frontmostPID = dependencies.frontmostPID() else {
                physicalAssessment = .unavailable
                return physicalAssessment
            }
            if frontmostPID == request.pid {
                physicalAssessment = .interference
            }
            return physicalAssessment
        }
        let current = dependencies.observePhysicalInput()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: physicalInputBaseline, current: current,
            expectedPointer: physicalInputBaseline?.pointer,
            // AXPress itself is semantic and posts no event through Kocoro's
            // private CGEventSource.
            expectedSyntheticEvents: [])
        if assessment == .unchanged {
            physicalInputBaseline = current
        } else {
            physicalAssessment = assessment
        }
        return assessment
    }
    if targetForegroundPolicy {
        switch assessCheckpoint() {
        case .interference:
            return semanticPressFailureV2("target_became_frontmost")
        case .unavailable:
            return semanticPressMonitoringUnavailableV2(commitState: "not_committed")
        case .unchanged:
            break
        }
    } else {
        guard let initialPhysicalInput = dependencies.observePhysicalInput() else {
            return semanticPressMonitoringUnavailableV2(commitState: "not_committed")
        }
        switch assessPhysicalInputInterferenceV1(
            baseline: initialPhysicalInput, current: initialPhysicalInput,
            expectedPointer: initialPhysicalInput.pointer, expectedSyntheticEvents: []) {
        case .interference:
            return semanticPressInterferenceV2(commitState: "not_committed")
        case .unavailable:
            return semanticPressMonitoringUnavailableV2(commitState: "not_committed")
        case .unchanged:
            physicalInputBaseline = initialPhysicalInput
        }
    }
    let activeInterferenceCode = targetForegroundPolicy
        ? "target_foreground_interference"
        : "physical_input_interference"

    if let assertion = request.riskDestinationAssertion {
        let titleTimeout = min(
            0.1, max(0, deadline.timeIntervalSince(dependencies.now())))
        guard titleTimeout > 0 else { return semanticPressFailureV2("request_expired") }
        switch dependencies.windowTitle(request.pid, request.windowID, titleTimeout) {
        case .unavailable:
            return semanticPressFailureV2("risk_destination_unavailable")
        case let .value(title):
            guard assertion.kind == "exact_window_title",
                  title == assertion.expectedWindowTitle else {
                return semanticPressFailureV2("risk_destination_drift")
            }
        }
        guard dependencies.now() < deadline else {
            return semanticPressFailureV2("request_expired")
        }
        // The destination assertion is read after the physical baseline; this
        // checkpoint closes user-input drift during that read and is immediately
        // adjacent to AXPress.
        switch assessCheckpoint() {
        case .interference:
            if targetForegroundPolicy {
                return semanticPressFailureV2("target_became_frontmost")
            }
            return semanticPressInterferenceV2(commitState: "not_committed")
        case .unavailable:
            return semanticPressMonitoringUnavailableV2(commitState: "not_committed")
        case .unchanged:
            break
        }
    }

    // The initial sample is the checkpoint immediately before AXPress for an
    // ordinary press. Risk presses perform the additional destination-bound
    // checkpoint above.
    let actionTimeout = min(
        targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
        max(0, deadline.timeIntervalSince(dependencies.now())))
    guard actionTimeout > 0 else { return semanticPressFailureV2("request_expired") }
    let action = target.performPress(actionTimeout)
    let commitState: String
    let timeoutRestored: Bool
    switch action {
    case .timeoutUnavailable:
        return semanticPressFailureV2("ax_messaging_timeout_unavailable")
    case let .completed(error, restored):
        switch error {
        case .success:
            commitState = "committed"
        case .actionUnsupported, .illegalArgument, .invalidUIElement,
             .notImplemented, .apiDisabled:
            commitState = "not_committed"
        default:
            // kAXErrorCannotComplete and the generic kAXErrorFailure do not
            // prove that the remote app failed before applying AXPress.
            commitState = "unknown"
        }
        timeoutRestored = restored
    }
    switch assessCheckpoint() {
    case .interference:
        return semanticPressInterferenceV2(
            commitState: commitState,
            failureCode: activeInterferenceCode)
    case .unavailable:
        return semanticPressMonitoringUnavailableV2(commitState: commitState)
    case .unchanged:
        break
    }
    if commitState == "unknown" {
        // One more sample closes the small race between the first post-call
        // checkpoint and publishing a commit-unknown acknowledgement.
        _ = assessCheckpoint()
        switch physicalAssessment {
        case .interference:
            return semanticPressInterferenceV2(
                commitState: "unknown",
                failureCode: activeInterferenceCode)
        case .unavailable:
            return semanticPressMonitoringUnavailableV2(commitState: "unknown")
        case .unchanged:
            return .init(
                status: "completed_unverified", commitState: "unknown",
                phase: "action", failureCode: "ax_press_commit_unknown")
        }
    }
    guard commitState == "committed" else {
        return semanticPressFailureV2("ax_press_failed", phase: "action")
    }

    // There is deliberately no causal predicate in schema v2. Perform one
    // bounded exact-ref observation for action-settling evidence, but never
    // translate mere target change or disappearance into "verified".
    let verification: TargetedAXPostconditionOutcomeV1<Bool> =
        runTargetedAXPostconditionVerificationV1(
            budget: .init(
                maxDuration: targetedAXPostconditionBudgetV1.maxDuration,
                maxAttempts: 1,
                retryInterval: 0,
                maxSynchronousCallDuration:
                    targetedAXPostconditionBudgetV1.maxSynchronousCallDuration),
            now: dependencies.now,
            sleep: dependencies.sleep,
            observeWithTimeout: { timeout in
                switch assessCheckpoint() {
                case .interference:
                    return .terminal(
                        failureCode: activeInterferenceCode, observation: nil)
                case .unavailable:
                    return .terminal(
                        failureCode: "interference_detection_unavailable", observation: nil)
                case .unchanged:
                    break
                }
                let observation = target.observeTarget(timeout)
                switch assessCheckpoint() {
                case .interference:
                    return .terminal(
                        failureCode: activeInterferenceCode, observation: nil)
                case .unavailable:
                    return .terminal(
                        failureCode: "interference_detection_unavailable", observation: nil)
                case .unchanged:
                    break
                }
                switch observation {
                case .present, .missing:
                    return .terminal(
                        failureCode: "postcondition_not_declared", observation: true)
                case .unavailable:
                    return .terminal(
                        failureCode: "postcondition_not_declared", observation: nil)
                }
            })
    // Final checkpoint closes the race after the last exact-ref AX read.
    _ = assessCheckpoint()
    switch physicalAssessment {
    case .interference:
        return semanticPressInterferenceV2(
            commitState: "committed",
            failureCode: activeInterferenceCode)
    case .unavailable:
        return semanticPressMonitoringUnavailableV2(commitState: "committed")
    case .unchanged:
        break
    }
    if !timeoutRestored {
        return .init(
            status: "completed_unverified", commitState: "committed",
            phase: "post_verification", failureCode: "ax_messaging_timeout_restore_failed")
    }
    switch verification {
    case .verified:
        // Unreachable because v2 has no declared predicate.
        return .init(
            status: "completed_unverified", commitState: "committed",
            phase: "post_verification", failureCode: "postcondition_not_declared")
    case let .inconclusive(failureCode, _, _):
        if failureCode == activeInterferenceCode {
            return semanticPressInterferenceV2(
                commitState: "committed",
                failureCode: activeInterferenceCode)
        }
        if failureCode == "interference_detection_unavailable" {
            return semanticPressMonitoringUnavailableV2(commitState: "committed")
        }
        return .init(
            status: "completed_unverified", commitState: "committed",
            phase: "post_verification", failureCode: "postcondition_not_declared")
    }
}

private enum SemanticPressResolvedWindowV2 {
    case unique(AXUIElement)
    case missing
    case ambiguous
}

private func resolveSemanticPressWindowV2(
    pid: Int, windowID: UInt32
) -> SemanticPressResolvedWindowV2 {
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
    return exact.first.map(SemanticPressResolvedWindowV2.unique) ?? .missing
}

func semanticPressFingerprintV2(_ element: AXUIElement) -> String? {
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

private func matchingSemanticPressFingerprintV2(
    in window: AXUIElement, fingerprint: String
) -> [AXUIElement] {
    var matches: [AXUIElement] = []
    func walk(_ element: AXUIElement) {
        if semanticPressFingerprintV2(element) == fingerprint { matches.append(element) }
        for child in axChildren(element) ?? [] { walk(child) }
    }
    for child in axChildren(window) ?? [] { walk(child) }
    return matches
}

private func performSemanticPressAXCallV2(
    element: AXUIElement, timeout: TimeInterval
) -> SemanticPressAXCallResultV2 {
    guard timeout > 0, timeout.isFinite,
          AXUIElementSetMessagingTimeout(element, Float(timeout)) == .success else {
        return .timeoutUnavailable
    }
    let result = AXUIElementPerformAction(element, kAXPressAction as CFString)
    let restored = AXUIElementSetMessagingTimeout(element, 0) == .success
    return .completed(error: result, timeoutRestored: restored)
}

private func semanticPressTargetV2(_ element: AXUIElement) -> SemanticPressTargetV2? {
    guard let role = axString(element, "AXRole"),
          let fingerprint = semanticPressFingerprintV2(element) else { return nil }
    let metadata = axValueSensitivityMetadata(element, role: role)
    return .init(
        role: role,
        fingerprint: fingerprint,
        enabled: axBool(element, "AXEnabled"),
        sensitive: isSensitiveAXValue(metadata),
        actions: axActions(element).sorted(),
        performPress: { timeout in
            performSemanticPressAXCallV2(element: element, timeout: timeout)
        },
        observeTarget: { timeout in
            let outcome = withTargetedAXMessagingTimeoutV1(
                element: element, timeout: timeout,
                operation: { () -> SemanticPressTargetObservationV2 in
                    guard let current = semanticPressFingerprintV2(element) else {
                        return .missing
                    }
                    return current == fingerprint ? .present : .missing
                })
            guard case let .completed(observation) = outcome else { return .unavailable }
            return observation
        })
}

func readSemanticPressRiskWindowTitleV2<Element>(
    element: Element, timeout: TimeInterval,
    setTimeout: (Element, Float) -> AXError,
    read: () -> String?
) -> SemanticPressRiskWindowTitleObservationV2 {
    switch withTargetedAXMessagingTimeoutV1(
        element: element, timeout: timeout, setTimeout: setTimeout,
        operation: read) {
    case let .completed(title): return .value(title)
    case .unavailable: return .unavailable
    }
}

private func liveSemanticPressRiskWindowTitleV2(
    pid: Int, windowID: UInt32, timeout: TimeInterval
) -> SemanticPressRiskWindowTitleObservationV2 {
    guard timeout > 0, timeout.isFinite else { return .unavailable }
    let startedAt = Date()
    let remaining: () -> TimeInterval = {
        max(0, timeout - Date().timeIntervalSince(startedAt))
    }
    let candidates = currentCGWindowIdentityCandidates()
    guard candidates.contains(where: {
        $0.windowID == Int(windowID) && $0.ownerPID == pid && $0.layer == 0
    }) else { return .value(nil) }

    let app = AXUIElementCreateApplication(Int32(pid))
    let appTimeout = remaining()
    guard appTimeout > 0 else { return .unavailable }
    let windows: [AXUIElement]
    switch withTargetedAXMessagingTimeoutV1(
        element: app, timeout: appTimeout,
        operation: { axWindows(app) }) {
    case let .completed(value): windows = value
    case .unavailable: return .unavailable
    }

    var exactTitles: [String] = []
    var ambiguous = false
    for window in windows {
        let callTimeout = remaining()
        guard callTimeout > 0 else { return .unavailable }
        let attributes: ((x: Double, y: Double, width: Double, height: Double)?, String?)
        switch withTargetedAXMessagingTimeoutV1(
            element: window, timeout: callTimeout,
            operation: { (elementFrame(window), axString(window, "AXTitle")) }) {
        case let .completed(value): attributes = value
        case .unavailable: return .unavailable
        }
        guard let frame = attributes.0 else { continue }
        let title = attributes.1 ?? ""
        let matches = matchingWindowCandidates(
            .init(
                pid: pid, title: title,
                frame: .init(
                    x: frame.x, y: frame.y,
                    width: frame.width, height: frame.height)),
            candidates: candidates)
        if matches.count > 1 && matches.contains(where: { $0.windowID == Int(windowID) }) {
            ambiguous = true
        } else if matches.count == 1, matches[0].windowID == Int(windowID) {
            exactTitles.append(title)
        }
    }
    guard !ambiguous, exactTitles.count <= 1 else { return .unavailable }
    return .value(exactTitles.first)
}

let productionSemanticPressDependenciesV2 = SemanticPressDependenciesV2(
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
        switch resolveSemanticPressWindowV2(pid: pid, windowID: windowID) {
        case .unique: return .unique
        case .missing: return .missing
        case .ambiguous: return .ambiguous
        }
    },
    resolveTarget: { pid, windowID, path in
        guard case let .unique(window) = resolveSemanticPressWindowV2(
            pid: pid, windowID: windowID),
            let element = resolveElement(in: window, path: path) else { return nil }
        return semanticPressTargetV2(element)
    },
    countFingerprint: { pid, windowID, fingerprint in
        guard case let .unique(window) = resolveSemanticPressWindowV2(
            pid: pid, windowID: windowID) else { return 0 }
        return matchingSemanticPressFingerprintV2(
            in: window, fingerprint: fingerprint).count
    },
    windowTitle: { pid, windowID, timeout in
        liveSemanticPressRiskWindowTitleV2(
            pid: pid, windowID: windowID, timeout: timeout)
    },
    frontmostPID: {
        refreshAppKitState()
        return NSWorkspace.shared.frontmostApplication.map {
            Int($0.processIdentifier)
        }
    },
    observePhysicalInput: observePhysicalInputInterferenceV1,
    now: Date.init,
    sleep: Thread.sleep(forTimeInterval:))
