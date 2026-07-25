import ApplicationServices
import AppKit
import CryptoKit
import Foundation

struct SemanticScrollRequestV1: Codable, Equatable {
    let schemaVersion: Int
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let ref: String
    let path: String
    let expectedRole: String
    let expectedFingerprint: String
    let axis: String
    let direction: String
    let steps: Int
    let fallbackPolicy: String
    let commitDeadlineAt: String

    init(
        schemaVersion: Int = 1, pid: Int, bundleID: String, windowID: UInt32,
        ref: String, path: String, expectedRole: String, expectedFingerprint: String,
        axis: String, direction: String, steps: Int,
        fallbackPolicy: String = "report_unsupported", commitDeadlineAt: String
    ) {
        self.schemaVersion = schemaVersion
        self.pid = pid
        self.bundleID = bundleID
        self.windowID = windowID
        self.ref = ref
        self.path = path
        self.expectedRole = expectedRole
        self.expectedFingerprint = expectedFingerprint
        self.axis = axis
        self.direction = direction
        self.steps = steps
        self.fallbackPolicy = fallbackPolicy
        self.commitDeadlineAt = commitDeadlineAt
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "pid", "bundle_id", "window_id", "ref", "path",
            "expected_role", "expected_fingerprint", "axis", "direction", "steps",
            "fallback_policy", "commit_deadline_at",
        ], field: "semantic_scroll_v1 params")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        pid = try container.decode(Int.self, forKey: strictMutationKey("pid"))
        bundleID = try container.decode(String.self, forKey: strictMutationKey("bundle_id"))
        let rawWindowID = try container.decode(Int.self, forKey: strictMutationKey("window_id"))
        ref = try container.decode(String.self, forKey: strictMutationKey("ref"))
        path = try container.decode(String.self, forKey: strictMutationKey("path"))
        expectedRole = try container.decode(String.self, forKey: strictMutationKey("expected_role"))
        expectedFingerprint = try container.decode(
            String.self, forKey: strictMutationKey("expected_fingerprint"))
        axis = try container.decode(String.self, forKey: strictMutationKey("axis"))
        direction = try container.decode(String.self, forKey: strictMutationKey("direction"))
        steps = try container.decode(Int.self, forKey: strictMutationKey("steps"))
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
              Set(["vertical", "horizontal"]).contains(axis),
              Set(["increment", "decrement"]).contains(direction),
              (1...10).contains(steps), fallbackPolicy == "report_unsupported",
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid("invalid semantic_scroll_v1 authority")
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
        try container.encode(axis, forKey: strictMutationKey("axis"))
        try container.encode(direction, forKey: strictMutationKey("direction"))
        try container.encode(steps, forKey: strictMutationKey("steps"))
        try container.encode(fallbackPolicy, forKey: strictMutationKey("fallback_policy"))
        try container.encode(commitDeadlineAt, forKey: strictMutationKey("commit_deadline_at"))
    }
}

struct SemanticScrollRPCRequestV1: Codable, Equatable {
    let id: Int64
    let method: String
    let params: SemanticScrollRequestV1

    init(id: Int64, method: String = "semantic_scroll_v1", params: SemanticScrollRequestV1) {
        self.id = id
        self.method = method
        self.params = params
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"],
            field: "semantic_scroll_v1 envelope")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(
            SemanticScrollRequestV1.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "semantic_scroll_v1" else {
            throw StrictMutationWireError.invalid("invalid semantic_scroll_v1 envelope")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(id, forKey: strictMutationKey("id"))
        try container.encode(method, forKey: strictMutationKey("method"))
        try container.encode(params, forKey: strictMutationKey("params"))
    }
}

func decodeSemanticScrollRPCRequestV1(_ payload: Data) throws -> SemanticScrollRPCRequestV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticScrollRPCRequestV1.self, from: payload)
}

struct SemanticScrollResultV1: Codable, Equatable {
    let schemaVersion: Int
    let status: String
    let commitState: String
    let phase: String
    let failureCode: String?
    let retrySafe: Bool
    let postcondition: String?
    let initialValue: Double?
    let finalValue: Double?
    let stepsCompleted: Int
    let expectedSteps: Int

    init(
        status: String, commitState: String, phase: String, failureCode: String?,
        postcondition: String? = nil, initialValue: Double? = nil,
        finalValue: Double? = nil, stepsCompleted: Int = 0, expectedSteps: Int = 1
    ) {
        schemaVersion = 1
        self.status = status
        self.commitState = commitState
        self.phase = phase
        self.failureCode = failureCode
        retrySafe = false
        self.postcondition = postcondition
        self.initialValue = initialValue
        self.finalValue = finalValue
        self.stepsCompleted = stepsCompleted
        self.expectedSteps = expectedSteps
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "status", "commit_state", "phase", "failure_code",
            "retry_safe", "postcondition", "initial_value", "final_value",
            "steps_completed", "expected_steps",
        ], field: "semantic_scroll_v1 result")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        status = try container.decode(String.self, forKey: strictMutationKey("status"))
        commitState = try container.decode(String.self, forKey: strictMutationKey("commit_state"))
        phase = try container.decode(String.self, forKey: strictMutationKey("phase"))
        failureCode = try container.decodeIfPresent(String.self, forKey: strictMutationKey("failure_code"))
        retrySafe = try container.decode(Bool.self, forKey: strictMutationKey("retry_safe"))
        postcondition = try container.decodeIfPresent(String.self, forKey: strictMutationKey("postcondition"))
        initialValue = try container.decodeIfPresent(Double.self, forKey: strictMutationKey("initial_value"))
        finalValue = try container.decodeIfPresent(Double.self, forKey: strictMutationKey("final_value"))
        stepsCompleted = try container.decode(Int.self, forKey: strictMutationKey("steps_completed"))
        expectedSteps = try container.decode(Int.self, forKey: strictMutationKey("expected_steps"))
        guard validateSemanticScrollResultV1(self) else {
            throw StrictMutationWireError.invalid("invalid semantic_scroll_v1 result tagged union")
        }
    }

    func encode(to encoder: Encoder) throws {
        guard validateSemanticScrollResultV1(self) else {
            throw StrictMutationWireError.invalid("invalid semantic_scroll_v1 result tagged union")
        }
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(status, forKey: strictMutationKey("status"))
        try container.encode(commitState, forKey: strictMutationKey("commit_state"))
        try container.encode(phase, forKey: strictMutationKey("phase"))
        try container.encode(failureCode, forKey: strictMutationKey("failure_code"))
        try container.encode(retrySafe, forKey: strictMutationKey("retry_safe"))
        try container.encode(postcondition, forKey: strictMutationKey("postcondition"))
        try container.encode(initialValue, forKey: strictMutationKey("initial_value"))
        try container.encode(finalValue, forKey: strictMutationKey("final_value"))
        try container.encode(stepsCompleted, forKey: strictMutationKey("steps_completed"))
        try container.encode(expectedSteps, forKey: strictMutationKey("expected_steps"))
    }
}

func decodeSemanticScrollResultV1(_ payload: Data) throws -> SemanticScrollResultV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(SemanticScrollResultV1.self, from: payload)
}

private func validateSemanticScrollResultV1(_ result: SemanticScrollResultV1) -> Bool {
    guard result.schemaVersion == 1, !result.retrySafe,
          (1...10).contains(result.expectedSteps),
          (0...result.expectedSteps).contains(result.stepsCompleted),
          result.initialValue?.isFinite ?? true,
          result.finalValue?.isFinite ?? true else { return false }
    switch result.status {
    case "verified":
        return result.commitState == "committed" && result.phase == "post_verification" &&
            result.failureCode == nil && result.postcondition == "scroll_value_changed_in_direction" &&
            result.initialValue != nil && result.finalValue != nil &&
            result.stepsCompleted == result.expectedSteps
    case "completed_unverified":
        if result.commitState == "unknown" {
            return result.phase == "action" && result.failureCode == "ax_scroll_commit_unknown" &&
                result.postcondition == nil
        }
        let codes = Set([
            "scroll_value_unchanged", "scroll_value_wrong_direction",
            "scroll_value_not_observed", "scroll_boundary_reached", "scroll_target_changed",
            "ax_scroll_failed", "interference_detection_unavailable",
            "ax_messaging_timeout_restore_failed",
        ])
        return result.commitState == "committed" && result.phase == "post_verification" &&
            codes.contains(result.failureCode ?? "") && result.postcondition == nil
    case "user_interference":
        return Set(["not_committed", "committed", "unknown"]).contains(result.commitState) &&
            result.phase == "user_interference" &&
            result.failureCode == "physical_input_interference" && result.postcondition == nil &&
            (result.commitState != "not_committed" || result.stepsCompleted == 0)
    case "cancelled":
        return Set(["not_committed", "committed", "unknown"]).contains(result.commitState) &&
            result.phase == "cancelled" && result.failureCode == "controller_cancelled" &&
            result.postcondition == nil &&
            (result.commitState != "not_committed" || result.stepsCompleted == 0)
    case "fallback_required":
        return result.commitState == "not_committed" && result.phase == "preflight" &&
            result.failureCode == "ax_scroll_metric_unsupported" &&
            result.stepsCompleted == 0 && result.postcondition == nil
    case "failed":
        let preflight = Set([
            "invalid_request", "request_expired", "process_not_live",
            "process_identity_mismatch", "window_not_found", "window_ambiguous",
            "path_not_found", "role_mismatch", "fingerprint_mismatch",
            "fingerprint_not_found", "fingerprint_ambiguous", "sensitive_target",
            "enabled_unknown", "target_disabled", "scroll_boundary",
            "interference_detection_unavailable", "ax_messaging_timeout_unavailable",
        ])
        let exactPhase = result.failureCode.map {
            preflight.contains($0) ? result.phase == "preflight" :
                ($0 == "ax_scroll_failed" && result.phase == "action")
        } ?? false
        return result.commitState == "not_committed" && result.stepsCompleted == 0 &&
            exactPhase && result.postcondition == nil
    default:
        return false
    }
}

enum SemanticScrollWindowResolutionV1 { case unique, missing, ambiguous, timeoutUnavailable }
enum SemanticScrollTargetResolutionV1 {
    case target(SemanticScrollTargetV1)
    case missing
    case timeoutUnavailable
}
enum SemanticScrollFingerprintCountV1 {
    case count(Int)
    case timeoutUnavailable
}
enum SemanticScrollAXCallResultV1 {
    case completed(error: AXError, timeoutRestored: Bool)
    case timeoutUnavailable
}
enum SemanticScrollObservationCallResultV1 {
    case completed(value: Double?, timeoutRestored: Bool)
    case timeoutUnavailable
}

struct SemanticScrollTargetV1 {
    let role: String
    let fingerprint: String
    let enabled: Bool?
    let sensitive: Bool
    let metricSupported: Bool
    let actionSupported: Bool
    let minValue: Double?
    let maxValue: Double?
    let performAction: (String, TimeInterval) -> SemanticScrollAXCallResultV1
    let observeValue: (TimeInterval) -> SemanticScrollObservationCallResultV1
}

struct SemanticScrollDependenciesV1 {
    let isPIDLive: (Int) -> Bool
    let bundleIDForPID: (Int) -> String?
    let resolveWindow: (Int, UInt32, TimeInterval) -> SemanticScrollWindowResolutionV1
    let resolveTarget: (Int, UInt32, String, String, String, TimeInterval) ->
        SemanticScrollTargetResolutionV1
    let countFingerprint: (Int, UInt32, String, TimeInterval) ->
        SemanticScrollFingerprintCountV1
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let isCancelled: () -> Bool
    let now: () -> Date
    let sleep: (TimeInterval) -> Void
}

private func scrollResultV1(
    _ request: SemanticScrollRequestV1, status: String, commitState: String,
    phase: String, failureCode: String?, postcondition: String? = nil,
    initial: Double? = nil, final: Double? = nil, completed: Int = 0
) -> SemanticScrollResultV1 {
    .init(
        status: status, commitState: commitState, phase: phase,
        failureCode: failureCode, postcondition: postcondition,
        initialValue: initial, finalValue: final,
        stepsCompleted: completed, expectedSteps: request.steps)
}

private func semanticScrollFailureV1(
    _ request: SemanticScrollRequestV1, _ code: String, phase: String = "preflight"
) -> SemanticScrollResultV1 {
    scrollResultV1(
        request, status: "failed", commitState: "not_committed",
        phase: phase, failureCode: code)
}

private func semanticScrollInterferenceV1(
    _ request: SemanticScrollRequestV1, commitState: String,
    initial: Double?, final: Double?, completed: Int
) -> SemanticScrollResultV1 {
    scrollResultV1(
        request, status: "user_interference", commitState: commitState,
        phase: "user_interference", failureCode: "physical_input_interference",
        initial: initial, final: final, completed: completed)
}

private func semanticScrollCancelledV1(
    _ request: SemanticScrollRequestV1, commitState: String,
    initial: Double?, final: Double?, completed: Int
) -> SemanticScrollResultV1 {
    scrollResultV1(
        request, status: "cancelled", commitState: commitState,
        phase: "cancelled", failureCode: "controller_cancelled",
        initial: initial, final: final, completed: completed)
}

private func semanticScrollPartialV1(
    _ request: SemanticScrollRequestV1, code: String,
    initial: Double?, final: Double?, completed: Int
) -> SemanticScrollResultV1 {
    return scrollResultV1(
        request, status: "completed_unverified", commitState: "committed",
        phase: "post_verification", failureCode: code,
        initial: initial, final: final, completed: completed)
}

private func semanticScrollTargetPreflightCodeV1(
    request: SemanticScrollRequestV1, target: SemanticScrollTargetV1?, fingerprintCount: Int
) -> String? {
    guard let target else { return "path_not_found" }
    guard target.role == request.expectedRole else { return "role_mismatch" }
    guard target.fingerprint == request.expectedFingerprint else { return "fingerprint_mismatch" }
    guard fingerprintCount == 1 else {
        return fingerprintCount == 0 ? "fingerprint_not_found" : "fingerprint_ambiguous"
    }
    guard !target.sensitive else { return "sensitive_target" }
    guard target.enabled == true else { return target.enabled == nil ? "enabled_unknown" : "target_disabled" }
    return nil
}

private func semanticScrollDirectionMatchesV1(
    _ request: SemanticScrollRequestV1, before: Double, after: Double
) -> Bool {
    request.direction == "increment" ? after > before : after < before
}

private func semanticScrollAtBoundaryV1(
    _ request: SemanticScrollRequestV1, target: SemanticScrollTargetV1, value: Double
) -> Bool {
    let tolerance = 0.000_000_1
    if request.direction == "increment", let max = target.maxValue {
        return value >= max - tolerance
    }
    if request.direction == "decrement", let min = target.minValue {
        return value <= min + tolerance
    }
    return false
}

func runSemanticScrollV1(
    request: SemanticScrollRequestV1, dependencies: SemanticScrollDependenciesV1
) -> SemanticScrollResultV1 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return semanticScrollFailureV1(request, "invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else { return semanticScrollFailureV1(request, "request_expired") }
    guard horizon <= 3 else { return semanticScrollFailureV1(request, "invalid_request") }
    guard !dependencies.isCancelled() else {
        return semanticScrollCancelledV1(
            request, commitState: "not_committed", initial: nil, final: nil, completed: 0)
    }
    guard dependencies.isPIDLive(request.pid) else {
        return semanticScrollFailureV1(request, "process_not_live")
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return semanticScrollFailureV1(request, "process_identity_mismatch")
    }
    let initialWindowTimeout = min(
        targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
        max(0, deadline.timeIntervalSince(dependencies.now())))
    guard initialWindowTimeout > 0 else {
        return semanticScrollFailureV1(request, "request_expired")
    }
    switch dependencies.resolveWindow(request.pid, request.windowID, initialWindowTimeout) {
    case .missing: return semanticScrollFailureV1(request, "window_not_found")
    case .ambiguous: return semanticScrollFailureV1(request, "window_ambiguous")
    case .timeoutUnavailable:
        return semanticScrollFailureV1(request, "ax_messaging_timeout_unavailable")
    case .unique: break
    }
    let uniquenessTimeout = min(
        targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
        max(0, deadline.timeIntervalSince(dependencies.now())))
    guard uniquenessTimeout > 0 else {
        return semanticScrollFailureV1(request, "request_expired")
    }
    let initialFingerprintCount: Int
    switch dependencies.countFingerprint(
        request.pid, request.windowID, request.expectedFingerprint, uniquenessTimeout) {
    case let .count(value): initialFingerprintCount = value
    case .timeoutUnavailable:
        return semanticScrollFailureV1(request, "ax_messaging_timeout_unavailable")
    }
    guard initialFingerprintCount == 1 else {
        return semanticScrollFailureV1(
            request,
            initialFingerprintCount == 0 ? "fingerprint_not_found" : "fingerprint_ambiguous")
    }

    var physicalBaseline: PhysicalInputInterferenceSnapshotV1?
    var physicalAssessment: PhysicalInputInterferenceAssessmentV1 = .unchanged
    let assessCheckpoint: () -> PhysicalInputInterferenceAssessmentV1 = {
        guard physicalAssessment == .unchanged else { return physicalAssessment }
        let current = dependencies.observePhysicalInput()
        let assessment: PhysicalInputInterferenceAssessmentV1
        if physicalBaseline == nil {
            assessment = assessPhysicalInputInterferenceV1(
                baseline: current, current: current, expectedPointer: current?.pointer,
                expectedSyntheticEvents: [])
        } else {
            assessment = assessPhysicalInputInterferenceV1(
                baseline: physicalBaseline, current: current,
                expectedPointer: physicalBaseline?.pointer, expectedSyntheticEvents: [])
        }
        if assessment == .unchanged { physicalBaseline = current }
        else { physicalAssessment = assessment }
        return assessment
    }

    var initialValue: Double?
    var finalValue: Double?
    var completed = 0
    for _ in 0..<request.steps {
        // Each semantic step is its own bounded commit boundary. Re-check the
        // deadline, immutable identity, exact target, and physical operator.
        if dependencies.isCancelled() {
            return semanticScrollCancelledV1(
                request, commitState: completed > 0 ? "committed" : "not_committed",
                initial: initialValue, final: finalValue, completed: completed)
        }
        guard dependencies.now() < deadline else {
            return completed == 0 ? semanticScrollFailureV1(request, "request_expired") :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        guard dependencies.isPIDLive(request.pid),
              dependencies.bundleIDForPID(request.pid) == request.bundleID else {
            return completed == 0 ? semanticScrollFailureV1(request, "process_identity_mismatch") :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        let windowTimeout = min(
            targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
            max(0, deadline.timeIntervalSince(dependencies.now())))
        guard windowTimeout > 0 else {
            return completed == 0 ? semanticScrollFailureV1(request, "request_expired") :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        switch dependencies.resolveWindow(request.pid, request.windowID, windowTimeout) {
        case .unique: break
        case .missing, .ambiguous:
            return completed == 0 ? semanticScrollFailureV1(request, "window_not_found") :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        case .timeoutUnavailable:
            return completed == 0 ? semanticScrollFailureV1(
                request, "ax_messaging_timeout_unavailable") : semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        let targetResolution = dependencies.resolveTarget(
            request.pid, request.windowID, request.path, request.axis,
            request.direction, targetedAXPostconditionBudgetV1.maxSynchronousCallDuration)
        let target: SemanticScrollTargetV1?
        switch targetResolution {
        case let .target(value): target = value
        case .missing: target = nil
        case .timeoutUnavailable:
            return completed == 0 ? semanticScrollFailureV1(
                request, "ax_messaging_timeout_unavailable") : semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        if let code = semanticScrollTargetPreflightCodeV1(
            request: request, target: target,
            fingerprintCount: initialFingerprintCount) {
            return completed == 0 ? semanticScrollFailureV1(request, code) :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        guard let target else { return semanticScrollFailureV1(request, "path_not_found") }
        guard target.metricSupported, target.actionSupported else {
            if completed == 0 {
                return scrollResultV1(
                    request, status: "fallback_required", commitState: "not_committed",
                    phase: "preflight", failureCode: "ax_scroll_metric_unsupported")
            }
            return semanticScrollPartialV1(
                request, code: "scroll_target_changed", initial: initialValue,
                final: finalValue, completed: completed)
        }
        let callTimeout = min(
            targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
            max(0, deadline.timeIntervalSince(dependencies.now())))
        guard callTimeout > 0 else {
            return completed == 0 ? semanticScrollFailureV1(request, "request_expired") :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        let beforeObservation = target.observeValue(callTimeout)
        let before: Double
        switch beforeObservation {
        case .timeoutUnavailable:
            return completed == 0 ? semanticScrollFailureV1(
                request, "ax_messaging_timeout_unavailable") : semanticScrollPartialV1(
                    request, code: "scroll_value_not_observed", initial: initialValue,
                    final: finalValue, completed: completed)
        case let .completed(value, restored):
            guard restored else {
                return completed == 0 ? semanticScrollFailureV1(
                    request, "ax_messaging_timeout_unavailable") : semanticScrollPartialV1(
                        request, code: "ax_messaging_timeout_restore_failed",
                        initial: initialValue, final: finalValue, completed: completed)
            }
            guard let value, value.isFinite else {
                if completed == 0 {
                    return scrollResultV1(
                        request, status: "fallback_required", commitState: "not_committed",
                        phase: "preflight", failureCode: "ax_scroll_metric_unsupported")
                }
                return semanticScrollPartialV1(
                    request, code: "scroll_value_not_observed", initial: initialValue,
                    final: finalValue, completed: completed)
            }
            before = value
        }
        if initialValue == nil { initialValue = before }
        finalValue = before
        if semanticScrollAtBoundaryV1(request, target: target, value: before) {
            if completed == 0 { return semanticScrollFailureV1(request, "scroll_boundary") }
            return semanticScrollPartialV1(
                request, code: "scroll_boundary_reached", initial: initialValue,
                final: finalValue, completed: completed)
        }
        switch assessCheckpoint() {
        case .interference:
            return semanticScrollInterferenceV1(
                request, commitState: completed > 0 ? "committed" : "not_committed",
                initial: initialValue, final: finalValue, completed: completed)
        case .unavailable:
            if completed == 0 { return semanticScrollFailureV1(request, "interference_detection_unavailable") }
            return semanticScrollPartialV1(
                request, code: "interference_detection_unavailable",
                initial: initialValue, final: finalValue, completed: completed)
        case .unchanged: break
        }
        if dependencies.isCancelled() {
            return semanticScrollCancelledV1(
                request, commitState: completed > 0 ? "committed" : "not_committed",
                initial: initialValue, final: finalValue, completed: completed)
        }
        guard dependencies.now() < deadline else {
            return completed == 0 ? semanticScrollFailureV1(request, "request_expired") :
                semanticScrollPartialV1(
                    request, code: "scroll_target_changed", initial: initialValue,
                    final: finalValue, completed: completed)
        }
        let actionName = request.direction == "increment" ? kAXIncrementAction : kAXDecrementAction
        let action = target.performAction(actionName as String, callTimeout)
        let actionCommitState: String
        let actionTimeoutRestored: Bool
        switch action {
        case .timeoutUnavailable:
            return completed == 0 ? semanticScrollFailureV1(
                request, "ax_messaging_timeout_unavailable") : semanticScrollPartialV1(
                    request, code: "ax_messaging_timeout_restore_failed",
                    initial: initialValue, final: finalValue, completed: completed)
        case let .completed(error, restored):
            actionTimeoutRestored = restored
            switch error {
            case .success: actionCommitState = "committed"
            case .attributeUnsupported, .actionUnsupported, .illegalArgument,
                 .invalidUIElement, .notImplemented, .apiDisabled:
                actionCommitState = "not_committed"
            default: actionCommitState = "unknown"
            }
        }
        if dependencies.isCancelled() {
            let overall = actionCommitState == "unknown" ? "unknown" :
                (actionCommitState == "committed" || completed > 0 ? "committed" : "not_committed")
            return semanticScrollCancelledV1(
                request, commitState: overall, initial: initialValue,
                final: finalValue, completed: completed)
        }
        switch assessCheckpoint() {
        case .interference:
            let overall = actionCommitState == "unknown" ? "unknown" :
                (actionCommitState == "committed" || completed > 0 ? "committed" : "not_committed")
            return semanticScrollInterferenceV1(
                request, commitState: overall, initial: initialValue,
                final: finalValue, completed: completed)
        case .unavailable:
            if actionCommitState == "unknown" {
                return scrollResultV1(
                    request, status: "completed_unverified", commitState: "unknown",
                    phase: "action", failureCode: "ax_scroll_commit_unknown",
                    initial: initialValue, final: finalValue, completed: completed)
            }
            if actionCommitState == "committed" || completed > 0 {
                return semanticScrollPartialV1(
                    request, code: "interference_detection_unavailable",
                    initial: initialValue, final: finalValue, completed: completed)
            }
            return semanticScrollFailureV1(request, "interference_detection_unavailable")
        case .unchanged: break
        }
        if actionCommitState == "unknown" {
            return scrollResultV1(
                request, status: "completed_unverified", commitState: "unknown",
                phase: "action", failureCode: "ax_scroll_commit_unknown",
                initial: initialValue, final: finalValue, completed: completed)
        }
        guard actionCommitState == "committed" else {
            return completed == 0 ? semanticScrollFailureV1(
                request, "ax_scroll_failed", phase: "action") : semanticScrollPartialV1(
                    request, code: "ax_scroll_failed", initial: initialValue,
                    final: finalValue, completed: completed)
        }

        let verification: TargetedAXPostconditionOutcomeV1<Double> =
            runTargetedAXPostconditionVerificationV1(
                now: dependencies.now, sleep: dependencies.sleep,
                observeWithTimeout: { timeout in
                    if dependencies.isCancelled() {
                        return .terminal(failureCode: "controller_cancelled", observation: nil)
                    }
                    switch assessCheckpoint() {
                    case .interference:
                        return .terminal(failureCode: "physical_input_interference", observation: nil)
                    case .unavailable:
                        return .terminal(failureCode: "interference_detection_unavailable", observation: nil)
                    case .unchanged: break
                    }
                    guard dependencies.now() < deadline else {
                        return .terminal(failureCode: "scroll_target_changed", observation: nil)
                    }
                    let current: SemanticScrollTargetV1
                    switch dependencies.resolveTarget(
                        request.pid, request.windowID, request.path,
                        request.axis, request.direction, timeout) {
                    case let .target(value): current = value
                    case .missing:
                        return .terminal(failureCode: "scroll_target_changed", observation: nil)
                    case .timeoutUnavailable:
                        return .terminal(
                            failureCode: "ax_messaging_timeout_restore_failed", observation: nil)
                    }
                    guard semanticScrollTargetPreflightCodeV1(
                            request: request, target: current,
                            fingerprintCount: initialFingerprintCount) == nil else {
                        return .terminal(failureCode: "scroll_target_changed", observation: nil)
                    }
                    let observation = current.observeValue(timeout)
                    if dependencies.isCancelled() {
                        return .terminal(failureCode: "controller_cancelled", observation: nil)
                    }
                    switch assessCheckpoint() {
                    case .interference:
                        return .terminal(failureCode: "physical_input_interference", observation: nil)
                    case .unavailable:
                        return .terminal(failureCode: "interference_detection_unavailable", observation: nil)
                    case .unchanged: break
                    }
                    switch observation {
                    case .timeoutUnavailable:
                        return .terminal(failureCode: "scroll_value_not_observed", observation: nil)
                    case let .completed(value, restored):
                        guard restored else {
                            return .terminal(
                                failureCode: "ax_messaging_timeout_restore_failed", observation: nil)
                        }
                        guard let value, value.isFinite else {
                            return .retryable(failureCode: "scroll_value_not_observed", observation: nil)
                        }
                        if value == before {
                            return .retryable(failureCode: "scroll_value_unchanged", observation: value)
                        }
                        guard semanticScrollDirectionMatchesV1(request, before: before, after: value) else {
                            return .terminal(failureCode: "scroll_value_wrong_direction", observation: value)
                        }
                        return .matched(value)
                    }
                })
        _ = assessCheckpoint()
        if dependencies.isCancelled() {
            return semanticScrollCancelledV1(
                request, commitState: "committed", initial: initialValue,
                final: finalValue, completed: completed)
        }
        if physicalAssessment == .interference {
            return semanticScrollInterferenceV1(
                request, commitState: "committed", initial: initialValue,
                final: finalValue, completed: completed)
        }
        if physicalAssessment == .unavailable {
            return semanticScrollPartialV1(
                request, code: "interference_detection_unavailable",
                initial: initialValue, final: finalValue, completed: completed)
        }
        guard actionTimeoutRestored else {
            return semanticScrollPartialV1(
                request, code: "ax_messaging_timeout_restore_failed",
                initial: initialValue, final: finalValue, completed: completed)
        }
        switch verification {
        case let .verified(value, _):
            finalValue = value
            completed += 1
        case let .inconclusive(code, value, _):
            if code == "controller_cancelled" {
                return semanticScrollCancelledV1(
                    request, commitState: "committed", initial: initialValue,
                    final: value ?? finalValue, completed: completed)
            }
            if code == "physical_input_interference" {
                return semanticScrollInterferenceV1(
                    request, commitState: "committed", initial: initialValue,
                    final: value ?? finalValue, completed: completed)
            }
            return semanticScrollPartialV1(
                request, code: code, initial: initialValue,
                final: value ?? finalValue, completed: completed)
        }
    }
    return scrollResultV1(
        request, status: "verified", commitState: "committed",
        phase: "post_verification", failureCode: nil,
        postcondition: "scroll_value_changed_in_direction",
        initial: initialValue, final: finalValue, completed: completed)
}

private enum SemanticScrollResolvedWindowV1 {
    case unique(AXUIElement), missing, ambiguous, timeoutUnavailable
}

private func resolveSemanticScrollWindowV1(
    pid: Int, windowID: UInt32, timeout: TimeInterval
) -> SemanticScrollResolvedWindowV1 {
    guard timeout > 0, timeout.isFinite else { return .timeoutUnavailable }
    let startedAt = Date()
    let candidates = currentCGWindowIdentityCandidates()
    guard candidates.contains(where: {
        $0.windowID == Int(windowID) && $0.ownerPID == pid && $0.layer == 0
    }) else { return .missing }
    let app = AXUIElementCreateApplication(Int32(pid))
    let appTimeout = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
    guard appTimeout > 0 else { return .timeoutUnavailable }
    let windows: [AXUIElement]
    switch withTargetedAXMessagingTimeoutV1(
        element: app, timeout: appTimeout, operation: { axWindows(app) }) {
    case let .completed(value): windows = value
    case .unavailable: return .timeoutUnavailable
    }
    var exact: [AXUIElement] = []
    var ambiguous = false
    for window in windows {
        let windowTimeout = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
        guard windowTimeout > 0 else { return .timeoutUnavailable }
        let observation: ((x: Double, y: Double, width: Double, height: Double)?, String?)
        switch withTargetedAXMessagingTimeoutV1(
            element: window, timeout: windowTimeout,
            operation: { (elementFrame(window), axString(window, "AXTitle")) }) {
        case let .completed(value): observation = value
        case .unavailable: return .timeoutUnavailable
        }
        guard let frame = observation.0 else { continue }
        let matches = matchingWindowCandidates(
            .init(
                pid: pid, title: observation.1 ?? "",
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
    return exact.first.map(SemanticScrollResolvedWindowV1.unique) ?? .missing
}

private func semanticScrollBarV1(
    target: AXUIElement, axis: String
) -> AXUIElement? {
    if axString(target, "AXRole") == "AXScrollBar" { return target }
    let attribute = axis == "vertical" ? kAXVerticalScrollBarAttribute : kAXHorizontalScrollBarAttribute
    var raw: CFTypeRef?
    guard AXUIElementCopyAttributeValue(
        target, attribute as CFString, &raw) == .success,
        let raw, CFGetTypeID(raw) == AXUIElementGetTypeID() else { return nil }
    return (raw as! AXUIElement)
}

private func semanticScrollNumberV1(_ element: AXUIElement, _ attribute: String) -> Double? {
    guard let number = axValue(element, attribute) as? NSNumber else { return nil }
    let value = number.doubleValue
    return value.isFinite ? value : nil
}

private func performSemanticScrollAXCallV1(
    scrollbar: AXUIElement, action: String, timeout: TimeInterval
) -> SemanticScrollAXCallResultV1 {
    guard timeout > 0, timeout.isFinite,
          AXUIElementSetMessagingTimeout(scrollbar, Float(timeout)) == .success else {
        return .timeoutUnavailable
    }
    let result = AXUIElementPerformAction(scrollbar, action as CFString)
    let restored = AXUIElementSetMessagingTimeout(scrollbar, 0) == .success
    return .completed(error: result, timeoutRestored: restored)
}

private func observeSemanticScrollValueV1(
    scrollbar: AXUIElement, timeout: TimeInterval
) -> SemanticScrollObservationCallResultV1 {
    guard timeout > 0, timeout.isFinite,
          AXUIElementSetMessagingTimeout(scrollbar, Float(timeout)) == .success else {
        return .timeoutUnavailable
    }
    let value = semanticScrollNumberV1(scrollbar, kAXValueAttribute as String)
    let restored = AXUIElementSetMessagingTimeout(scrollbar, 0) == .success
    return .completed(value: value, timeoutRestored: restored)
}

private enum SemanticScrollElementResolutionV1 {
    case element(AXUIElement)
    case missing
    case timeoutUnavailable
}

private func semanticScrollRemainingV1(
    startedAt: Date, budget: TimeInterval
) -> TimeInterval {
    max(0, budget - Date().timeIntervalSince(startedAt))
}

private func resolveSemanticScrollElementV1(
    in window: AXUIElement, path: String, timeout: TimeInterval
) -> SemanticScrollElementResolutionV1 {
    guard timeout > 0, timeout.isFinite else { return .timeoutUnavailable }
    let parts = path.split(separator: "/")
    guard parts.first == "window[0]" else { return .missing }
    let startedAt = Date()
    var current = window
    for part in parts.dropFirst() {
        guard let bracketStart = part.firstIndex(of: "["),
              let bracketEnd = part.firstIndex(of: "]"),
              let index = Int(part[part.index(after: bracketStart)..<bracketEnd]) else {
            return .missing
        }
        let expectedRole = String(part[part.startIndex..<bracketStart])
        let childrenTimeout = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
        guard childrenTimeout > 0 else { return .timeoutUnavailable }
        let children: [AXUIElement]
        switch withTargetedAXMessagingTimeoutV1(
            element: current, timeout: childrenTimeout,
            operation: { axChildren(current) ?? [] }) {
        case let .completed(value): children = value
        case .unavailable: return .timeoutUnavailable
        }
        var roleIndex = 0
        var match: AXUIElement?
        for child in children {
            let roleTimeout = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
            guard roleTimeout > 0 else { return .timeoutUnavailable }
            let role: String?
            switch withTargetedAXMessagingTimeoutV1(
                element: child, timeout: roleTimeout,
                operation: { axString(child, "AXRole") }) {
            case let .completed(value): role = value
            case .unavailable: return .timeoutUnavailable
            }
            if role == expectedRole {
                if roleIndex == index { match = child; break }
                roleIndex += 1
            }
        }
        guard let match else { return .missing }
        current = match
    }
    return .element(current)
}

private func semanticScrollTargetV1(
    in window: AXUIElement, path: String, axis: String,
    direction: String, timeout: TimeInterval
) -> SemanticScrollTargetResolutionV1 {
    let startedAt = Date()
    let target: AXUIElement
    switch resolveSemanticScrollElementV1(in: window, path: path, timeout: timeout) {
    case let .element(value): target = value
    case .missing: return .missing
    case .timeoutUnavailable: return .timeoutUnavailable
    }
    let metadataTimeout = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
    guard metadataTimeout > 0 else { return .timeoutUnavailable }
    typealias TargetMetadata = (
        role: String?, fingerprint: String?, enabled: Bool?, sensitive: Bool,
        scrollbar: AXUIElement?)
    let targetMetadata: TargetMetadata
    switch withTargetedAXMessagingTimeoutV1(
        element: target, timeout: metadataTimeout,
        operation: { () -> TargetMetadata in
            let role = axString(target, "AXRole")
            let metadata = axValueSensitivityMetadata(target, role: role ?? "")
            return (
                role, semanticPressFingerprintV2(target), axBool(target, "AXEnabled"),
                isSensitiveAXValue(metadata),
                semanticScrollBarV1(target: target, axis: axis))
        }) {
    case let .completed(value): targetMetadata = value
    case .unavailable: return .timeoutUnavailable
    }
    guard let role = targetMetadata.role,
          let fingerprint = targetMetadata.fingerprint else { return .missing }
    guard let scrollbar = targetMetadata.scrollbar else {
        return .target(.init(
            role: role, fingerprint: fingerprint, enabled: targetMetadata.enabled,
            sensitive: targetMetadata.sensitive, metricSupported: false,
            actionSupported: false, minValue: nil, maxValue: nil,
            performAction: { _, _ in
                .completed(error: .actionUnsupported, timeoutRestored: true)
            },
            observeValue: { _ in .completed(value: nil, timeoutRestored: true) }))
    }
    let scrollbarTimeout = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
    guard scrollbarTimeout > 0 else { return .timeoutUnavailable }
    typealias ScrollbarMetadata = (
        value: Double?, minValue: Double?, maxValue: Double?, actions: [String])
    let scrollbarMetadata: ScrollbarMetadata
    switch withTargetedAXMessagingTimeoutV1(
        element: scrollbar, timeout: scrollbarTimeout,
        operation: { () -> ScrollbarMetadata in
            (
                semanticScrollNumberV1(scrollbar, kAXValueAttribute as String),
                semanticScrollNumberV1(scrollbar, kAXMinValueAttribute as String),
                semanticScrollNumberV1(scrollbar, kAXMaxValueAttribute as String),
                axActions(scrollbar))
        }) {
    case let .completed(value): scrollbarMetadata = value
    case .unavailable: return .timeoutUnavailable
    }
    let action = direction == "increment" ?
        kAXIncrementAction as String : kAXDecrementAction as String
    return .target(.init(
        role: role, fingerprint: fingerprint, enabled: targetMetadata.enabled,
        sensitive: targetMetadata.sensitive,
        metricSupported: scrollbarMetadata.value != nil,
        actionSupported: scrollbarMetadata.actions.contains(action),
        minValue: scrollbarMetadata.minValue, maxValue: scrollbarMetadata.maxValue,
        performAction: { action, callTimeout in
            performSemanticScrollAXCallV1(
                scrollbar: scrollbar, action: action, timeout: callTimeout)
        },
        observeValue: { callTimeout in
            observeSemanticScrollValueV1(scrollbar: scrollbar, timeout: callTimeout)
        }))
}

private func countSemanticScrollFingerprintV1(
    in window: AXUIElement, fingerprint: String, timeout: TimeInterval
) -> SemanticScrollFingerprintCountV1 {
    guard timeout > 0, timeout.isFinite else { return .timeoutUnavailable }
    let startedAt = Date()
    var count = 0
    var unavailable = false
    func walk(_ element: AXUIElement) {
        guard !unavailable, count < 2 else { return }
        let remaining = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
        guard remaining > 0 else { unavailable = true; return }
        let observation: (String?, [AXUIElement])
        switch withTargetedAXMessagingTimeoutV1(
            element: element, timeout: remaining,
            operation: { (semanticPressFingerprintV2(element), axChildren(element) ?? []) }) {
        case let .completed(value): observation = value
        case .unavailable: unavailable = true; return
        }
        if observation.0 == fingerprint { count += 1 }
        for child in observation.1 { walk(child) }
    }
    let rootRemaining = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
    guard rootRemaining > 0 else { return .timeoutUnavailable }
    let rootChildren: [AXUIElement]
    switch withTargetedAXMessagingTimeoutV1(
        element: window, timeout: rootRemaining,
        operation: { axChildren(window) ?? [] }) {
    case let .completed(value): rootChildren = value
    case .unavailable: return .timeoutUnavailable
    }
    for child in rootChildren { walk(child) }
    return unavailable ? .timeoutUnavailable : .count(count)
}

private func semanticScrollCancellationAuthorityV1(
    requestID: Int64, request: SemanticScrollRequestV1
) -> Data {
    let fields = [
        "semantic-scroll-v1", String(requestID), String(request.pid), request.bundleID,
        String(request.windowID), request.ref, request.path, request.expectedRole,
        request.expectedFingerprint, request.axis, request.direction, String(request.steps),
        request.commitDeadlineAt,
    ]
    return Data(fields.map { "\($0.utf8.count):\($0)" }.joined().utf8)
}

func semanticScrollCancellationMarkerURLV1(
    requestID: Int64, request: SemanticScrollRequestV1
) -> URL {
    let digest = SHA256.hash(data: semanticScrollCancellationAuthorityV1(
        requestID: requestID, request: request))
        .map { String(format: "%02x", $0) }.joined()
    return URL(fileURLWithPath: "/tmp", isDirectory: true).appendingPathComponent(
        "kocoro-ax-scroll-cancel-v1-\(digest)", isDirectory: false)
}

func productionSemanticScrollDependenciesV1(
    requestID: Int64, request: SemanticScrollRequestV1
) -> SemanticScrollDependenciesV1 {
    let cancellationURL = semanticScrollCancellationMarkerURLV1(
        requestID: requestID, request: request)
    return SemanticScrollDependenciesV1(
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
    resolveWindow: { pid, windowID, timeout in
        switch resolveSemanticScrollWindowV1(
            pid: pid, windowID: windowID, timeout: timeout) {
        case .unique: return .unique
        case .missing: return .missing
        case .ambiguous: return .ambiguous
        case .timeoutUnavailable: return .timeoutUnavailable
        }
    },
    resolveTarget: { pid, windowID, path, axis, direction, timeout in
        let startedAt = Date()
        let window: AXUIElement
        switch resolveSemanticScrollWindowV1(
            pid: pid, windowID: windowID, timeout: timeout) {
        case let .unique(value): window = value
        case .timeoutUnavailable: return .timeoutUnavailable
        case .missing, .ambiguous: return .missing
        }
        let remaining = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
        guard remaining > 0 else { return .timeoutUnavailable }
        return semanticScrollTargetV1(
            in: window, path: path, axis: axis,
            direction: direction, timeout: remaining)
    },
    countFingerprint: { pid, windowID, fingerprint, timeout in
        let startedAt = Date()
        let window: AXUIElement
        switch resolveSemanticScrollWindowV1(
            pid: pid, windowID: windowID, timeout: timeout) {
        case let .unique(value): window = value
        case .timeoutUnavailable: return .timeoutUnavailable
        case .missing, .ambiguous: return .count(0)
        }
        let remaining = semanticScrollRemainingV1(startedAt: startedAt, budget: timeout)
        guard remaining > 0 else { return .timeoutUnavailable }
        return countSemanticScrollFingerprintV1(
            in: window, fingerprint: fingerprint, timeout: remaining)
    },
    observePhysicalInput: observePhysicalInputInterferenceV1,
    isCancelled: { FileManager.default.fileExists(atPath: cancellationURL.path) },
    now: Date.init,
    sleep: Thread.sleep(forTimeInterval:))
}
