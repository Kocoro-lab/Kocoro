import AppKit
import CoreGraphics
import CryptoKit
import Foundation

struct TargetBoundInputRequestV1: Equatable, Decodable {
    let schemaVersion: Int
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let expectedWindowAXBounds: DisplayTopologyRectV1
    let action: String
    let ref: String?
    let path: String?
    let expectedRole: String?
    let expectedFingerprint: String?
    let text: String?
    let key: String?
    let keys: [String]?
    let modifiers: [String]?
    let commitDeadlineAt: String

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container,
            exactly: [
                "schema_version", "pid", "bundle_id", "window_id",
                "expected_window_ax_bounds", "action", "ref", "path",
                "expected_role", "expected_fingerprint", "text", "key",
                "keys", "modifiers", "commit_deadline_at",
            ],
            field: "target_bound_input.params")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        pid = try container.decode(Int.self, forKey: strictMutationKey("pid"))
        bundleID = try container.decode(String.self, forKey: strictMutationKey("bundle_id"))
        windowID = try container.decode(UInt32.self, forKey: strictMutationKey("window_id"))
        expectedWindowAXBounds = try container.decode(
            DisplayTopologyRectV1.self,
            forKey: strictMutationKey("expected_window_ax_bounds"))
        action = try container.decode(String.self, forKey: strictMutationKey("action"))
        ref = try container.decodeIfPresent(String.self, forKey: strictMutationKey("ref"))
        path = try container.decodeIfPresent(String.self, forKey: strictMutationKey("path"))
        expectedRole = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("expected_role"))
        expectedFingerprint = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("expected_fingerprint"))
        text = try container.decodeIfPresent(String.self, forKey: strictMutationKey("text"))
        key = try container.decodeIfPresent(String.self, forKey: strictMutationKey("key"))
        keys = try container.decodeIfPresent(
            [String].self, forKey: strictMutationKey("keys"))
        modifiers = try container.decodeIfPresent(
            [String].self, forKey: strictMutationKey("modifiers"))
        commitDeadlineAt = try container.decode(
            String.self, forKey: strictMutationKey("commit_deadline_at"))

        guard schemaVersion == 1, pid > 0, windowID > 0,
              strictMutationIdentity(bundleID),
              expectedWindowAXBounds.width > 0,
              expectedWindowAXBounds.height > 0,
              expectedWindowAXBounds.x.isFinite,
              expectedWindowAXBounds.y.isFinite,
              expectedWindowAXBounds.width.isFinite,
              expectedWindowAXBounds.height.isFinite,
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid("invalid target_bound_input authority")
        }
        switch action {
        case "type":
            let elementBound =
                ref.map {
                    strictMutationIdentity($0) && $0.count > 1 &&
                        $0.first == "e" && $0.dropFirst().allSatisfy(\.isNumber)
                } == true &&
                path.map {
                    strictMutationIdentity($0) &&
                        ($0 == "window[0]" || $0.hasPrefix("window[0]/"))
                } == true &&
                expectedRole.map(strictMutationIdentity) == true &&
                expectedFingerprint.map(strictMutationIdentity) == true
            let windowBound =
                ref == nil && path == nil && expectedRole == nil &&
                expectedFingerprint == nil
            guard elementBound || windowBound,
                  let text, !text.isEmpty, key == nil, keys == nil,
                  modifiers == nil else {
                throw StrictMutationWireError.invalid("invalid target_bound_input type tuple")
            }
        case "hotkey":
            guard ref == nil, path == nil, expectedRole == nil,
                  expectedFingerprint == nil, text == nil,
                  let key, strictMutationIdentity(key),
                  keys == nil, let modifiers else {
                throw StrictMutationWireError.invalid("invalid target_bound_input hotkey tuple")
            }
            guard strictInputKeyV1(key), strictInputModifiersV1(modifiers) else {
                throw StrictMutationWireError.invalid("invalid target_bound_input modifiers")
            }
        case "keypress":
            guard ref == nil, path == nil, expectedRole == nil,
                  expectedFingerprint == nil, text == nil, key == nil,
                  let keys, (1...64).contains(keys.count),
                  keys.allSatisfy(strictInputKeyV1),
                  let modifiers, strictInputModifiersV1(modifiers) else {
                throw StrictMutationWireError.invalid("invalid target_bound_input keypress tuple")
            }
        default:
            throw StrictMutationWireError.invalid("unsupported target_bound_input action")
        }
    }
}

struct TargetBoundInputRPCRequestV1: Equatable, Decodable {
    let id: Int64
    let method: String
    let params: TargetBoundInputRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"],
            field: "target_bound_input")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(
            TargetBoundInputRequestV1.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "target_bound_input" else {
            throw StrictMutationWireError.invalid("invalid target_bound_input envelope")
        }
    }
}

func decodeTargetBoundInputRPCRequestV1(_ payload: Data) throws -> TargetBoundInputRPCRequestV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(TargetBoundInputRPCRequestV1.self, from: payload)
}

struct TargetBoundInputResultV1: Encodable, Equatable {
    let schemaVersion = 1
    let status: String
    let action: String
    let inputCommitted: Bool
    let clipboardTouched: Bool
    let clipboardRestored: Bool
    let phase: String
    let failureCode: String?
    let retrySafe = false
    let postcondition: String?

    init(
        status: String,
        action: String,
        inputCommitted: Bool,
        clipboardTouched: Bool,
        clipboardRestored: Bool,
        phase: String,
        failureCode: String?,
        postcondition: String? = nil
    ) {
        self.status = status
        self.action = action
        self.inputCommitted = inputCommitted
        self.clipboardTouched = clipboardTouched
        self.clipboardRestored = clipboardRestored
        self.phase = phase
        self.failureCode = failureCode
        self.postcondition = postcondition
    }

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case status, action
        case inputCommitted = "input_committed"
        case clipboardTouched = "clipboard_touched"
        case clipboardRestored = "clipboard_restored"
        case phase
        case failureCode = "failure_code"
        case retrySafe = "retry_safe"
        case postcondition
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(schemaVersion, forKey: .schemaVersion)
        try container.encode(status, forKey: .status)
        try container.encode(action, forKey: .action)
        try container.encode(inputCommitted, forKey: .inputCommitted)
        try container.encode(clipboardTouched, forKey: .clipboardTouched)
        try container.encode(clipboardRestored, forKey: .clipboardRestored)
        try container.encode(phase, forKey: .phase)
        try container.encode(failureCode, forKey: .failureCode)
        try container.encode(retrySafe, forKey: .retrySafe)
        try container.encode(postcondition, forKey: .postcondition)
    }
}

struct TargetBoundPreparedInput {
    let post: () -> Bool

    init(_ post: @escaping () -> Bool) { self.post = post }
}

enum TargetBoundKeySequencePostOutcomeV1: Equatable {
    case completed(keyPairsCommitted: Int)
    case cancelled(
        keyPairsCommitted: Int, inputCommitted: Bool, cleanupComplete: Bool)
    case failed(
        keyPairsCommitted: Int, inputCommitted: Bool, cleanupComplete: Bool)
}

struct TargetBoundPreparedKeySequenceV1 {
    let post: () -> TargetBoundKeySequencePostOutcomeV1
}

private enum TargetBoundOrdinaryKeySequenceOutcomeV1: Equatable {
    case completed(Int)
    case cancelled(Int)
    case failed(Int)
}

func makeTargetBoundPreparedKeySequenceV1(
    keys: [String],
    modifiers: [String],
    prepareModifier: @escaping (String) -> StrictPreparedModifierV1?,
    prepareKey: @escaping (String, [String]) -> TargetBoundPreparedInput?,
    isCancelled: @escaping () -> Bool
) -> TargetBoundPreparedKeySequenceV1? {
    guard (1...64).contains(keys.count),
          keys.allSatisfy(strictInputKeyV1),
          strictInputModifiersV1(modifiers) else { return nil }
    let preparedKeys = keys.map { prepareKey($0, modifiers) }
    guard preparedKeys.allSatisfy({ $0 != nil }) else { return nil }

    return TargetBoundPreparedKeySequenceV1(post: {
        let result = runStrictModifierSequenceV1(
            modifiers: modifiers,
            prepare: prepareModifier,
            isCancelled: isCancelled,
            action: {
                var committed = 0
                for prepared in preparedKeys {
                    if isCancelled() {
                        return TargetBoundOrdinaryKeySequenceOutcomeV1.cancelled(committed)
                    }
                    guard prepared!.post() else {
                        return TargetBoundOrdinaryKeySequenceOutcomeV1.failed(committed)
                    }
                    committed += 1
                }
                return .completed(committed)
            })
        switch result {
        case let .completed(.completed(committed)):
            return .completed(keyPairsCommitted: committed)
        case let .completed(.cancelled(committed)):
            return .cancelled(
                keyPairsCommitted: committed,
                inputCommitted: committed > 0 || !modifiers.isEmpty,
                cleanupComplete: true)
        case let .completed(.failed(committed)):
            return .failed(
                keyPairsCommitted: committed,
                inputCommitted: committed > 0 || !modifiers.isEmpty,
                cleanupComplete: true)
        case let .releaseFailed(inner):
            let committed: Int
            switch inner {
            case let .completed(value), let .cancelled(value), let .failed(value):
                committed = value
            }
            return .failed(
                keyPairsCommitted: committed,
                inputCommitted: committed > 0 || !modifiers.isEmpty,
                cleanupComplete: false)
        case let .cancelled(cleanupComplete):
            return .cancelled(
                keyPairsCommitted: 0,
                inputCommitted: !modifiers.isEmpty,
                cleanupComplete: cleanupComplete)
        case let .preparationFailed(cleanupComplete),
             let .pressFailed(cleanupComplete):
            return .failed(
                keyPairsCommitted: 0,
                inputCommitted: !modifiers.isEmpty,
                cleanupComplete: cleanupComplete)
        }
    })
}

enum TargetBoundClipboardPostOutcome: Equatable {
    case committed
    case ownershipLost
    case failed
}

struct TargetBoundClipboardTransaction {
    let post: () -> TargetBoundClipboardPostOutcome
    let restore: () -> Bool
}

enum TargetBoundTypeVerificationObservationV1 {
    case matched
    case mismatch
    case unavailable
    case targetChanged
}

struct TargetBoundTypePostconditionVerifierV1 {
    let observe: (TimeInterval) -> TargetBoundTypeVerificationObservationV1
}

enum TargetBoundTypeVerificationPreparationV1 {
    case ready(TargetBoundTypePostconditionVerifierV1)
    case unavailable(failureCode: String)
    case failed(failureCode: String)
}

struct TargetBoundInputDependencies {
    let canAdmitInput: () -> Bool
    let authorityFailure: (TargetBoundInputRequestV1) -> String?
    let restoreAuthority: (TargetBoundInputRequestV1, String) -> Bool
    let requiresClipboard: (String) -> Bool
    let prepareHotkey: (String, [String]) -> TargetBoundPreparedInput?
    let prepareKeypress:
        ([String], [String]) -> TargetBoundPreparedKeySequenceV1?
    let prepareDirectText: (String) -> TargetBoundPreparedInput?
    let prepareClipboardText: (String) -> TargetBoundClipboardPreparationV1
    let prepareTypeVerification:
        (TargetBoundInputRequestV1, String) -> TargetBoundTypeVerificationPreparationV1
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let now: () -> Date
    let sleep: (TimeInterval) -> Void
}

private let targetBoundInputMaximumDeadlineHorizonV1 = 2.0
private let targetBoundInputCounterSettleDelayV1 = 0.005
/// Upper bound for one authority revalidation pass. It must stay well inside
/// targetBoundInputMaximumDeadlineHorizonV1 so a hung target cannot hold the
/// single-threaded socket loop past the commit horizon.
private let targetBoundInputAuthorityTimeoutV1 = 0.75
private let targetBoundInputMaximumCounterSettleAttemptsV1 = 10

func targetBoundInputAssessPhysicalInputV1(
    baseline: PhysicalInputInterferenceSnapshotV1?,
    expectedPointer: CoordinateMouseEventPointV1?,
    expectedSyntheticEvents: [(CGEventType, UInt32)]?,
    observe: () -> PhysicalInputInterferenceSnapshotV1?,
    settle: () -> Void,
    maximumSettleAttempts: Int =
        targetBoundInputMaximumCounterSettleAttemptsV1
) -> (
    assessment: PhysicalInputInterferenceAssessmentV1,
    snapshot: PhysicalInputInterferenceSnapshotV1?
) {
    var settleAttempt = 0
    while true {
        let current = observe()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: baseline,
            current: current,
            expectedPointer: expectedPointer,
            expectedSyntheticEvents: expectedSyntheticEvents
        )
        if assessment != .unavailable ||
            expectedSyntheticEvents == nil ||
            expectedSyntheticEvents?.isEmpty == true ||
            settleAttempt >= max(0, maximumSettleAttempts) {
            return (assessment, current)
        }
        settleAttempt += 1
        settle()
    }
}

private func targetBoundInputFailure(
    _ request: TargetBoundInputRequestV1,
    code: String,
    phase: String = "preflight",
    clipboardTouched: Bool = false,
    clipboardRestored: Bool = false
) -> TargetBoundInputResultV1 {
    TargetBoundInputResultV1(
        status: "failed", action: request.action, inputCommitted: false,
        clipboardTouched: clipboardTouched, clipboardRestored: clipboardRestored,
        phase: phase, failureCode: code)
}

private func targetBoundInputUserInterference(
    _ request: TargetBoundInputRequestV1,
    inputCommitted: Bool,
    clipboardTouched: Bool = false,
    clipboardRestored: Bool = false
) -> TargetBoundInputResultV1 {
    TargetBoundInputResultV1(
        status: "user_interference", action: request.action,
        inputCommitted: inputCommitted,
        clipboardTouched: clipboardTouched,
        clipboardRestored: clipboardRestored,
        phase: "user_interference",
        failureCode: "physical_input_interference")
}

private func targetBoundInputMonitoringLostAfterCommit(
    _ request: TargetBoundInputRequestV1,
    clipboardTouched: Bool,
    clipboardRestored: Bool
) -> TargetBoundInputResultV1 {
    TargetBoundInputResultV1(
        status: "completed_unverified", action: request.action,
        inputCommitted: true,
        clipboardTouched: clipboardTouched,
        clipboardRestored: clipboardRestored,
        phase: "post_verification",
        failureCode: "interference_detection_unavailable")
}

private func targetBoundTypePostVerification(
    request: TargetBoundInputRequestV1,
    preparation: TargetBoundTypeVerificationPreparationV1,
    clipboardTouched: Bool,
    restoreClipboard: @escaping () -> Bool,
    physicalInputAfterPost: PhysicalInputInterferenceSnapshotV1?,
    dependencies: TargetBoundInputDependencies
) -> TargetBoundInputResultV1 {
    // Restoring the pasteboard is deliberately LAZY and memoized. Restoring
    // eagerly before the verifier observes the target lets a busy or
    // beachballing app service our synthetic Cmd+V after the restore, so it
    // pastes the user's PREVIOUS clipboard content into the target field. Each
    // return path triggers the restore at the latest safe moment instead, and
    // the verifier below is the only evidence the paste actually landed.
    var restoreResult: Bool? = nil
    let clipboardRestoredNow: () -> Bool = {
        if let restored = restoreResult { return restored }
        let restored = restoreClipboard()
        restoreResult = restored
        return restored
    }
    // Applied only to results produced after the preparation switch, matching
    // the previous placement of the restore-failure check.
    let withRestoreFailure: (TargetBoundInputResultV1) -> TargetBoundInputResultV1 = { result in
        guard clipboardTouched, !clipboardRestoredNow() else { return result }
        return TargetBoundInputResultV1(
            status: "completed_unverified", action: request.action,
            inputCommitted: true, clipboardTouched: true, clipboardRestored: false,
            phase: "post_verification",
            failureCode: "clipboard_restore_failed_after_commit")
    }
    var physicalInputBaseline = physicalInputAfterPost
    var physicalAssessment: PhysicalInputInterferenceAssessmentV1 = .unchanged
    let assessCurrentPhysicalInput: () -> PhysicalInputInterferenceAssessmentV1 = {
        let current = dependencies.observePhysicalInput()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: physicalInputBaseline,
            current: current,
            expectedPointer: physicalInputAfterPost?.pointer)
        if assessment == .unchanged { physicalInputBaseline = current }
        return assessment
    }
    physicalAssessment = assessCurrentPhysicalInput()
    if physicalAssessment == .interference {
        return targetBoundInputUserInterference(
            request, inputCommitted: true,
            clipboardTouched: clipboardTouched,
            clipboardRestored: clipboardRestoredNow())
    }
    if physicalAssessment == .unavailable {
        return targetBoundInputMonitoringLostAfterCommit(
            request, clipboardTouched: clipboardTouched,
            clipboardRestored: clipboardRestoredNow())
    }
    switch preparation {
    case let .unavailable(failureCode):
        return withRestoreFailure(TargetBoundInputResultV1(
            status: "completed_unverified", action: request.action,
            inputCommitted: true, clipboardTouched: clipboardTouched,
            clipboardRestored: clipboardRestoredNow(), phase: "post_verification",
            failureCode: failureCode))
    case let .failed(failureCode):
        // A failed preparation is consumed before input commit. This branch is
        // defensive: runTargetBoundInput handles it before posting the event.
        return targetBoundInputFailure(
            request, code: failureCode, phase: "action",
            clipboardTouched: clipboardTouched, clipboardRestored: clipboardRestoredNow())
    case let .ready(verifier):
        let outcome: TargetedAXPostconditionOutcomeV1<Bool> =
            runTargetedAXPostconditionVerificationV1(
                now: dependencies.now,
                sleep: dependencies.sleep,
                observeWithTimeout: { timeout in
                    physicalAssessment = assessCurrentPhysicalInput()
                    if physicalAssessment == .interference {
                        return .terminal(
                            failureCode: "physical_input_interference", observation: nil)
                    }
                    if physicalAssessment == .unavailable {
                        return .terminal(
                            failureCode: "interference_detection_unavailable", observation: nil)
                    }
                    switch verifier.observe(timeout) {
                    case .matched:
                        return .matched(true)
                    case .mismatch:
                        return .retryable(
                            failureCode: "target_value_mismatch", observation: nil)
                    case .unavailable:
                        return .retryable(
                            failureCode: "target_value_readback_unavailable", observation: nil)
                    case .targetChanged:
                        return .terminal(
                            failureCode: "target_changed_during_verification", observation: nil)
                    }
                })
        physicalAssessment = assessCurrentPhysicalInput()
        if physicalAssessment == .interference {
            return targetBoundInputUserInterference(
                request, inputCommitted: true,
                clipboardTouched: clipboardTouched,
                clipboardRestored: clipboardRestoredNow())
        }
        if physicalAssessment == .unavailable {
            return targetBoundInputMonitoringLostAfterCommit(
                request, clipboardTouched: clipboardTouched,
                clipboardRestored: clipboardRestoredNow())
        }
        switch outcome {
        case .verified:
            return withRestoreFailure(TargetBoundInputResultV1(
                status: "verified", action: request.action,
                inputCommitted: true, clipboardTouched: clipboardTouched,
                clipboardRestored: clipboardRestoredNow(), phase: "post_verification",
                failureCode: nil,
                postcondition: "target_value_matches_expected_edit"))
        case let .inconclusive(failureCode, _, _):
            return withRestoreFailure(TargetBoundInputResultV1(
                status: "completed_unverified", action: request.action,
                inputCommitted: true, clipboardTouched: clipboardTouched,
                clipboardRestored: clipboardRestoredNow(), phase: "post_verification",
                failureCode: failureCode))
        }
    }
}

func runTargetBoundInput(
    request: TargetBoundInputRequestV1,
    dependencies: TargetBoundInputDependencies
) -> TargetBoundInputResultV1 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return targetBoundInputFailure(request, code: "invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else {
        return targetBoundInputFailure(request, code: "request_expired")
    }
    guard horizon <= targetBoundInputMaximumDeadlineHorizonV1 else {
        return targetBoundInputFailure(request, code: "invalid_request")
    }
    guard dependencies.canAdmitInput() else {
        return targetBoundInputFailure(request, code: "input_recovery_blocked")
    }
    if let initialFailure = dependencies.authorityFailure(request) {
        let windowBoundType = request.action == "type" && request.ref == nil
        guard !windowBoundType,
              targetBoundInputRestorableFocusFailuresV1.contains(initialFailure),
              dependencies.restoreAuthority(request, initialFailure) else {
            return targetBoundInputFailure(request, code: initialFailure)
        }
        if let restoredFailure = dependencies.authorityFailure(request) {
            return targetBoundInputFailure(request, code: restoredFailure)
        }
    }
    guard let initialPhysicalInput = dependencies.observePhysicalInput() else {
        return targetBoundInputFailure(
            request, code: "interference_detection_unavailable")
    }
    if assessPhysicalInputInterferenceV1(
        baseline: initialPhysicalInput,
        current: initialPhysicalInput,
        expectedPointer: nil
    ) == .interference {
        return targetBoundInputUserInterference(request, inputCommitted: false)
    }

    if request.action == "keypress" {
        guard let prepared = dependencies.prepareKeypress(
            request.keys!, request.modifiers!) else {
            return targetBoundInputFailure(
                request, code: "event_preparation_failed", phase: "preparation")
        }
        guard dependencies.now() < deadline else {
            return targetBoundInputFailure(
                request, code: "request_expired_before_input", phase: "action")
        }
        if let failure = dependencies.authorityFailure(request) {
            return targetBoundInputFailure(request, code: failure, phase: "action")
        }
        let physicalInputBeforePost = dependencies.observePhysicalInput()
        switch assessPhysicalInputInterferenceV1(
            baseline: initialPhysicalInput,
            current: physicalInputBeforePost,
            expectedPointer: initialPhysicalInput.pointer
        ) {
        case .interference:
            return targetBoundInputUserInterference(request, inputCommitted: false)
        case .unavailable:
            return targetBoundInputFailure(
                request, code: "interference_detection_unavailable", phase: "action")
        case .unchanged:
            break
        }
        let outcome = prepared.post()
        switch outcome {
        case let .cancelled(_, inputCommitted, cleanupComplete):
            let code = cleanupComplete
                ? (inputCommitted ? "cancelled_after_partial_input" : "cancelled_before_input")
                : "modifier_release_unconfirmed"
            if inputCommitted {
                return TargetBoundInputResultV1(
                    status: "completed_unverified", action: request.action,
                    inputCommitted: true, clipboardTouched: false,
                    clipboardRestored: false, phase: "action", failureCode: code)
            }
            return targetBoundInputFailure(request, code: code, phase: "action")
        case let .failed(_, inputCommitted, cleanupComplete):
            let code = cleanupComplete ? "event_post_failed" : "modifier_release_unconfirmed"
            if inputCommitted {
                return TargetBoundInputResultV1(
                    status: "completed_unverified", action: request.action,
                    inputCommitted: true, clipboardTouched: false,
                    clipboardRestored: false, phase: "action", failureCode: code)
            }
            return targetBoundInputFailure(request, code: code, phase: "action")
        case let .completed(keyPairsCommitted):
            let afterPostAssessment = targetBoundInputAssessPhysicalInputV1(
                baseline: physicalInputBeforePost,
                expectedPointer: initialPhysicalInput.pointer,
                expectedSyntheticEvents: [
                    (.keyDown, UInt32(keyPairsCommitted + request.modifiers!.count)),
                    (.keyUp, UInt32(keyPairsCommitted + request.modifiers!.count)),
                ],
                observe: dependencies.observePhysicalInput,
                settle: {
                    dependencies.sleep(targetBoundInputCounterSettleDelayV1)
                }
            )
            switch afterPostAssessment.assessment {
            case .interference:
                return targetBoundInputUserInterference(request, inputCommitted: true)
            case .unavailable:
                return targetBoundInputMonitoringLostAfterCommit(
                    request, clipboardTouched: false, clipboardRestored: false)
            case .unchanged:
                return TargetBoundInputResultV1(
                    status: "completed_unverified", action: request.action,
                    inputCommitted: true, clipboardTouched: false,
                    clipboardRestored: false, phase: "post_verification",
                    failureCode: "postcondition_not_declared")
            }
        }
    }

    if request.action == "hotkey" {
        guard let prepared = dependencies.prepareHotkey(request.key!, request.modifiers!) else {
            return targetBoundInputFailure(
                request, code: "event_preparation_failed", phase: "preparation")
        }
        guard dependencies.now() < deadline else {
            return targetBoundInputFailure(request, code: "request_expired_before_input", phase: "action")
        }
        if let failure = dependencies.authorityFailure(request) {
            return targetBoundInputFailure(request, code: failure, phase: "action")
        }
        let physicalInputBeforePost = dependencies.observePhysicalInput()
        switch assessPhysicalInputInterferenceV1(
            baseline: initialPhysicalInput,
            current: physicalInputBeforePost,
            expectedPointer: initialPhysicalInput.pointer
        ) {
        case .interference:
            return targetBoundInputUserInterference(request, inputCommitted: false)
        case .unavailable:
            return targetBoundInputFailure(
                request, code: "interference_detection_unavailable", phase: "action")
        case .unchanged:
            break
        }
        let posted = prepared.post()
        let afterPostAssessment = targetBoundInputAssessPhysicalInputV1(
            baseline: physicalInputBeforePost,
            expectedPointer: initialPhysicalInput.pointer,
            expectedSyntheticEvents:
                posted ? [(.keyDown, 1), (.keyUp, 1)] : nil,
            observe: dependencies.observePhysicalInput,
            settle: {
                dependencies.sleep(targetBoundInputCounterSettleDelayV1)
            }
        )
        switch afterPostAssessment.assessment {
        case .interference:
            return targetBoundInputUserInterference(
                request, inputCommitted: posted)
        case .unavailable where posted:
            return targetBoundInputMonitoringLostAfterCommit(
                request, clipboardTouched: false, clipboardRestored: false)
        case .unavailable, .unchanged:
            break
        }
        guard posted else {
            return targetBoundInputFailure(request, code: "event_post_failed", phase: "action")
        }
        return TargetBoundInputResultV1(
            status: "completed_unverified", action: request.action,
            inputCommitted: true, clipboardTouched: false, clipboardRestored: false,
            phase: "post_verification", failureCode: "postcondition_not_declared")
    }

    let text = request.text!
    if dependencies.requiresClipboard(text) {
        // The first authority check above occurs before touching the clipboard.
        let transaction: TargetBoundClipboardTransaction
        switch dependencies.prepareClipboardText(text) {
        case let .prepared(ready):
            transaction = ready
        case let .failedAfterTouch(restored):
            // The pasteboard was already cleared, so report it truthfully rather
            // than claiming it was never touched.
            return targetBoundInputFailure(
                request,
                code: restored
                    ? "clipboard_preparation_failed"
                    : "clipboard_restore_failed_before_input",
                phase: "preparation",
                clipboardTouched: true, clipboardRestored: restored)
        }
        let restoreWithoutInput: (String) -> TargetBoundInputResultV1 = { code in
            let restored = transaction.restore()
            return targetBoundInputFailure(
                request,
                code: restored ? code : "clipboard_restore_failed_before_input",
                phase: "action", clipboardTouched: true, clipboardRestored: restored)
        }
        guard dependencies.now() < deadline else {
            return restoreWithoutInput("request_expired_before_input")
        }
        if let failure = dependencies.authorityFailure(request) {
            return restoreWithoutInput(failure)
        }
        let verification = dependencies.prepareTypeVerification(request, text)
        if case let .failed(failureCode) = verification {
            return restoreWithoutInput(failureCode)
        }
        let physicalInputBeforePost = dependencies.observePhysicalInput()
        switch assessPhysicalInputInterferenceV1(
            baseline: initialPhysicalInput,
            current: physicalInputBeforePost,
            expectedPointer: initialPhysicalInput.pointer
        ) {
        case .interference:
            let restored = transaction.restore()
            return targetBoundInputUserInterference(
                request, inputCommitted: false,
                clipboardTouched: true, clipboardRestored: restored)
        case .unavailable:
            return restoreWithoutInput("interference_detection_unavailable")
        case .unchanged:
            break
        }
        let postOutcome = transaction.post()
        let afterPostAssessment = targetBoundInputAssessPhysicalInputV1(
            baseline: physicalInputBeforePost,
            expectedPointer: initialPhysicalInput.pointer,
            expectedSyntheticEvents: postOutcome == .committed
                ? [(.keyDown, 1), (.keyUp, 1)]
                : (postOutcome == .ownershipLost ? [] : nil),
            observe: dependencies.observePhysicalInput,
            settle: {
                dependencies.sleep(targetBoundInputCounterSettleDelayV1)
            }
        )
        let physicalInputAfterPost = afterPostAssessment.snapshot
        let physicalAssessment = afterPostAssessment.assessment
        if physicalAssessment == .interference {
            switch postOutcome {
            case .ownershipLost:
                return targetBoundInputUserInterference(
                    request, inputCommitted: false,
                    clipboardTouched: true, clipboardRestored: false)
            case .committed, .failed:
                let restored = transaction.restore()
                return targetBoundInputUserInterference(
                    request, inputCommitted: postOutcome == .committed,
                    clipboardTouched: true, clipboardRestored: restored)
            }
        }
        switch postOutcome {
        case .committed:
            break
        case .ownershipLost:
            return targetBoundInputFailure(
                request, code: "clipboard_ownership_lost_before_input",
                phase: "action", clipboardTouched: true, clipboardRestored: false)
        case .failed:
            return restoreWithoutInput("event_post_failed")
        }
        if physicalAssessment == .unavailable {
            return targetBoundInputMonitoringLostAfterCommit(
                request, clipboardTouched: true, clipboardRestored: transaction.restore())
        }
        // Restore is handed over as a deferred closure so it runs only after the
        // verifier has read the target back — see targetBoundTypePostVerification.
        return targetBoundTypePostVerification(
            request: request, preparation: verification,
            clipboardTouched: true, restoreClipboard: transaction.restore,
            physicalInputAfterPost: physicalInputAfterPost,
            dependencies: dependencies)
    }

    guard let prepared = dependencies.prepareDirectText(text) else {
        return targetBoundInputFailure(
            request, code: "event_preparation_failed", phase: "preparation")
    }
    guard dependencies.now() < deadline else {
        return targetBoundInputFailure(request, code: "request_expired_before_input", phase: "action")
    }
    if let failure = dependencies.authorityFailure(request) {
        return targetBoundInputFailure(request, code: failure, phase: "action")
    }
    let verification = dependencies.prepareTypeVerification(request, text)
    if case let .failed(failureCode) = verification {
        return targetBoundInputFailure(request, code: failureCode, phase: "action")
    }
    let physicalInputBeforePost = dependencies.observePhysicalInput()
    switch assessPhysicalInputInterferenceV1(
        baseline: initialPhysicalInput,
        current: physicalInputBeforePost,
        expectedPointer: initialPhysicalInput.pointer
    ) {
    case .interference:
        return targetBoundInputUserInterference(request, inputCommitted: false)
    case .unavailable:
        return targetBoundInputFailure(
            request, code: "interference_detection_unavailable", phase: "action")
    case .unchanged:
        break
    }
    let posted = prepared.post()
    let afterPostAssessment = targetBoundInputAssessPhysicalInputV1(
        baseline: physicalInputBeforePost,
        expectedPointer: initialPhysicalInput.pointer,
        expectedSyntheticEvents:
            posted ? [(.keyDown, 1), (.keyUp, 1)] : nil,
        observe: dependencies.observePhysicalInput,
        settle: {
            dependencies.sleep(targetBoundInputCounterSettleDelayV1)
        }
    )
    let physicalInputAfterPost = afterPostAssessment.snapshot
    switch afterPostAssessment.assessment {
    case .interference:
        return targetBoundInputUserInterference(
            request, inputCommitted: posted)
    case .unavailable where posted:
        return targetBoundInputMonitoringLostAfterCommit(
            request, clipboardTouched: false, clipboardRestored: false)
    case .unavailable, .unchanged:
        break
    }
    guard posted else {
        return targetBoundInputFailure(request, code: "event_post_failed", phase: "action")
    }
    // Direct (non-clipboard) typing never touched the pasteboard, so there is
    // nothing to restore and the deferred closure is never consulted.
    return targetBoundTypePostVerification(
        request: request, preparation: verification,
        clipboardTouched: false, restoreClipboard: { false },
        physicalInputAfterPost: physicalInputAfterPost,
        dependencies: dependencies)
}

func targetBoundInputAXBoundsCorrelateWithCG(
    _ left: DisplayTopologyRectV1,
    _ right: DisplayTopologyRectV1
) -> Bool {
    // AX and Quartz logical bounds are admitted by the same conservative
    // calibration tolerance used to mint the unique CGWindow ID.
    abs(left.x - right.x) <= 2.0 &&
        abs(left.y - right.y) <= 2.0 &&
        abs(left.width - right.width) <= 2.0 &&
        abs(left.height - right.height) <= 2.0
}

private func targetBoundInputExactWindow(
    _ windowID: UInt32
) -> CaptureCoordinateWindowWindowSnapshot? {
    guard let raw = CGWindowListCopyWindowInfo(
        [.optionIncludingWindow], CGWindowID(windowID)) as? [[String: Any]] else {
        return nil
    }
    let exact = raw.filter {
        ($0[kCGWindowNumber as String] as? NSNumber)?.uint32Value == windowID
    }
    guard exact.count == 1, let info = exact.first,
          let ownerPID = info[kCGWindowOwnerPID as String] as? NSNumber,
          let layer = info[kCGWindowLayer as String] as? NSNumber,
          let onScreen = info[kCGWindowIsOnscreen as String] as? NSNumber,
          let rawBounds = info[kCGWindowBounds as String],
          CFGetTypeID(rawBounds as CFTypeRef) == CFDictionaryGetTypeID(),
          let bounds = CGRect(dictionaryRepresentation: rawBounds as! CFDictionary) else {
        return nil
    }
    return CaptureCoordinateWindowWindowSnapshot(
        windowID: windowID, ownerPID: ownerPID.intValue, layer: layer.intValue,
        isOnScreen: onScreen.boolValue,
        bounds: DisplayTopologyRectV1(
            x: Double(bounds.origin.x), y: Double(bounds.origin.y),
            width: Double(bounds.width), height: Double(bounds.height)))
}

private func targetBoundInputFrontmostNormalWindowID(pid: Int) -> UInt32? {
    guard let windows = CGWindowListCopyWindowInfo(
        [.optionOnScreenOnly, .excludeDesktopElements],
        kCGNullWindowID) as? [[String: Any]] else { return nil }
    for info in windows {
        guard let owner = info[kCGWindowOwnerPID as String] as? NSNumber,
              owner.intValue == pid,
              let layer = info[kCGWindowLayer as String] as? NSNumber,
              layer.intValue == 0,
              let onScreen = info[kCGWindowIsOnscreen as String] as? NSNumber,
              onScreen.boolValue,
              let alpha = info[kCGWindowAlpha as String] as? NSNumber,
              alpha.doubleValue > 0,
              let number = info[kCGWindowNumber as String] as? NSNumber,
              let windowID = UInt32(exactly: number.uint64Value), windowID > 0 else {
            continue
        }
        return windowID
    }
    return nil
}

private func targetBoundInputFocusedAXWindow(pid: Int, windowID: UInt32) -> AXUIElement? {
    guard let processID = pid_t(exactly: pid) else { return nil }
    let app = AXUIElementCreateApplication(processID)
    guard let rawWindow = axValue(app, "AXFocusedWindow"),
          CFGetTypeID(rawWindow) == AXUIElementGetTypeID() else { return nil }
    let window = rawWindow as! AXUIElement
    let title = axString(window, "AXTitle") ?? ""
    guard let rawFrame = elementFrame(window) else { return nil }
    let frame = AXFrame(
        x: rawFrame.x, y: rawFrame.y,
        width: rawFrame.width, height: rawFrame.height)
    guard let exact = uniqueWindowID(pid: pid, title: title, frame: frame),
          UInt32(exactly: exact) == windowID else { return nil }
    return window
}

private func targetBoundInputFingerprint(_ element: AXUIElement) -> String? {
    guard let role = axString(element, "AXRole") else { return nil }
    return stableElementFingerprint(AXElementSnapshotAttributes(
        role: role,
        subrole: axString(element, "AXSubrole"),
        identifier: axString(element, "AXIdentifier"),
        title: axString(element, "AXTitle"),
        description: axString(element, "AXDescription"),
        value: nil, valueRedacted: true,
        protectedContent: axBool(element, "AXProtectedContent") ?? false,
        enabled: axBool(element, "AXEnabled") ?? true,
        focused: axBool(element, "AXFocused") ?? false,
        selected: axBool(element, "AXSelected") ?? false,
        actions: axActions(element), frame: nil))
}

/// Bounds the authority revalidation pass. productionTargetBoundInputAuthorityFailure
/// issues an unbounded number of AX calls (focused window, bounded path walk,
/// fingerprint reads, focused element) and was the only commit-adjacent path
/// without a scoped messaging timeout. An unresponsive target would block the
/// single-threaded socket loop while Go's ack timeout fires and releases the GUI
/// barrier — and on the clipboard branch that is a window in which the user's
/// pasteboard already holds the agent's text.
private func productionTargetBoundInputAuthorityFailureBoundedV1(
    _ request: TargetBoundInputRequestV1
) -> String? {
    guard let processID = pid_t(exactly: request.pid) else { return "process_not_live" }
    switch withTargetedAXMessagingTimeoutV1(
        element: AXUIElementCreateApplication(processID),
        timeout: targetBoundInputAuthorityTimeoutV1,
        operation: { productionTargetBoundInputAuthorityFailure(request) }
    ) {
    case let .completed(failure):
        return failure
    case .unavailable:
        // Fail closed: an unbounded pass is never silently substituted.
        return "authority_revalidation_timeout"
    }
}

private func productionTargetBoundInputAuthorityFailure(
    _ request: TargetBoundInputRequestV1
) -> String? {
    refreshAppKitState()
    guard let processID = pid_t(exactly: request.pid),
          let app = NSRunningApplication(processIdentifier: processID),
          !app.isTerminated else { return "process_not_live" }
    guard app.bundleIdentifier == request.bundleID else {
        return "process_identity_mismatch"
    }
    guard NSWorkspace.shared.frontmostApplication?.processIdentifier == processID else {
        return "frontmost_process_mismatch"
    }
    guard let window = targetBoundInputExactWindow(request.windowID),
          window.ownerPID == request.pid else { return "window_identity_mismatch" }
    guard window.layer == 0, window.isOnScreen else { return "window_not_actionable" }
    guard targetBoundInputAXBoundsCorrelateWithCG(
        request.expectedWindowAXBounds, window.bounds) else {
        return "window_bounds_mismatch"
    }
    guard let focusedWindow = targetBoundInputFocusedAXWindow(
        pid: request.pid, windowID: request.windowID) else {
        return "focused_window_mismatch"
    }
    guard targetBoundInputFrontmostNormalWindowID(pid: request.pid) == request.windowID else {
        return "frontmost_window_mismatch"
    }
    if request.action == "type", request.ref != nil {
        guard let path = request.path,
              let target = resolveElement(in: focusedWindow, path: path) else {
            return "path_not_found"
        }
        guard axString(target, "AXRole") == request.expectedRole else {
            return "role_mismatch"
        }
        guard targetBoundInputFingerprint(target) == request.expectedFingerprint else {
            return "fingerprint_mismatch"
        }
        let appElement = AXUIElementCreateApplication(processID)
        guard axElementsEqual(target, axFocusedElement(appElement)) else {
            return "focused_element_mismatch"
        }
    }
    return nil
}

private let targetBoundInputRestorableFocusFailuresV1: Set<String> = [
    "frontmost_process_mismatch",
    "focused_window_mismatch",
    "frontmost_window_mismatch",
]

private func productionRestoreTargetBoundInputAuthority(
    _ request: TargetBoundInputRequestV1,
    _ failure: String
) -> Bool {
    guard targetBoundInputRestorableFocusFailuresV1.contains(failure) else {
        return false
    }
    refreshAppKitState()
    guard let processID = pid_t(exactly: request.pid),
          let app = NSRunningApplication(processIdentifier: processID),
          !app.isTerminated,
          app.bundleIdentifier == request.bundleID,
          let window = targetBoundInputExactWindow(request.windowID),
          window.ownerPID == request.pid,
          window.layer == 0,
          window.isOnScreen,
          targetBoundInputAXBoundsCorrelateWithCG(
              request.expectedWindowAXBounds, window.bounds) else {
        return false
    }

    app.unhide()
    _ = app.activate(options: [.activateAllWindows])
    let deadline = Date().addingTimeInterval(0.4)
    while Date() < deadline {
        refreshAppKitState()
        if productionTargetBoundInputAuthorityFailure(request) == nil {
            return true
        }
        Thread.sleep(forTimeInterval: 0.02)
    }
    return false
}

private func targetBoundInputModifierFlags(_ modifiers: [String]) -> CGEventFlags? {
    var flags: CGEventFlags = []
    for modifier in modifiers {
        switch modifier.lowercased() {
        case "command", "cmd": flags.insert(.maskCommand)
        case "shift": flags.insert(.maskShift)
        case "option", "alt": flags.insert(.maskAlternate)
        case "control", "ctrl": flags.insert(.maskControl)
        default: return nil
        }
    }
    return flags
}

private func productionTargetBoundPreparedKey(
    key: String,
    modifiers: [String]
) -> TargetBoundPreparedInput? {
    guard let code = keyCodeMap[key.lowercased()],
          let flags = targetBoundInputModifierFlags(modifiers),
          let syntheticSource = physicalInputSyntheticEventSourceV1,
          let down = CGEvent(
              keyboardEventSource: syntheticSource, virtualKey: code, keyDown: true),
          let release = productionInputRelease(metadata: .key(
            // The shortcut is recognized on keyDown. Clear flags on keyUp so
            // the persistent private source does not retain a synthetic
            // command/shift mask into the next action's user-presence sample.
            virtualKey: UInt16(code), eventFlags: 0),
            eventSource: syntheticSource) else {
        return nil
    }
    down.flags = flags
    return TargetBoundPreparedInput {
        guard let token = processInputCommitGateV1.registerPress(
            release: release,
            commitDown: { down.post(tap: .cghidEventTap); return true }) else { return false }
        return processInputCommitGateV1.confirmRelease(token: token)
    }
}

private func productionTargetBoundPreparedKeySequence(
    keys: [String],
    modifiers: [String],
    isCancelled: @escaping () -> Bool
) -> TargetBoundPreparedKeySequenceV1? {
    let prepareModifier = productionStrictModifierPrepareV1()
    return makeTargetBoundPreparedKeySequenceV1(
        keys: keys,
        modifiers: modifiers,
        prepareModifier: prepareModifier,
        prepareKey: productionTargetBoundPreparedKey,
        isCancelled: isCancelled)
}

private func productionTargetBoundDirectText(_ text: String) -> TargetBoundPreparedInput? {
    // Production intentionally sends every string through one clipboard paste
    // below, minimizing the interval in which a human focus change could split
    // a multi-character direct-key sequence across two targets.
    nil
}

/// Preparation outcome. Both failure paths occur after `clearContents()` has
/// already run, so the pasteboard was modified even though no input was posted.
/// Returning a bare nil made the caller report `clipboard_touched: false` while
/// the clipboard was left empty, or still holding the text meant for the target.
enum TargetBoundClipboardPreparationV1 {
    case prepared(TargetBoundClipboardTransaction)
    case failedAfterTouch(restored: Bool)
}

func makeTargetBoundClipboardTransaction(
    _ text: String,
    pasteboard: NSPasteboard,
    waitBeforeRestore: @escaping () -> Void
) -> TargetBoundClipboardPreparationV1 {
    var savedItems: [[NSPasteboard.PasteboardType: Data]] = []
    for item in pasteboard.pasteboardItems ?? [] {
        var values: [NSPasteboard.PasteboardType: Data] = [:]
        for type in item.types {
            if let data = item.data(forType: type) { values[type] = data }
        }
        if !values.isEmpty { savedItems.append(values) }
    }
    let restoreSavedItemsIfOwned: (Int) -> Bool = { expectedChangeCount in
        guard pasteboard.changeCount == expectedChangeCount else { return false }
        pasteboard.clearContents()
        if savedItems.isEmpty { return true }
        let items = savedItems.map { values -> NSPasteboardItem in
            let item = NSPasteboardItem()
            for (type, data) in values { item.setData(data, forType: type) }
            return item
        }
        return pasteboard.writeObjects(items)
    }
    pasteboard.clearContents()
    let emptyOwnedChangeCount = pasteboard.changeCount
    guard pasteboard.setString(text, forType: .string) else {
        return .failedAfterTouch(restored: restoreSavedItemsIfOwned(emptyOwnedChangeCount))
    }
    let ownedChangeCount = pasteboard.changeCount
    guard let prepared = productionTargetBoundPreparedKey(key: "v", modifiers: ["command"]) else {
        return .failedAfterTouch(restored: restoreSavedItemsIfOwned(ownedChangeCount))
    }
    let restore: () -> Bool = {
        waitBeforeRestore()
        // Never overwrite clipboard content written by the user or another
        // application after our temporary value. Losing our clipboard
        // ownership is reported in the content-free acknowledgement instead.
        return restoreSavedItemsIfOwned(ownedChangeCount)
    }
    let post: () -> TargetBoundClipboardPostOutcome = {
        // This check is intentionally adjacent to CGEvent posting. If another
        // process replaced our temporary value, never paste its content into
        // the target application and never overwrite it during cleanup.
        guard pasteboard.changeCount == ownedChangeCount else {
            return .ownershipLost
        }
        return prepared.post() ? .committed : .failed
    }
    return .prepared(TargetBoundClipboardTransaction(post: post, restore: restore))
}

private func productionTargetBoundClipboardText(
    _ text: String
) -> TargetBoundClipboardPreparationV1 {
    makeTargetBoundClipboardTransaction(
        text,
        pasteboard: NSPasteboard.general,
        waitBeforeRestore: { Thread.sleep(forTimeInterval: 0.1) })
}

private func targetBoundInputVerificationTarget(
    _ request: TargetBoundInputRequestV1
) -> AXUIElement? {
    guard request.action == "type",
          request.ref != nil,
          let path = request.path,
          let processID = pid_t(exactly: request.pid),
          let window = targetBoundInputFocusedAXWindow(
              pid: request.pid, windowID: request.windowID),
          let target = resolveElement(in: window, path: path),
          axString(target, "AXRole") == request.expectedRole,
          targetBoundInputFingerprint(target) == request.expectedFingerprint,
          axElementsEqual(
              target,
              axFocusedElement(AXUIElementCreateApplication(processID))) else {
        return nil
    }
    return target
}

private func targetBoundInputStringValue(_ element: AXUIElement) -> String? {
    var raw: CFTypeRef?
    guard AXUIElementCopyAttributeValue(
        element, kAXValueAttribute as CFString, &raw) == .success,
          let raw, CFGetTypeID(raw) == CFStringGetTypeID() else { return nil }
    return raw as? String
}

private func targetBoundInputSelectedRange(_ element: AXUIElement) -> NSRange? {
    var raw: CFTypeRef?
    guard AXUIElementCopyAttributeValue(
        element, kAXSelectedTextRangeAttribute as CFString, &raw) == .success,
          let raw, CFGetTypeID(raw) == AXValueGetTypeID() else { return nil }
    var range = CFRange()
    guard AXValueGetValue(raw as! AXValue, .cfRange, &range),
          range.location >= 0, range.length >= 0 else { return nil }
    return NSRange(location: range.location, length: range.length)
}

func targetBoundExpectedTextValueV1(
    before: String,
    selectedRange: NSRange,
    insertedText: String
) -> String? {
    let utf16Length = (before as NSString).length
    guard selectedRange.location <= utf16Length,
          selectedRange.length <= utf16Length - selectedRange.location else { return nil }
    return (before as NSString).replacingCharacters(
        in: selectedRange, with: insertedText)
}

private func productionTargetBoundTypeVerification(
    request: TargetBoundInputRequestV1,
    text: String
) -> TargetBoundTypeVerificationPreparationV1 {
    if request.ref == nil {
        return .unavailable(failureCode: "postcondition_not_declared")
    }
    guard let target = targetBoundInputVerificationTarget(request) else {
        return .failed(failureCode: "focused_element_mismatch")
    }
    let role = axString(target, "AXRole") ?? ""
    guard !isSensitiveAXValue(axValueSensitivityMetadata(target, role: role)) else {
        // Never read AXValue or AXSelectedTextRange for a sensitive target.
        return .unavailable(failureCode: "verification_redacted_sensitive_target")
    }
    guard let before = targetBoundInputStringValue(target),
          let selectedRange = targetBoundInputSelectedRange(target),
          let expected = targetBoundExpectedTextValueV1(
              before: before, selectedRange: selectedRange, insertedText: text) else {
        return .unavailable(failureCode: "target_value_readback_unavailable")
    }
    guard expected != before else {
        // An idempotent replacement cannot prove that our event caused the
        // observed value, even when the read-back equals the expected string.
        return .unavailable(failureCode: "target_value_noop_unverifiable")
    }
    guard let processID = pid_t(exactly: request.pid) else {
        return .failed(failureCode: "process_not_live")
    }
    let appElement = AXUIElementCreateApplication(processID)
    return .ready(.init(observe: { timeout in
        // Apple scopes a messaging timeout to one exact AX element reference;
        // it does not flow from the application object to descendants. Split
        // this attempt's allowance across the exact app reference used for the
        // focus check and the already-bound target used for AXValue read-back.
        let perElementTimeout = timeout / 2
        let focusedOutcome = withTargetedAXMessagingTimeoutV1(
            element: appElement,
            timeout: perElementTimeout,
            operation: { axFocusedElement(appElement) })
        guard case let .completed(focusedElement) = focusedOutcome,
              let focusedElement else {
            return .unavailable
        }
        guard axElementsEqual(target, focusedElement) else {
            return .targetChanged
        }
        let valueOutcome = withTargetedAXMessagingTimeoutV1(
            element: target,
            timeout: perElementTimeout,
            operation: { targetBoundInputStringValue(target) })
        guard case let .completed(observedValue) = valueOutcome,
              let observed = observedValue else {
            return .unavailable
        }
        return observed == expected ? .matched : .mismatch
    }))
}

func targetBoundInputCancellationMarkerURLV1(
    requestID: Int64,
    request: TargetBoundInputRequestV1
) -> URL {
    let authority = Data(
        "\(request.pid):\(request.bundleID):\(request.windowID):\(requestID)".utf8)
    let digest = SHA256.hash(data: authority)
        .map { String(format: "%02x", $0) }.joined()
    return URL(fileURLWithPath: "/tmp", isDirectory: true).appendingPathComponent(
        "kocoro-ax-target-input-cancel-v1-\(digest)", isDirectory: false)
}

func productionTargetBoundInputDependenciesV1(
    isCancelled: @escaping () -> Bool
) -> TargetBoundInputDependencies {
    TargetBoundInputDependencies(
        canAdmitInput: processInputCommitGateV1.canAdmitInput,
        authorityFailure: productionTargetBoundInputAuthorityFailureBoundedV1,
        restoreAuthority: productionRestoreTargetBoundInputAuthority,
        requiresClipboard: { _ in true },
        prepareHotkey: productionTargetBoundPreparedKey,
        prepareKeypress: { keys, modifiers in
            productionTargetBoundPreparedKeySequence(
                keys: keys, modifiers: modifiers, isCancelled: isCancelled)
        },
        prepareDirectText: productionTargetBoundDirectText,
        prepareClipboardText: productionTargetBoundClipboardText,
        prepareTypeVerification: productionTargetBoundTypeVerification,
        observePhysicalInput: observePhysicalInputInterferenceV1,
        now: Date.init,
        sleep: Thread.sleep(forTimeInterval:))
}

let productionTargetBoundInputDependencies =
    productionTargetBoundInputDependenciesV1(isCancelled: { false })
