import ApplicationServices
import XCTest
@testable import ax_server

final class SemanticTextSelectionV2Tests: XCTestCase {
    private let now = Date(timeIntervalSince1970: 1_800_000_000)

    func testExactRangeReadbackIsTheOnlyVerifiedOutcome() {
        var actionTimeout: TimeInterval?
        var observationTimeout: TimeInterval?
        let clean = snapshot()
        let result = runSemanticTextSelectionV2(
            request: request(),
            dependencies: dependencies(
                target: target(
                    setSelectedRange: { _, timeout in
                        actionTimeout = timeout
                        return .completed(error: .success, timeoutRestored: true)
                    },
                    observeSelectedRange: { timeout in
                        observationTimeout = timeout
                        return .completed(
                            observation: .range(.init(location: 5, length: 12)),
                            timeoutRestored: true)
                    }),
                snapshots: [clean, clean, clean, clean, clean]))

        XCTAssertEqual(result.status, "verified")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.postcondition, "selected_range_matches")
        XCTAssertEqual(result.selectedRange, .init(location: 5, length: 12))
        XCTAssertEqual(actionTimeout, targetedAXPostconditionBudgetV1.maxSynchronousCallDuration)
        XCTAssertEqual(observationTimeout, targetedAXPostconditionBudgetV1.maxSynchronousCallDuration)
    }

    func testMismatchedRangeNeverClaimsVerified() {
        let clean = snapshot()
        let result = runSemanticTextSelectionV2(
            request: request(),
            dependencies: dependencies(
                target: target(observeSelectedRange: { _ in
                    .completed(
                        observation: .range(.init(location: 6, length: 12)),
                        timeoutRestored: true)
                }),
                snapshots: Array(repeating: clean, count: 20)))

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.failureCode, "selected_range_mismatch")
        XCTAssertEqual(result.selectedRange, .init(location: 6, length: 12))
        XCTAssertNil(result.postcondition)
    }

    func testIdentityAndFingerprintDriftFailBeforeMutation() {
        for dependencies in [
            dependencies(bundleID: "com.attacker.Replaced"),
            dependencies(window: .ambiguous),
            dependencies(target: target(role: "AXButton")),
            dependencies(target: target(fingerprint: "axf_changed")),
            dependencies(fingerprintCount: 2),
            dependencies(target: target(sensitive: true)),
            dependencies(target: target(enabled: nil)),
        ] {
            let result = runSemanticTextSelectionV2(
                request: request(), dependencies: dependencies)
            XCTAssertEqual(result.status, "failed")
            XCTAssertEqual(result.commitState, "not_committed")
        }
    }

    func testClearlyUnsupportedTargetRequiresExplicitFallbackDecisionWithoutMutation() {
        var setCalls = 0
        let result = runSemanticTextSelectionV2(
            request: request(),
            dependencies: dependencies(target: target(
                supportsParameterizedTextRange: false,
                setSelectedRange: { _, _ in
                    setCalls += 1
                    return .completed(error: .success, timeoutRestored: true)
                })))
        XCTAssertEqual(result.status, "fallback_required")
        XCTAssertEqual(result.commitState, "not_committed")
        XCTAssertEqual(result.failureCode, "ax_text_range_unsupported")
        XCTAssertEqual(setCalls, 0)
    }

    func testCannotCompleteAndGenericFailureAreCommitUnknownAndNonRetryable() {
        for error in [AXError.cannotComplete, AXError.failure] {
            let clean = snapshot()
            let result = runSemanticTextSelectionV2(
                request: request(),
                dependencies: dependencies(
                    target: target(setSelectedRange: { _, _ in
                        .completed(error: error, timeoutRestored: true)
                    }),
                    snapshots: [clean, clean, clean]))
            XCTAssertEqual(result.status, "completed_unverified")
            XCTAssertEqual(result.commitState, "unknown")
            XCTAssertEqual(result.phase, "action")
            XCTAssertEqual(result.failureCode, "ax_selection_commit_unknown")
            XCTAssertFalse(result.retrySafe)
        }
    }

    func testExplicitPrecommitAXErrorsRemainNotCommitted() {
        for error in [
            AXError.attributeUnsupported, AXError.parameterizedAttributeUnsupported,
            AXError.illegalArgument, AXError.invalidUIElement, AXError.notImplemented,
        ] {
            let result = runSemanticTextSelectionV2(
                request: request(),
                dependencies: dependencies(
                    target: target(setSelectedRange: { _, _ in
                        .completed(error: error, timeoutRestored: true)
                    }),
                    snapshots: [snapshot(), snapshot()]))
            XCTAssertEqual(result.commitState, "not_committed")
            if error == .attributeUnsupported || error == .parameterizedAttributeUnsupported {
                XCTAssertEqual(result.status, "fallback_required")
            } else {
                XCTAssertEqual(result.status, "failed")
                XCTAssertEqual(result.failureCode, "ax_selection_failed")
            }
        }
    }

    func testActionTimeoutSetupFailsBeforeCommitAndRestoreFailureStaysUnverified() {
        let setup = runSemanticTextSelectionV2(
            request: request(),
            dependencies: dependencies(
                target: target(setSelectedRange: { _, _ in .timeoutUnavailable }),
                snapshots: [snapshot()]))
        XCTAssertEqual(setup.status, "failed")
        XCTAssertEqual(setup.commitState, "not_committed")
        XCTAssertEqual(setup.failureCode, "ax_messaging_timeout_unavailable")

        let clean = snapshot()
        let restore = runSemanticTextSelectionV2(
            request: request(),
            dependencies: dependencies(
                target: target(setSelectedRange: { _, _ in
                    .completed(error: .success, timeoutRestored: false)
                }),
                snapshots: [clean, clean, clean, clean, clean]))
        XCTAssertEqual(restore.status, "completed_unverified")
        XCTAssertEqual(restore.commitState, "committed")
        XCTAssertEqual(restore.failureCode, "ax_messaging_timeout_restore_failed")
    }

    func testPhysicalInputAndMonitorLossRetainExactCommitState() {
        let clean = snapshot()
        let held = snapshot(heldModifierFlags: 1)
        let changed = snapshot(hidCounter: 1)
        for (snapshots, expectedState) in [
            ([held], "not_committed"),
            ([clean, changed], "committed"),
        ] {
            let result = runSemanticTextSelectionV2(
                request: request(),
                dependencies: dependencies(snapshots: snapshots))
            XCTAssertEqual(result.status, "user_interference")
            XCTAssertEqual(result.commitState, expectedState)
            XCTAssertEqual(result.failureCode, "physical_input_interference")
        }

        let postCommitLoss = runSemanticTextSelectionV2(
            request: request(),
            dependencies: dependencies(snapshots: [clean, nil]))
        XCTAssertEqual(postCommitLoss.status, "completed_unverified")
        XCTAssertEqual(postCommitLoss.commitState, "committed")
        XCTAssertEqual(postCommitLoss.failureCode, "interference_detection_unavailable")
    }

    func testInterferenceDuringAndAfterVerificationOverridesVerifiedResult() {
        let clean = snapshot()
        let changed = snapshot(hidCounter: 1)
        for snapshots in [
            [clean, clean, clean, changed],
            [clean, clean, clean, clean, changed],
        ] {
            let result = runSemanticTextSelectionV2(
                request: request(),
                dependencies: dependencies(snapshots: snapshots))
            XCTAssertEqual(result.status, "user_interference")
            XCTAssertEqual(result.commitState, "committed")
        }
    }

    private func request(deadline: Date? = nil) -> SemanticTextSelectionRequestV2 {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return .init(
            pid: 42, bundleID: "com.apple.Notes", windowID: 7001,
            ref: "e1", path: "window[0]/AXTextArea[0]", expectedRole: "AXTextArea",
            expectedFingerprint: "axf_target", range: .init(location: 5, length: 12),
            commitDeadlineAt: formatter.string(
                from: deadline ?? now.addingTimeInterval(1)))
    }

    private func target(
        role: String = "AXTextArea",
        fingerprint: String = "axf_target",
        enabled: Bool? = true,
        sensitive: Bool = false,
        supportsParameterizedTextRange: Bool = true,
        selectedTextRangeSettable: Bool = true,
        setSelectedRange: @escaping (SemanticTextRangeV2, TimeInterval) ->
            SemanticTextSelectionAXCallResultV2 = { _, _ in
                .completed(error: .success, timeoutRestored: true)
            },
        observeSelectedRange: @escaping (TimeInterval) ->
            SemanticTextSelectionObservationCallResultV2 = { _ in
                .completed(
                    observation: .range(.init(location: 5, length: 12)),
                    timeoutRestored: true)
            }
    ) -> SemanticTextSelectionTargetV2 {
        .init(
            role: role, fingerprint: fingerprint, enabled: enabled,
            sensitive: sensitive,
            supportsParameterizedTextRange: supportsParameterizedTextRange,
            selectedTextRangeSettable: selectedTextRangeSettable,
            setSelectedRange: setSelectedRange,
            observeSelectedRange: observeSelectedRange)
    }

    private func dependencies(
        pidLive: Bool = true,
        bundleID: String? = "com.apple.Notes",
        window: SemanticTextSelectionWindowResolutionV2 = .unique,
        target: SemanticTextSelectionTargetV2? = nil,
        fingerprintCount: Int = 1,
        snapshots: [PhysicalInputInterferenceSnapshotV1?]? = nil
    ) -> SemanticTextSelectionDependenciesV2 {
        var remaining = snapshots ?? Array(repeating: snapshot(), count: 20)
        return .init(
            isPIDLive: { _ in pidLive },
            bundleIDForPID: { _ in bundleID },
            resolveWindow: { _, _ in window },
            resolveTarget: { _, _, _ in target ?? self.target() },
            countFingerprint: { _, _, _ in fingerprintCount },
            observePhysicalInput: {
                remaining.isEmpty ? nil : remaining.removeFirst()
            },
            now: { self.now },
            sleep: { _ in })
    }

    private func snapshot(
        hidCounter: UInt32 = 0,
        heldModifierFlags: UInt64 = 0
    ) -> PhysicalInputInterferenceSnapshotV1 {
        .init(
            pointer: .init(x: 100, y: 200),
            hidEventCounters: [hidCounter], syntheticEventCounters: [0],
            heldModifierFlags: heldModifierFlags)
    }
}
