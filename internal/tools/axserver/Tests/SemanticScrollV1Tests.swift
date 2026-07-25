import ApplicationServices
import Foundation
import XCTest
@testable import ax_server

final class SemanticScrollV1Tests: XCTestCase {
    private let now = Date(timeIntervalSince1970: 1_800_000_000)

    func testAllRequestedStepsMustChangeTheExactAXMetricInDirectionToVerify() {
        let harness = ScrollHarness(values: [0.1, 0.2, 0.2, 0.3, 0.3, 0.4])
        let result = runSemanticScrollV1(
            request: request(steps: 3), dependencies: harness.dependencies(now: now))
        XCTAssertEqual(result.status, "verified")
        XCTAssertEqual(result.commitState, "committed")
        XCTAssertEqual(result.postcondition, "scroll_value_changed_in_direction")
        XCTAssertEqual(result.stepsCompleted, 3)
        XCTAssertEqual(result.expectedSteps, 3)
        XCTAssertEqual(harness.actions, ["AXIncrement", "AXIncrement", "AXIncrement"])
        XCTAssertEqual(harness.fingerprintCalls, 1)
    }

    func testNoOpAndPartialBoundaryNeverClaimVerified() {
        let noOp = ScrollHarness(values: [0.1, 0.1, 0.1, 0.1])
        let noOpResult = runSemanticScrollV1(
            request: request(), dependencies: noOp.dependencies(now: now))
        XCTAssertEqual(noOpResult.status, "completed_unverified")
        XCTAssertEqual(noOpResult.failureCode, "scroll_value_unchanged")
        XCTAssertEqual(noOpResult.stepsCompleted, 0)

        let partial = ScrollHarness(values: [0.8, 0.9, 0.9, 1.0, 1.0])
        let partialResult = runSemanticScrollV1(
            request: request(steps: 3), dependencies: partial.dependencies(now: now))
        XCTAssertEqual(partialResult.status, "completed_unverified")
        XCTAssertEqual(partialResult.failureCode, "scroll_boundary_reached")
        XCTAssertEqual(partialResult.stepsCompleted, 2)
        XCTAssertEqual(partialResult.expectedSteps, 3)
    }

    func testUnsupportedMetricOrActionRequiresFallbackBeforeAnySideEffect() {
        for target in [
            ScrollHarness.target(metricSupported: false),
            ScrollHarness.target(actionSupported: false),
        ] {
            let harness = ScrollHarness(values: [0.2], target: target)
            let result = runSemanticScrollV1(
                request: request(), dependencies: harness.dependencies(now: now))
            XCTAssertEqual(result.status, "fallback_required")
            XCTAssertEqual(result.commitState, "not_committed")
            XCTAssertEqual(result.failureCode, "ax_scroll_metric_unsupported")
            XCTAssertEqual(harness.actions.count, 0)
        }
    }

    func testStaleAmbiguousSensitiveAndDisabledTargetsFailBeforeAction() {
        let cases: [SemanticScrollDependenciesV1] = [
            ScrollHarness(values: [0.2], bundleID: "com.attacker.Replaced").dependencies(now: now),
            ScrollHarness(values: [0.2], window: .ambiguous).dependencies(now: now),
            ScrollHarness(values: [0.2], target: ScrollHarness.target(role: "AXButton")).dependencies(now: now),
            ScrollHarness(values: [0.2], target: ScrollHarness.target(fingerprint: "changed")).dependencies(now: now),
            ScrollHarness(values: [0.2], fingerprintCount: 2).dependencies(now: now),
            ScrollHarness(values: [0.2], target: ScrollHarness.target(sensitive: true)).dependencies(now: now),
            ScrollHarness(values: [0.2], target: ScrollHarness.target(enabled: nil)).dependencies(now: now),
        ]
        for dependencies in cases {
            let result = runSemanticScrollV1(request: request(), dependencies: dependencies)
            XCTAssertEqual(result.status, "failed")
            XCTAssertEqual(result.commitState, "not_committed")
        }
    }

    func testTimeoutAndCommitUnknownRemainNonRetryable() {
        let windowTimeout = ScrollHarness(values: [0.2], window: .timeoutUnavailable)
        let windowTimeoutResult = runSemanticScrollV1(
            request: request(), dependencies: windowTimeout.dependencies(now: now))
        XCTAssertEqual(windowTimeoutResult.status, "failed")
        XCTAssertEqual(windowTimeoutResult.failureCode, "ax_messaging_timeout_unavailable")
        XCTAssertEqual(windowTimeout.actions.count, 0)

        let targetTimeout = ScrollHarness(values: [0.2], targetTimeoutUnavailable: true)
        let targetTimeoutResult = runSemanticScrollV1(
            request: request(), dependencies: targetTimeout.dependencies(now: now))
        XCTAssertEqual(targetTimeoutResult.status, "failed")
        XCTAssertEqual(targetTimeoutResult.failureCode, "ax_messaging_timeout_unavailable")
        XCTAssertEqual(targetTimeout.actions.count, 0)

        let countTimeout = ScrollHarness(
            values: [0.2], fingerprintTimeoutUnavailable: true)
        let countTimeoutResult = runSemanticScrollV1(
            request: request(), dependencies: countTimeout.dependencies(now: now))
        XCTAssertEqual(countTimeoutResult.status, "failed")
        XCTAssertEqual(countTimeoutResult.failureCode, "ax_messaging_timeout_unavailable")
        XCTAssertEqual(countTimeout.actions.count, 0)

        let setup = ScrollHarness(
            values: [0.2], target: ScrollHarness.target(
                performAction: { _, _ in .timeoutUnavailable }))
        let setupResult = runSemanticScrollV1(
            request: request(), dependencies: setup.dependencies(now: now))
        XCTAssertEqual(setupResult.status, "failed")
        XCTAssertEqual(setupResult.failureCode, "ax_messaging_timeout_unavailable")

        let ambiguous = ScrollHarness(
            values: [0.2], target: ScrollHarness.target(
                performAction: { _, _ in
                    .completed(error: .cannotComplete, timeoutRestored: true)
                }))
        let ambiguousResult = runSemanticScrollV1(
            request: request(), dependencies: ambiguous.dependencies(now: now))
        XCTAssertEqual(ambiguousResult.status, "completed_unverified")
        XCTAssertEqual(ambiguousResult.commitState, "unknown")
        XCTAssertEqual(ambiguousResult.failureCode, "ax_scroll_commit_unknown")
        XCTAssertFalse(ambiguousResult.retrySafe)
    }

    func testPhysicalInputInterferenceIsCheckedBeforeAndAfterAXAction() {
        let clean = ScrollHarness.snapshot()
        let held = ScrollHarness.snapshot(heldModifierFlags: 1)
        let before = ScrollHarness(values: [0.2], snapshots: [held])
        let beforeResult = runSemanticScrollV1(
            request: request(), dependencies: before.dependencies(now: now))
        XCTAssertEqual(beforeResult.status, "user_interference")
        XCTAssertEqual(beforeResult.commitState, "not_committed")
        XCTAssertEqual(before.actions.count, 0)

        let changed = ScrollHarness.snapshot(hidCounter: 1)
        let after = ScrollHarness(values: [0.2, 0.3], snapshots: [clean, changed])
        let afterResult = runSemanticScrollV1(
            request: request(), dependencies: after.dependencies(now: now))
        XCTAssertEqual(afterResult.status, "user_interference")
        XCTAssertEqual(afterResult.commitState, "committed")
    }

    func testCommitDeadlineIsRecheckedBeforeEverySemanticStep() {
        var actionCommitted = false
        var postActionNowCalls = 0
        let harness = ScrollHarness(
            values: [0.1, 0.2],
            target: ScrollHarness.target(performAction: { _, _ in
                actionCommitted = true
                return .completed(error: .success, timeoutRestored: true)
            }))
        let result = runSemanticScrollV1(
            request: request(steps: 2),
            dependencies: harness.dependencies(now: now, nowProvider: {
                if actionCommitted {
                    postActionNowCalls += 1
                    if postActionNowCalls >= 4 {
                        return self.now.addingTimeInterval(2)
                    }
                }
                return self.now
            }))
        XCTAssertNotEqual(result.status, "verified")
        XCTAssertEqual(result.stepsCompleted, 1)
        XCTAssertEqual(harness.actions.count, 1)
    }

    func testCancellationBeforeAndAfterAXCommitReturnsExactTypedState() {
        let before = ScrollHarness(values: [0.2], cancelled: { true })
        let beforeResult = runSemanticScrollV1(
            request: request(), dependencies: before.dependencies(now: now))
        XCTAssertEqual(beforeResult.status, "cancelled")
        XCTAssertEqual(beforeResult.commitState, "not_committed")
        XCTAssertEqual(beforeResult.stepsCompleted, 0)
        XCTAssertEqual(before.actions.count, 0)

        var actionCommitted = false
        let after = ScrollHarness(
            values: [0.2, 0.3],
            target: ScrollHarness.target(performAction: { _, _ in
                actionCommitted = true
                return .completed(error: .success, timeoutRestored: true)
            }),
            cancelled: { actionCommitted })
        let afterResult = runSemanticScrollV1(
            request: request(), dependencies: after.dependencies(now: now))
        XCTAssertEqual(afterResult.status, "cancelled")
        XCTAssertEqual(afterResult.commitState, "committed")
        XCTAssertEqual(afterResult.stepsCompleted, 0)
        XCTAssertEqual(after.actions.count, 1)
    }

    private func request(steps: Int = 1) -> SemanticScrollRequestV1 {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return .init(
            pid: 42, bundleID: "com.apple.Notes", windowID: 7001,
            ref: "e3", path: "window[0]/AXScrollArea[0]",
            expectedRole: "AXScrollArea", expectedFingerprint: "axf_scroll_target",
            axis: "vertical", direction: "increment", steps: steps,
            commitDeadlineAt: formatter.string(from: now.addingTimeInterval(1)))
    }
}

private final class ScrollHarness {
    private static var neverCancelled: () -> Bool = { false }
    var values: [Double]
    var actions: [String] = []
    let bundleID: String?
    let window: SemanticScrollWindowResolutionV1
    let targetTemplate: SemanticScrollTargetV1
    let fingerprintCount: Int
    var fingerprintCalls = 0
    var snapshots: [PhysicalInputInterferenceSnapshotV1?]
    let cancelled: () -> Bool
    let targetTimeoutUnavailable: Bool
    let fingerprintTimeoutUnavailable: Bool

    init(
        values: [Double], bundleID: String? = "com.apple.Notes",
        window: SemanticScrollWindowResolutionV1 = .unique,
        target: SemanticScrollTargetV1 = ScrollHarness.target(),
        fingerprintCount: Int = 1,
        snapshots: [PhysicalInputInterferenceSnapshotV1?] = Array(
            repeating: ScrollHarness.snapshot(), count: 100),
        cancelled: @escaping () -> Bool = ScrollHarness.neverCancelled,
        targetTimeoutUnavailable: Bool = false,
        fingerprintTimeoutUnavailable: Bool = false
    ) {
        self.values = values
        self.bundleID = bundleID
        self.window = window
        targetTemplate = target
        self.fingerprintCount = fingerprintCount
        self.snapshots = snapshots
        self.cancelled = cancelled
        self.targetTimeoutUnavailable = targetTimeoutUnavailable
        self.fingerprintTimeoutUnavailable = fingerprintTimeoutUnavailable
    }

    func dependencies(
        now: Date, nowProvider: (() -> Date)? = nil
    ) -> SemanticScrollDependenciesV1 {
        .init(
            isPIDLive: { _ in true }, bundleIDForPID: { _ in self.bundleID },
            resolveWindow: { _, _, _ in self.window },
            resolveTarget: { _, _, _, _, _, _ in
                if self.targetTimeoutUnavailable { return .timeoutUnavailable }
                return .target(.init(
                    role: self.targetTemplate.role,
                    fingerprint: self.targetTemplate.fingerprint,
                    enabled: self.targetTemplate.enabled,
                    sensitive: self.targetTemplate.sensitive,
                    metricSupported: self.targetTemplate.metricSupported,
                    actionSupported: self.targetTemplate.actionSupported,
                    minValue: self.targetTemplate.minValue,
                    maxValue: self.targetTemplate.maxValue,
                    performAction: { action, timeout in
                        self.actions.append(action)
                        return self.targetTemplate.performAction(action, timeout)
                    },
                    observeValue: { _ in
                        guard !self.values.isEmpty else {
                            return .completed(value: nil, timeoutRestored: true)
                        }
                        let value = self.values.count == 1 ? self.values[0] : self.values.removeFirst()
                        return .completed(value: value, timeoutRestored: true)
                    }))
            },
            countFingerprint: { _, _, _, _ in
                self.fingerprintCalls += 1
                if self.fingerprintTimeoutUnavailable { return .timeoutUnavailable }
                return .count(self.fingerprintCount)
            },
            observePhysicalInput: {
                self.snapshots.isEmpty ? nil : self.snapshots.removeFirst()
            },
            isCancelled: self.cancelled,
            now: { nowProvider?() ?? now }, sleep: { _ in })
    }

    static func target(
        role: String = "AXScrollArea", fingerprint: String = "axf_scroll_target",
        enabled: Bool? = true, sensitive: Bool = false,
        metricSupported: Bool = true, actionSupported: Bool = true,
        performAction: @escaping (String, TimeInterval) -> SemanticScrollAXCallResultV1 = {
            _, _ in .completed(error: .success, timeoutRestored: true)
        }
    ) -> SemanticScrollTargetV1 {
        .init(
            role: role, fingerprint: fingerprint, enabled: enabled,
            sensitive: sensitive, metricSupported: metricSupported,
            actionSupported: actionSupported, minValue: 0, maxValue: 1,
            performAction: performAction,
            observeValue: { _ in .completed(value: 0.2, timeoutRestored: true) })
    }

    static func snapshot(
        hidCounter: UInt32 = 0, heldModifierFlags: UInt64 = 0
    ) -> PhysicalInputInterferenceSnapshotV1 {
        .init(
            pointer: .init(x: 10, y: 20), hidEventCounters: [hidCounter],
            syntheticEventCounters: [0], heldModifierFlags: heldModifierFlags)
    }
}
