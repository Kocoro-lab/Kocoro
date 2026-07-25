import CoreGraphics
import XCTest
@testable import ax_server

final class CoordinatePixelScrollTests: XCTestCase {
    func testProviderDeltasMapExactlyToOppositeCGPointAxes() {
        XCTAssertEqual(
            coordinatePixelScrollCGDeltasV1(providerX: 37, providerY: -618),
            .init(axis1: 618, axis2: -37))
        XCTAssertNil(coordinatePixelScrollCGDeltasV1(
            providerX: Int64(Int32.min), providerY: 1))
        XCTAssertNil(coordinatePixelScrollCGDeltasV1(
            providerX: 1, providerY: Int64(Int32.min)))
    }

    func testProviderPixelsScaleThroughImmutableFrameWithIdenticalRounding() {
        XCTAssertEqual(
            coordinatePixelScrollCGDeltasV1(
                providerX: 37, providerY: -618,
                scaleX: 2.0 / 3.0, scaleY: 2.0 / 3.0),
            .init(axis1: 412, axis2: -25))
        XCTAssertEqual(
            coordinatePixelScrollCGDeltasV1(
                providerX: 37, providerY: -618,
                scaleX: 2, scaleY: 0.5),
            .init(axis1: 309, axis2: -74))
        XCTAssertEqual(
            coordinatePixelScrollCGDeltasV1(
                providerX: 1, providerY: -1,
                scaleX: 0.5, scaleY: 0.5),
            .init(axis1: 1, axis2: -1))
        XCTAssertNil(coordinatePixelScrollCGDeltasV1(
            providerX: 1, providerY: 0, scaleX: 0.49, scaleY: 1))
        XCTAssertNil(coordinatePixelScrollCGDeltasV1(
            providerX: Int64(Int32.max), providerY: 0,
            scaleX: 2, scaleY: 1))
        XCTAssertNil(coordinatePixelScrollCGDeltasV1(
            providerX: 1, providerY: 0, scaleX: 0, scaleY: 1))
        XCTAssertNil(coordinatePixelScrollCGDeltasV1(
            providerX: 1, providerY: 0,
            scaleX: .infinity, scaleY: 1))
    }

    func testProductionPreparationProvesExactMovePixelUnitAndFullDeltaFields() {
        let point = CoordinateMouseEventPointV1(x: 123.5, y: 456.5)
        let prepared = productionCoordinatePixelScrollPreparedEventsV1(
            point: point, providerDeltaX: 37, providerDeltaY: -618,
            observePointer: { .init(x: 1, y: 2) })
        XCTAssertNotNil(prepared)
        XCTAssertEqual(prepared?.point, point)
        XCTAssertEqual(prepared?.providerDeltaX, 37)
        XCTAssertEqual(prepared?.providerDeltaY, -618)
        XCTAssertEqual(prepared?.cgDeltas, .init(axis1: 618, axis2: -37))
        XCTAssertTrue(prepared?.eventContractVerified == true)
        XCTAssertEqual(prepared?.pointerMoveEventType, .mouseMoved)
        XCTAssertEqual(prepared?.pointerMoveEventLocation, point)
        XCTAssertEqual(prepared?.scrollEventType, .scrollWheel)
        XCTAssertEqual(prepared?.scrollEventLocation, point)
        XCTAssertEqual(prepared?.scrollEventContinuous, 1)
        XCTAssertEqual(prepared?.scrollEventPointDeltaAxis1, 618)
        XCTAssertEqual(prepared?.scrollEventPointDeltaAxis2, -37)
        XCTAssertEqual(prepared?.pointerMoveExpectedEventCount, 1)
    }

    func testProductionPreparationTreatsAlreadyPlacedPointerAsNoOpMove() {
        let point = CoordinateMouseEventPointV1(x: 123.5, y: 456.5)
        let prepared = productionCoordinatePixelScrollPreparedEventsV1(
            point: point, providerDeltaX: 37, providerDeltaY: -618,
            observePointer: { point })

        XCTAssertNotNil(prepared)
        XCTAssertEqual(prepared?.pointerMoveExpectedEventCount, 0)
        XCTAssertEqual(prepared?.postPointerMove(), .committed)
    }

    func testPhysicalMonitorWaitsForDeliveredUnattributedScrollCounter() {
        let counters = Array(
            repeating: UInt32(10),
            count: physicalInputHIDEventTypesV1.count)
        let synthetic = Array(
            repeating: UInt32(20),
            count: physicalInputHIDEventTypesV1.count)
        var delivered = counters
        delivered[physicalInputHIDEventTypesV1.firstIndex(of: .scrollWheel)!] += 1
        let baseline = PhysicalInputInterferenceSnapshotV1(
            pointer: .init(x: 123.5, y: 456.5),
            hidEventCounters: counters,
            syntheticEventCounters: synthetic)
        let current = PhysicalInputInterferenceSnapshotV1(
            pointer: .init(x: 123.5, y: 456.5),
            hidEventCounters: delivered,
            syntheticEventCounters: synthetic)
        var observations = [baseline, baseline, current]
        var settleCount = 0
        let monitor = CoordinatePixelScrollPhysicalMonitorV1(
            expectedSyntheticHeldModifierFlags: 0,
            observe: { observations.removeFirst() },
            settle: { settleCount += 1 },
            maximumSettleAttempts: 2)

        XCTAssertEqual(
            monitor.assess(
                expectedPointer: .init(x: 123.5, y: 456.5),
                expectedEvents: [.init(type: .scrollWheel, count: 1)],
                stage: "after_scroll"),
            .unchanged)
        XCTAssertEqual(settleCount, 1)
    }

    func testNormalExecutionCommitsMoveBeforeScrollAndNeverClaimsEffectVerification() throws {
        let harness = PixelScrollHarness()
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move", "scroll"])
        XCTAssertEqual(result.status, "committed_unverified")
        XCTAssertEqual(result.pointerMoveCommitState, "committed")
        XCTAssertEqual(result.scrollCommitState, "committed")
        XCTAssertEqual(result.failureCode, "scroll_postcondition_not_declared")
        XCTAssertEqual(result.requested?.cgPointDeltaAxis1, 618)
        XCTAssertEqual(result.requested?.cgPointDeltaAxis2, -37)
        XCTAssertFalse(result.retrySafe)
    }

    func testCancellationAfterMoveReportsMoveCommitAndNeverPostsScroll() throws {
        let harness = PixelScrollHarness()
        harness.cancelValues = [false, false, true]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move"])
        XCTAssertEqual(result.status, "committed_unverified")
        XCTAssertEqual(result.pointerMoveCommitState, "committed")
        XCTAssertEqual(result.scrollCommitState, "not_committed")
        XCTAssertEqual(result.phase, "between_commits")
        XCTAssertEqual(result.failureCode, "cancelled_before_scroll")
    }

    func testAuthorityDriftAfterMoveIsUserInterferenceAndStopsBeforeScroll() throws {
        let harness = PixelScrollHarness()
        harness.authorityFailures = [nil, "window_bounds_mismatch"]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move"])
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.pointerMoveCommitState, "committed")
        XCTAssertEqual(result.scrollCommitState, "not_committed")
        XCTAssertEqual(result.failureCode, "window_bounds_mismatch")
    }

    func testPrecommitPhysicalInterferenceNeverPostsEitherEvent() throws {
        let harness = PixelScrollHarness()
        harness.physicalAssessmentValues = [.interference]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, [])
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.pointerMoveCommitState, "not_committed")
        XCTAssertEqual(result.scrollCommitState, "not_committed")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
    }

    func testUnknownScrollCommitIsNeverRetriedOrFlattened() throws {
        let harness = PixelScrollHarness()
        harness.scrollCommitState = .unknown
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move", "scroll"])
        XCTAssertEqual(result.status, "commit_unknown")
        XCTAssertEqual(result.pointerMoveCommitState, "committed")
        XCTAssertEqual(result.scrollCommitState, "unknown")
        XCTAssertEqual(result.failureCode, "scroll_commit_unknown")
        XCTAssertFalse(result.retrySafe)
    }

    func testDeadlineBeforeMoveCommitsNoEvent() throws {
        let harness = PixelScrollHarness()
        harness.nowValues = [
            pixelScrollTestDate("2026-07-22T12:03:30Z"),
            pixelScrollTestDate("2026-07-22T12:03:30Z"),
            pixelScrollTestDate("2026-07-22T12:03:32Z"),
        ]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, [])
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "request_expired")
        XCTAssertEqual(result.pointerMoveCommitState, "not_committed")
    }

    func testCancellationImmediatelyBeforeMoveCommitsNoEvent() throws {
        let harness = PixelScrollHarness()
        harness.cancelValues = [false, true]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, [])
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "cancelled")
    }

    func testDeadlineBetweenMoveAndScrollPreservesOnlyMoveCommit() throws {
        let harness = PixelScrollHarness()
        harness.nowValues = [
            pixelScrollTestDate("2026-07-22T12:03:30Z"),
            pixelScrollTestDate("2026-07-22T12:03:30Z"),
            pixelScrollTestDate("2026-07-22T12:03:30Z"),
            pixelScrollTestDate("2026-07-22T12:03:32Z"),
        ]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move"])
        XCTAssertEqual(result.status, "committed_unverified")
        XCTAssertEqual(result.pointerMoveCommitState, "committed")
        XCTAssertEqual(result.scrollCommitState, "not_committed")
        XCTAssertEqual(result.failureCode, "request_expired_before_scroll")
    }

    func testPhysicalInterferenceAfterMoveStopsBeforeScroll() throws {
        let harness = PixelScrollHarness()
        harness.physicalAssessmentValues = [.unchanged, .interference]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move"])
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.scrollCommitState, "not_committed")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
    }

    func testPhysicalInterferenceAfterScrollPreservesBothCommits() throws {
        let harness = PixelScrollHarness()
        harness.physicalAssessmentValues = [
            .unchanged, .unchanged, .unchanged, .interference,
        ]
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move", "scroll"])
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.pointerMoveCommitState, "committed")
        XCTAssertEqual(result.scrollCommitState, "committed")
    }

    func testPointerEndpointMismatchStopsBeforeScroll() throws {
        let harness = PixelScrollHarness()
        harness.observedPointer = .init(x: 360.5, y: 450.5)
        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())
        XCTAssertEqual(harness.posts, ["move"])
        XCTAssertEqual(result.status, "committed_unverified")
        XCTAssertEqual(result.failureCode, "pointer_endpoint_not_verified")
        XCTAssertFalse(result.pointerEndpoint?.verified ?? true)
    }

    func testPointerEndpointIsReobservedAfterPhysicalInputSettles() throws {
        let harness = PixelScrollHarness()
        harness.observedPointerValues = [
            .init(x: 360.5, y: 450.5),
            .init(x: 350.5, y: 450.5),
        ]

        let result = runCoordinatePixelScrollV1(
            request: try pixelScrollRequest(),
            dependencies: harness.dependencies())

        XCTAssertEqual(harness.posts, ["move", "scroll"])
        XCTAssertEqual(result.status, "committed_unverified")
        XCTAssertEqual(result.failureCode, "scroll_postcondition_not_declared")
        XCTAssertTrue(result.pointerEndpoint?.verified ?? false)
    }

    private func pixelScrollRequest() throws -> CoordinatePixelScrollRequestV1 {
        try decodeCoordinatePixelScrollRPCRequestV1(
            fixture("coordinate_pixel_scroll.request.v1.json")).params
    }

    private func fixture(_ name: String) throws -> Data {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent("testdata/\(name)")
        return try Data(contentsOf: url)
    }
}

private final class PixelScrollHarness {
    var posts: [String] = []
    var cancelValues: [Bool] = []
    var authorityFailures: [String?] = []
    var physicalAssessmentValues: [PhysicalInputInterferenceAssessmentV1] = []
    var pointerMoveCommitState: CoordinatePixelScrollCommitStateV1 = .committed
    var scrollCommitState: CoordinatePixelScrollCommitStateV1 = .committed
    var observedPointer = CoordinateMouseEventPointV1(x: 350.5, y: 450.5)
    var observedPointerValues: [CoordinateMouseEventPointV1?] = []
    var nowValues: [Date] = []
    private var now = pixelScrollTestDate("2026-07-22T12:03:30Z")

    func dependencies() -> CoordinatePixelScrollDependenciesV1 {
        CoordinatePixelScrollDependenciesV1(
            authorityFailure: { _ in
                if !self.authorityFailures.isEmpty {
                    return self.authorityFailures.removeFirst()
                }
                return nil
            },
            prepare: { point, x, y, scaleX, scaleY, axis1, axis2 in
                CoordinatePixelScrollPreparedEventsV1(
                    point: point, providerDeltaX: x, providerDeltaY: y,
                    providerToQuartzScaleX: scaleX,
                    providerToQuartzScaleY: scaleY,
                    cgDeltas: coordinatePixelScrollCGDeltasV1(
                        providerX: x, providerY: y,
                        scaleX: scaleX, scaleY: scaleY)!,
                    eventContractVerified: true,
                    scrollEventPointDeltaAxis1: axis1,
                    scrollEventPointDeltaAxis2: axis2,
                    postPointerMove: {
                        self.posts.append("move")
                        return self.pointerMoveCommitState
                    },
                    postScroll: {
                        self.posts.append("scroll")
                        return self.scrollCommitState
                    })
            },
            observePointer: {
                if !self.observedPointerValues.isEmpty {
                    return self.observedPointerValues.removeFirst()
                }
                return self.observedPointer
            },
            assessPhysicalInput: { _, _, _ in
                if !self.physicalAssessmentValues.isEmpty {
                    return self.physicalAssessmentValues.removeFirst()
                }
                return .unchanged
            },
            isCancelled: {
                if !self.cancelValues.isEmpty {
                    return self.cancelValues.removeFirst()
                }
                return false
            },
            now: {
                if !self.nowValues.isEmpty {
                    return self.nowValues.removeFirst()
                }
                return self.now
            })
    }
}

private func pixelScrollTestDate(_ value: String) -> Date {
    ISO8601DateFormatter().date(from: value)!
}
