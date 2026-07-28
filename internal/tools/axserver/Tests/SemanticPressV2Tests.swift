import ApplicationServices
import XCTest
@testable import ax_server

final class SemanticPressV2Tests: XCTestCase {
    private let now = Date(timeIntervalSince1970: 1_800_000_000)

    func testIdentityDriftFailsBeforePress() throws {
        var pressCount = 0
        let dependencies = makeDependencies(
            bundleID: "com.attacker.Replaced",
            target: makeTarget(performPress: { timeout in
                pressCount += 1
                return .completed(error: .success, timeoutRestored: true)
            }))
        let result = runSemanticPressV2(
            request: makeRequest(), dependencies: dependencies)
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "process_identity_mismatch")
        XCTAssertEqual(result.commitState, "not_committed")
        XCTAssertEqual(pressCount, 0)
    }

    func testPrecommitPhysicalInputYieldsWithoutPress() throws {
        var pressCount = 0
        let heldModifier = snapshot(heldModifierFlags: 1)
        let result = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(
                target: makeTarget(performPress: { _ in
                    pressCount += 1
                    return .completed(error: .success, timeoutRestored: true)
                }),
                snapshots: [heldModifier]))
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertEqual(result.commitState, "not_committed")
        XCTAssertEqual(pressCount, 0)
    }

    func testBackgroundPolicyIgnoresUnrelatedPhysicalInput() throws {
        let changed = snapshot(hidCounter: 9, heldModifierFlags: 1)
        let result = runSemanticPressV2(
            request: makeRequest(interferencePolicy: "target_foreground"),
            dependencies: makeDependencies(
                snapshots: [changed],
                frontmostPIDs: [11, 11, 11, 11, 11]))
        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
    }

    func testBackgroundPolicyStopsBeforePressWhenTargetBecomesFrontmost() throws {
        var pressCount = 0
        let result = runSemanticPressV2(
            request: makeRequest(interferencePolicy: "target_foreground"),
            dependencies: makeDependencies(
                target: makeTarget(performPress: { _ in
                    pressCount += 1
                    return .completed(error: .success, timeoutRestored: true)
                }),
                frontmostPIDs: [42]))
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.commitState, "not_committed")
        XCTAssertEqual(result.failureCode, "target_became_frontmost")
        XCTAssertEqual(pressCount, 0)
    }

    func testBackgroundPolicyReportsForegroundRaceAfterCommit() throws {
        let result = runSemanticPressV2(
            request: makeRequest(interferencePolicy: "target_foreground"),
            dependencies: makeDependencies(frontmostPIDs: [11, 42]))
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.failureCode, "target_foreground_interference")
    }

    func testPhysicalInputInsideSynchronousAXPressIsDetectedAfterReturn() throws {
        let clean = snapshot()
        let changed = snapshot(hidCounter: 1)
        var timeout: TimeInterval?
        let result = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(
                target: makeTarget(performPress: {
                    timeout = $0
                    return .completed(error: .success, timeoutRestored: true)
                }),
                snapshots: [clean, changed]))
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertEqual(timeout, targetedAXPostconditionBudgetV1.maxSynchronousCallDuration)
    }

    func testMonitorLossBeforeAndAfterCommitUsesDifferentTaggedOutcomes() throws {
        let precommit = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(snapshots: [nil]))
        XCTAssertEqual(precommit.status, "failed")
        XCTAssertEqual(precommit.commitState, "not_committed")
        XCTAssertEqual(precommit.failureCode, "interference_detection_unavailable")

        let postcommit = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(snapshots: [snapshot(), nil]))
        XCTAssertEqual(postcommit.status, "completed_unverified")
        XCTAssertEqual(postcommit.commitState, "committed")
        XCTAssertEqual(postcommit.failureCode, "interference_detection_unavailable")
    }

    func testTargetedActionAndObservationReceiveBoundedTimeoutsButNeverClaimVerified() throws {
        let clean = snapshot()
        var actionTimeout: TimeInterval?
        var observationTimeout: TimeInterval?
        let target = makeTarget(
            performPress: {
                actionTimeout = $0
                return .completed(error: .success, timeoutRestored: true)
            },
            observeTarget: {
                observationTimeout = $0
                return .present
            })
        let result = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(
                target: target,
                snapshots: [clean, clean, clean, clean, clean]))
        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.phase, "post_verification")
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertNil(result.postcondition)
        XCTAssertFalse(result.retrySafe)
        XCTAssertEqual(actionTimeout, targetedAXPostconditionBudgetV1.maxSynchronousCallDuration)
        XCTAssertEqual(observationTimeout, targetedAXPostconditionBudgetV1.maxSynchronousCallDuration)
    }

    func testTimeoutSetupFailureAndExpiredRequestNeverCommit() throws {
        let timeoutFailure = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(
                target: makeTarget(performPress: { _ in .timeoutUnavailable }),
                snapshots: [snapshot()]))
        XCTAssertEqual(timeoutFailure.status, "failed")
        XCTAssertEqual(timeoutFailure.failureCode, "ax_messaging_timeout_unavailable")
        XCTAssertEqual(timeoutFailure.commitState, "not_committed")

        let expired = runSemanticPressV2(
            request: makeRequest(deadline: now.addingTimeInterval(-1)),
            dependencies: makeDependencies())
        XCTAssertEqual(expired.status, "failed")
        XCTAssertEqual(expired.failureCode, "request_expired")
        XCTAssertEqual(expired.commitState, "not_committed")
    }

    func testInterferenceDuringPostObservationOverridesOrdinaryCompletion() throws {
        let clean = snapshot()
        let changed = snapshot(hidCounter: 1)
        let result = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(
                snapshots: [clean, clean, clean, changed]))
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
    }

    func testFinalReturnCheckpointOverridesOtherwiseOrdinaryCompletion() throws {
        let clean = snapshot()
        let changed = snapshot(hidCounter: 1)
        let result = runSemanticPressV2(
            request: makeRequest(),
            dependencies: makeDependencies(
                snapshots: [clean, clean, clean, clean, changed]))
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
    }

    func testCannotCompleteAndGenericFailureAreCommitUnknownAndNeverRetryable() throws {
        for error in [AXError.cannotComplete, AXError.failure] {
            let result = runSemanticPressV2(
                request: makeRequest(),
                dependencies: makeDependencies(
                    target: makeTarget(performPress: { _ in
                        .completed(error: error, timeoutRestored: true)
                    }),
                    snapshots: [snapshot(), snapshot(), snapshot()]))
            XCTAssertEqual(result.status, "completed_unverified")
            XCTAssertEqual(result.commitState, "unknown")
            XCTAssertEqual(result.phase, "action")
            XCTAssertEqual(result.failureCode, "ax_press_commit_unknown")
            XCTAssertFalse(result.retrySafe)
        }
    }

    func testExplicitPrecommitAXErrorsRemainNotCommittedFailures() throws {
        for error in [
            AXError.actionUnsupported, AXError.illegalArgument,
            AXError.invalidUIElement, AXError.notImplemented,
        ] {
            let result = runSemanticPressV2(
                request: makeRequest(),
                dependencies: makeDependencies(
                    target: makeTarget(performPress: { _ in
                        .completed(error: error, timeoutRestored: true)
                    }),
                    snapshots: [snapshot(), snapshot()]))
            XCTAssertEqual(result.status, "failed")
            XCTAssertEqual(result.commitState, "not_committed")
            XCTAssertEqual(result.failureCode, "ax_press_failed")
            XCTAssertFalse(result.retrySafe)
        }
    }

    func testRiskDestinationDriftAfterPhysicalBaselineNeverPresses() throws {
        var pressCount = 0
        let result = runSemanticPressV2(
            request: makeRequest(riskWindowTitle: "general - Slack"),
            dependencies: makeDependencies(
                windowTitle: "random - Slack",
                target: makeTarget(performPress: { _ in
                    pressCount += 1
                    return .completed(error: .success, timeoutRestored: true)
                }),
                snapshots: [snapshot(), snapshot()]))
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.commitState, "not_committed")
        XCTAssertEqual(result.failureCode, "risk_destination_drift")
        XCTAssertEqual(pressCount, 0)
    }

    func testRiskDestinationAssertionRejectsFormatCharacters() throws {
        for title in ["pay\u{200B}roll", "safe\u{202E}liame"] {
            let request = makeRequest(riskWindowTitle: title)
            XCTAssertThrowsError(try JSONEncoder().encode(request))
        }
    }

    func testRiskDestinationReadIsBoundedAndRestoreFailureFailsClosed() throws {
        var timeouts: [Float] = []
        let result = readSemanticPressRiskWindowTitleV2(
            element: 7, timeout: 0.1,
            setTimeout: { _, timeout in
                timeouts.append(timeout)
                return timeout == 0 ? .cannotComplete : .success
            },
            read: { "general - Slack" })
        guard case .unavailable = result else {
            return XCTFail("restore failure exposed a trusted title")
        }
        XCTAssertEqual(timeouts, [0.1, 0])
    }

    func testRiskDestinationUnavailableOrDeadlineOverrunNeverPresses() throws {
        for overrun in [false, true] {
            var pressCount = 0
            var liveNow = now
            let result = runSemanticPressV2(
                request: makeRequest(riskWindowTitle: "general - Slack"),
                dependencies: makeDependencies(
                    windowTitleObservation: { _ in
                        if overrun { liveNow = self.now.addingTimeInterval(2) }
                        return overrun ? .value("general - Slack") : .unavailable
                    },
                    nowProvider: { liveNow },
                    target: makeTarget(performPress: { _ in
                        pressCount += 1
                        return .completed(error: .success, timeoutRestored: true)
                    }),
                    snapshots: [snapshot(), snapshot()]))
            XCTAssertEqual(result.status, "failed")
            XCTAssertEqual(result.commitState, "not_committed")
            XCTAssertEqual(
                result.failureCode,
                overrun ? "request_expired" : "risk_destination_unavailable")
            XCTAssertEqual(pressCount, 0)
        }
    }

    private func makeRequest(
        deadline: Date? = nil,
        riskWindowTitle: String? = nil,
        interferencePolicy: String = "global_physical"
    ) -> SemanticPressRequestV2 {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return .init(
            pid: 42, bundleID: "com.apple.Notes", windowID: 7001,
            ref: "e1", path: "window[0]/AXButton[0]", expectedRole: "AXButton",
            expectedFingerprint: "axf_target",
            interferencePolicy: interferencePolicy,
            commitDeadlineAt: formatter.string(
                from: deadline ?? now.addingTimeInterval(1)),
            riskDestinationAssertion: riskWindowTitle.map {
                .init(expectedWindowTitle: $0)
            })
    }

    private func makeTarget(
        role: String = "AXButton",
        fingerprint: String = "axf_target",
        enabled: Bool? = true,
        sensitive: Bool = false,
        actions: [String] = ["AXPress"],
        performPress: @escaping (TimeInterval) -> SemanticPressAXCallResultV2 = {
            _ in .completed(error: .success, timeoutRestored: true)
        },
        observeTarget: @escaping (TimeInterval) -> SemanticPressTargetObservationV2 = {
            _ in .present
        }
    ) -> SemanticPressTargetV2 {
        .init(
            role: role, fingerprint: fingerprint, enabled: enabled,
            sensitive: sensitive, actions: actions,
            performPress: performPress, observeTarget: observeTarget)
    }

    private func makeDependencies(
        pidLive: Bool = true,
        bundleID: String? = "com.apple.Notes",
        window: SemanticPressWindowResolutionV2 = .unique,
        windowTitle: String? = "Note",
        windowTitleObservation: ((TimeInterval) -> SemanticPressRiskWindowTitleObservationV2)? = nil,
        nowProvider: (() -> Date)? = nil,
        target: SemanticPressTargetV2? = nil,
        fingerprintCount: Int = 1,
        snapshots: [PhysicalInputInterferenceSnapshotV1?]? = nil,
        frontmostPIDs: [Int?]? = nil
    ) -> SemanticPressDependenciesV2 {
        var remaining = snapshots ?? [
            snapshot(), snapshot(), snapshot(), snapshot(), snapshot(),
        ]
        var remainingFrontmost = frontmostPIDs ?? [11, 11, 11, 11, 11]
        let exactTarget = target ?? makeTarget()
        return .init(
            isPIDLive: { _ in pidLive },
            bundleIDForPID: { _ in bundleID },
            resolveWindow: { _, _ in window },
            resolveTarget: { _, _, _ in exactTarget },
            countFingerprint: { _, _, _ in fingerprintCount },
            windowTitle: { _, _, timeout in
                windowTitleObservation?(timeout) ?? .value(windowTitle)
            },
            frontmostPID: {
                remainingFrontmost.isEmpty ? nil : remainingFrontmost.removeFirst()
            },
            observePhysicalInput: {
                remaining.isEmpty ? nil : remaining.removeFirst()
            },
            now: nowProvider ?? { self.now },
            sleep: { _ in })
    }

    private func snapshot(
        hidCounter: UInt32 = 0,
        heldModifierFlags: UInt64 = 0
    ) -> PhysicalInputInterferenceSnapshotV1 {
        .init(
            pointer: .init(x: 100, y: 200),
            hidEventCounters: [hidCounter],
            syntheticEventCounters: [0],
            heldModifierFlags: heldModifierFlags)
    }
}
