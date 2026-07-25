import CoreGraphics
import Foundation
import XCTest
@testable import ax_server

final class CoordinateMouseEventTests: XCTestCase {
    func testPhysicalAssessmentWaitsForDeliveredSyntheticCounter() {
        let baseline = Harness.physicalSnapshot(pointerX: 100)
        let delivered = Harness.physicalSnapshot(
            pointerX: 200.5,
            changes: [(.mouseMoved, 1)]
        )
        var observations = [baseline, delivered]
        var settleCount = 0

        let result = coordinateMouseAssessPhysicalInputV1(
            baseline: baseline,
            expectedPointer: .init(x: 200.5, y: 300.5),
            expectedSyntheticEvents: [(.mouseMoved, 1)],
            expectedSyntheticHeldModifierFlags: 0,
            observe: { observations.removeFirst() },
            settle: { settleCount += 1 },
            maximumSettleAttempts: 2
        )

        XCTAssertEqual(result.assessment, .unchanged)
        XCTAssertEqual(result.snapshot, delivered)
        XCTAssertEqual(settleCount, 1)
    }

    func testNoOpPointerMoveDoesNotRequireSyntheticCounter() throws {
        let harness = Harness()
        harness.moveOutcome = .committed(
            observed: .init(x: 200.5, y: 300.5),
            expectedSyntheticEventCount: 0
        )
        harness.physicalInputSnapshots = [
            Harness.physicalSnapshot(pointerX: 200.5),
            Harness.physicalSnapshot(pointerX: 200.5),
            Harness.physicalSnapshot(pointerX: 200.5),
            Harness.physicalSnapshot(pointerX: 200.5),
            Harness.physicalSnapshot(
                pointerX: 200.5,
                changes: [(.leftMouseDown, 2), (.leftMouseUp, 2)]
            ),
        ]

        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies()
        )

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "click_postcondition_not_declared")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertEqual(harness.sleepCalls, [0.012])
    }

    func testOfficialClickButtonsMapToExactQuartzEventsAndButtons() {
        let expected: [(String, CGMouseButton, CGEventType, CGEventType)] = [
            ("left", .left, .leftMouseDown, .leftMouseUp),
            ("right", .right, .rightMouseDown, .rightMouseUp),
            ("wheel", .center, .otherMouseDown, .otherMouseUp),
            ("back", CGMouseButton(rawValue: 3)!, .otherMouseDown, .otherMouseUp),
            ("forward", CGMouseButton(rawValue: 4)!, .otherMouseDown, .otherMouseUp),
        ]
        for (name, button, down, up) in expected {
            XCTAssertEqual(coordinateMouseButtonMappingV1(name)?.button, button)
            XCTAssertEqual(coordinateMouseButtonMappingV1(name)?.downType, down)
            XCTAssertEqual(coordinateMouseButtonMappingV1(name)?.upType, up)
        }
        XCTAssertNil(coordinateMouseButtonMappingV1("middle"))
    }

    func testEveryExtraOfficialClickButtonRunsThroughStrictEngine() throws {
        for button in ["wheel", "back", "forward"] {
            let harness = Harness()
            harness.physicalInputSnapshots = [
                Harness.physicalSnapshot(pointerX: 100),
                Harness.physicalSnapshot(
                    pointerX: 200.5, changes: [(.mouseMoved, 1)]),
                Harness.physicalSnapshot(
                    pointerX: 200.5, changes: [(.mouseMoved, 1)]),
                Harness.physicalSnapshot(
                    pointerX: 200.5, changes: [(.mouseMoved, 1)]),
                Harness.physicalSnapshot(
                    pointerX: 200.5,
                    changes: [
                        (.mouseMoved, 1),
                        (.otherMouseDown, 2),
                        (.otherMouseUp, 2),
                    ]),
            ]
            let result = runCoordinateMouseEvent(
                request: try request(
                    "coordinate_mouse_event.request.click.v1.json",
                    button: button),
                dependencies: harness.dependencies())

            XCTAssertEqual(result.status, "completed_unverified", button)
            XCTAssertTrue(result.primaryActionCommitted, button)
            XCTAssertEqual(harness.events.first, "prepare:\(button):2")
            XCTAssertEqual(harness.postCount, 1)
        }
    }

    func testMoveCommitsOnlyAfterAuthorityChecksAndVerifiesEndpoint() throws {
        let harness = Harness()
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(result.pointerEndpoint?.verified, true)
        XCTAssertEqual(harness.moveCount, 1)
        XCTAssertEqual(harness.prepareCount, 0)
        XCTAssertEqual(harness.postCount, 0)
        XCTAssertEqual(harness.topologyCalls, 1)
    }

    func testMoveYieldsToPhysicalInputDuringSettleWindow() throws {
        let harness = Harness()
        harness.physicalInputSnapshots = [
            Harness.physicalSnapshot(pointerX: 100),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 201.5, changes: [(.mouseMoved, 1)],
                externalChanges: [(.mouseMoved, 1)]),
        ]

        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(harness.sleepCalls, [0.012])
    }

    func testMoveFailsClosedWhenPhysicalInputObservationIsUnavailable() throws {
        let harness = Harness()
        harness.physicalInputSnapshots = [nil]

        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "interference_detection_unavailable")
        XCTAssertFalse(result.pointerMotionCommitted)
        XCTAssertEqual(harness.moveCount, 0)
    }

    func testMoveYieldsBeforeSideEffectWhenPhysicalModifierIsAlreadyHeld() throws {
        let harness = Harness()
        harness.physicalInputSnapshots = [
            .init(
                pointer: .init(x: 100, y: 300.5),
                hidEventCounters: [10],
                heldModifierFlags: 1),
        ]

        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertFalse(result.pointerMotionCommitted)
        XCTAssertEqual(harness.moveCount, 0)
    }

    func testClickYieldsWhenPhysicalInputCompetesWithHeldMouseSequence() throws {
        let harness = Harness()
        harness.physicalInputSnapshots = [
            Harness.physicalSnapshot(pointerX: 100),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 200.5,
                changes: [(.mouseMoved, 1), (.leftMouseDown, 2), (.leftMouseUp, 2)],
                externalChanges: [(.mouseMoved, 1)]),
        ]

        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(harness.postCount, 1)
    }

    func testMoveFailsClosedWhenAnotherApplicationWindowCoversTargetPoint() throws {
        let harness = Harness()
        harness.frontmostWindowIDs = [9001]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "point_occluded")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertFalse(result.pointerMotionCommitted)
        XCTAssertEqual(harness.moveCount, 0)
        XCTAssertEqual(harness.prepareCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testClickFailsClosedWhenSameApplicationSheetCoversPointBeforeMove() throws {
        let harness = Harness()
        // The target is initially hittable, then a same-app sheet (a distinct
        // CGWindow) covers the point while click events are being prepared.
        harness.frontmostWindowIDs = [7001, 7002]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "point_occluded")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertFalse(result.pointerMotionCommitted)
        XCTAssertEqual(harness.prepareCount, 1)
        XCTAssertEqual(harness.moveCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testStaleTopologyFailsBeforeAnyPointerOrClickSideEffect() throws {
        let harness = Harness()
        harness.topologies = [Harness.topology(generation: 8)]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "stale_topology")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertFalse(result.pointerMotionCommitted)
        XCTAssertFalse(result.retrySafe)
        XCTAssertEqual(harness.prepareCount, 0)
        XCTAssertEqual(harness.moveCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testClickPreparesBeforeMovingThenRevalidatesBeforePosting() throws {
        let harness = Harness()
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "click_postcondition_not_declared")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(
            harness.events,
            [
                "prepare:left:2", "move", "observe_pointer", "post",
                "observe_pointer",
            ])
        XCTAssertEqual(harness.topologyCalls, 3)
    }

    func testConsequentialRiskClickReverifiesExactAXAuthorityImmediatelyBeforePost() throws {
        let harness = Harness()
        harness.configureRiskClickSnapshots()
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.risk_click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "click_postcondition_not_declared")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertEqual(harness.riskVerificationCalls, 1)
        XCTAssertEqual(harness.riskVerificationTimeouts, [0.1])
        XCTAssertEqual(harness.postCount, 1)
        XCTAssertEqual(
            harness.events,
            [
                "prepare:left:1", "move", "observe_pointer", "verify_risk",
                "post", "observe_pointer",
            ])
    }

    func testConsequentialRiskClickFailsClosedOnDestinationOrHitTargetDrift() throws {
        for (name, outcome) in [
            ("destination", CoordinateMouseRiskVerificationOutcomeV1.drift("risk_destination_drift")),
            ("hit target", CoordinateMouseRiskVerificationOutcomeV1.drift("risk_hit_target_drift")),
        ] {
            let harness = Harness()
            harness.configureRiskClickSnapshots()
            harness.riskVerificationOutcome = outcome
            let result = runCoordinateMouseEvent(
                request: try request("coordinate_mouse_event.request.risk_click.v1.json"),
                dependencies: harness.dependencies())

            XCTAssertEqual(result.status, "failed", name)
            if case let .drift(code) = outcome {
                XCTAssertEqual(result.failureCode, code, name)
            }
            XCTAssertTrue(result.pointerMotionCommitted, name)
            XCTAssertFalse(result.primaryActionCommitted, name)
            XCTAssertEqual(harness.riskVerificationCalls, 1, name)
            XCTAssertEqual(harness.postCount, 0, name)
        }
    }

    func testConsequentialRiskClickFailsClosedWhenAXVerificationIsUnavailable() throws {
        let harness = Harness()
        harness.configureRiskClickSnapshots()
        harness.riskVerificationOutcome = .unavailable("risk_hit_target_unavailable")
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.risk_click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "risk_hit_target_unavailable")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testConsequentialRiskClickYieldsToPhysicalInputDuringAXVerification() throws {
        let harness = Harness()
        harness.configureRiskClickSnapshots(
            afterVerificationExternalChanges: [(.mouseMoved, 1)])
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.risk_click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(harness.riskVerificationCalls, 1)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testConsequentialRiskClickDoesNotPostWhenDeadlineExpiresDuringAXVerification() throws {
        let harness = Harness()
        harness.configureRiskClickSnapshots()
        harness.nowValues = [
            coordinateMouseTestDate("2026-07-22T12:03:30Z"),
            coordinateMouseTestDate("2026-07-22T12:03:30Z"),
            coordinateMouseTestDate("2026-07-22T12:03:30Z"),
            coordinateMouseTestDate("2026-07-22T12:03:30Z"),
            coordinateMouseTestDate("2026-07-22T12:03:30.500Z"),
        ]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.risk_click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "request_expired_before_click")
        XCTAssertEqual(harness.riskVerificationCalls, 1)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testClickGateBlockAfterPointerMoveDoesNotClaimPrimaryCommit() throws {
        let harness = Harness()
        harness.postOutcome = .notCommitted
        harness.physicalInputSnapshots = [
            Harness.physicalSnapshot(pointerX: 100),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Harness.physicalSnapshot(
                pointerX: 200.5, changes: [(.mouseMoved, 1)]),
        ]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "input_commit_blocked")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(result.phase, "action")
    }

    func testInterruptedMultiClickTruthfullyReportsPartialPrimaryCommit() throws {
        let harness = Harness()
        harness.postOutcome = .partiallyCommitted
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "input_sequence_interrupted_after_commit")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
    }

    func testClickDoesNotPostWhenWindowChangesAfterPointerMove() throws {
        let harness = Harness()
        harness.windows = [
            Harness.window(),
            Harness.window(),
            Harness.window(bounds: .init(x: 101, y: 100, width: 800, height: 600)),
        ]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "target_changed_before_click")
        XCTAssertEqual(result.phase, "action")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(harness.prepareCount, 1)
        XCTAssertEqual(harness.moveCount, 1)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testClickEndpointFailureReportsPointerSideEffectButNoPrimaryCommit() throws {
        let harness = Harness()
        harness.moveOutcome = .committed(
            observed: .init(x: 210.5, y: 300.5),
            expectedSyntheticEventCount: 1
        )
        harness.observedPointerValues = [.init(x: 210.5, y: 300.5)]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "pointer_endpoint_not_verified")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(result.pointerEndpoint?.verified, false)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testClickEndpointIsReobservedAfterPhysicalInputSettles() throws {
        let harness = Harness()
        harness.moveOutcome = .committed(
            observed: .init(x: 210.5, y: 300.5),
            expectedSyntheticEventCount: 1
        )
        harness.observedPointerValues = [.init(x: 200.5, y: 300.5)]

        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "click_postcondition_not_declared")
        XCTAssertTrue(result.pointerEndpoint?.verified ?? false)
        XCTAssertEqual(harness.postCount, 1)
    }

    func testMoveEndpointFailureIsCommittedButUnverified() throws {
        let harness = Harness()
        harness.moveOutcome = .committed(
            observed: nil,
            expectedSyntheticEventCount: 1
        )
        harness.observedPointerValues = [.init(x: 210.5, y: 300.5)]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "pointer_endpoint_not_verified")
        XCTAssertTrue(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertFalse(result.retrySafe)
    }

    func testClickPreparationFailureNeverMovesPointer() throws {
        let harness = Harness()
        harness.prepareSucceeds = false
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.failureCode, "event_preparation_failed")
        XCTAssertEqual(harness.prepareCount, 1)
        XCTAssertEqual(harness.moveCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testDeadlineExpiryAndExcessiveHorizonFailBeforeSideEffects() throws {
        let expired = Harness()
        expired.nowValues = [coordinateMouseTestDate("2026-07-22T12:03:30.500Z")]
        let expiredResult = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: expired.dependencies())
        XCTAssertEqual(expiredResult.failureCode, "request_expired")
        XCTAssertEqual(expired.moveCount, 0)

        let tooEarly = Harness()
        tooEarly.nowValues = [coordinateMouseTestDate("2026-07-22T12:03:20Z")]
        let horizonResult = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.move.v1.json"),
            dependencies: tooEarly.dependencies())
        XCTAssertEqual(horizonResult.failureCode, "invalid_request")
        XCTAssertEqual(tooEarly.moveCount, 0)
    }

    func testClickDeadlineCanExpireAfterPointerMoveButBeforePost() throws {
        let harness = Harness()
        harness.nowValues = [
            coordinateMouseTestDate("2026-07-22T12:03:30Z"),
            coordinateMouseTestDate("2026-07-22T12:03:30.100Z"),
            coordinateMouseTestDate("2026-07-22T12:03:30.500Z"),
        ]
        let result = runCoordinateMouseEvent(
            request: try request("coordinate_mouse_event.request.click.v1.json"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.failureCode, "request_expired_before_click")
        XCTAssertEqual(result.phase, "action")
        XCTAssertFalse(result.primaryActionCommitted)
        XCTAssertTrue(result.pointerMotionCommitted)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testEveryAuthorityFailureIsFailClosed() throws {
        let cases: [(String, (Harness) -> Void)] = [
            ("helper_boot_mismatch", { $0.topologies = [Harness.topology(helperBootID: "other")] }),
            ("process_not_live", { $0.pidLive = false }),
            ("process_identity_mismatch", { $0.bundleID = "other.bundle" }),
            ("window_not_found", { $0.windows = [nil] }),
            ("window_identity_mismatch", { $0.windows = [Harness.window(ownerPID: 999)] }),
            ("window_not_actionable", { $0.windows = [Harness.window(isOnScreen: false)] }),
            ("window_bounds_mismatch", { $0.windows = [Harness.window(bounds: .init(x: 101, y: 100, width: 800, height: 600))] }),
            ("display_not_found", { $0.topologies = [Harness.topology(displayID: 2)] }),
            ("display_not_actionable", { $0.topologies = [Harness.topology(isAsleep: true)] }),
        ]
        for (code, configure) in cases {
            let harness = Harness()
            configure(harness)
            let result = runCoordinateMouseEvent(
                request: try request("coordinate_mouse_event.request.move.v1.json"),
                dependencies: harness.dependencies())
            XCTAssertEqual(result.failureCode, code)
            XCTAssertEqual(harness.moveCount, 0, code)
            XCTAssertEqual(harness.postCount, 0, code)
            XCTAssertFalse(result.retrySafe)
        }
    }

    func testNewEngineDoesNotDelegateToGenericMouseEvent() throws {
        let sourceURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Sources/CoordinateMouseEvent.swift")
        let source = try String(contentsOf: sourceURL)
        XCTAssertFalse(source.contains("InputDriver.mouseEvent"))
        XCTAssertFalse(source.contains("\"mouse_event\""))
    }

    private func request(_ name: String) throws -> CoordinateMouseEventRequestV1 {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("testdata/\(name)")
        return try decodeCoordinateMouseEventRPCRequestV1(Data(contentsOf: url)).params
    }

    private func request(
        _ name: String,
        button: String
    ) throws -> CoordinateMouseEventRequestV1 {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("testdata/\(name)")
        let data = try Data(contentsOf: url)
        var root = try XCTUnwrap(
            JSONSerialization.jsonObject(with: data) as? [String: Any])
        var params = try XCTUnwrap(root["params"] as? [String: Any])
        params["button"] = button
        root["params"] = params
        return try decodeCoordinateMouseEventRPCRequestV1(
            JSONSerialization.data(withJSONObject: root)).params
    }

}

private final class Harness {
    static let now = coordinateMouseTestDate("2026-07-22T12:03:30Z")

    var topologies = [topology()]
    var windows: [CaptureCoordinateWindowWindowSnapshot?] = [window()]
    var frontmostWindowIDs: [UInt32?] = [7001]
    var nowValues = [now, now, now, now]
    var pidLive = true
    var bundleID: String? = "com.example.fixture"
    var prepareSucceeds = true
    var postOutcome: CoordinateMousePreparedClick.PostOutcome = .committed
    var moveOutcome = CoordinateMousePointerMoveOutcome.committed(
        observed: CoordinateMouseEventPointV1(x: 200.5, y: 300.5),
        expectedSyntheticEventCount: 1)
    var observedPointerValues = [
        CoordinateMouseEventPointV1(x: 200.5, y: 300.5),
    ]
    var riskVerificationOutcome = CoordinateMouseRiskVerificationOutcomeV1.matched

    var topologyCalls = 0
    var windowCalls = 0
    var frontmostWindowCalls = 0
    var nowCalls = 0
    var prepareCount = 0
    var moveCount = 0
    var postCount = 0
    var physicalInputCalls = 0
    var riskVerificationCalls = 0
    var riskVerificationTimeouts: [TimeInterval] = []
    var observedPointerCalls = 0
    var physicalInputSnapshots: [PhysicalInputInterferenceSnapshotV1?] = [
        physicalSnapshot(pointerX: 100),
        physicalSnapshot(pointerX: 200.5, changes: [(.mouseMoved, 1)]),
        physicalSnapshot(pointerX: 200.5, changes: [(.mouseMoved, 1)]),
        physicalSnapshot(pointerX: 200.5, changes: [(.mouseMoved, 1)]),
        physicalSnapshot(
            pointerX: 200.5,
            changes: [(.mouseMoved, 1), (.leftMouseDown, 2), (.leftMouseUp, 2)]),
    ]
    var sleepCalls: [TimeInterval] = []
    var events: [String] = []

    func dependencies() -> CoordinateMouseEventDependencies {
        CoordinateMouseEventDependencies(
            observeTopology: {
                let value = self.topologies[min(self.topologyCalls, self.topologies.count - 1)]
                self.topologyCalls += 1
                return value
            },
            isPIDLive: { _ in self.pidLive },
            bundleIDForPID: { _ in self.bundleID },
            exactWindow: { _ in
                let value = self.windows[min(self.windowCalls, self.windows.count - 1)]
                self.windowCalls += 1
                return value
            },
            frontmostWindowAtPoint: { _ in
                let value = self.frontmostWindowIDs[
                    min(self.frontmostWindowCalls, self.frontmostWindowIDs.count - 1)]
                self.frontmostWindowCalls += 1
                return value
            },
            prepareClick: { _, button, clicks in
                self.prepareCount += 1
                self.events.append("prepare:\(button):\(clicks)")
                guard self.prepareSucceeds else { return nil }
                return CoordinateMousePreparedClick {
                    self.postCount += 1
                    self.events.append("post")
                    return self.postOutcome
                }
            },
            movePointer: { _ in
                self.moveCount += 1
                self.events.append("move")
                return self.moveOutcome
            },
            observePointer: {
                self.events.append("observe_pointer")
                let value = self.observedPointerValues[
                    min(
                        self.observedPointerCalls,
                        self.observedPointerValues.count - 1
                    )
                ]
                self.observedPointerCalls += 1
                return value
            },
            observePhysicalInput: {
                let value = self.physicalInputSnapshots[
                    min(self.physicalInputCalls, self.physicalInputSnapshots.count - 1)]
                self.physicalInputCalls += 1
                return value
            },
            verifyRiskTarget: { _, _, timeout in
                self.riskVerificationCalls += 1
                self.riskVerificationTimeouts.append(timeout)
                self.events.append("verify_risk")
                return self.riskVerificationOutcome
            },
            sleep: { self.sleepCalls.append($0) },
            now: {
                let value = self.nowValues[min(self.nowCalls, self.nowValues.count - 1)]
                self.nowCalls += 1
                return value
            })
    }

    func configureRiskClickSnapshots(
        afterVerificationExternalChanges: [(CGEventType, UInt32)] = []
    ) {
        physicalInputSnapshots = [
            Self.physicalSnapshot(pointerX: 100),
            Self.physicalSnapshot(pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Self.physicalSnapshot(pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Self.physicalSnapshot(pointerX: 200.5, changes: [(.mouseMoved, 1)]),
            Self.physicalSnapshot(
                pointerX: 200.5,
                changes: [(.mouseMoved, 1)],
                externalChanges: afterVerificationExternalChanges),
            Self.physicalSnapshot(
                pointerX: 200.5,
                changes: [(.mouseMoved, 1), (.leftMouseDown, 1), (.leftMouseUp, 1)]),
        ]
    }

    static func physicalSnapshot(
        pointerX: Double,
        changes: [(CGEventType, UInt32)] = [],
        externalChanges: [(CGEventType, UInt32)] = []
    ) -> PhysicalInputInterferenceSnapshotV1 {
        var counters = Array(
            repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        var syntheticCounters = Array(
            repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count)
        for (eventType, delta) in changes {
            let index = physicalInputHIDEventTypesV1.firstIndex(of: eventType)!
            counters[index] += delta
            syntheticCounters[index] += delta
        }
        for (eventType, delta) in externalChanges {
            counters[physicalInputHIDEventTypesV1.firstIndex(of: eventType)!] += delta
        }
        return .init(
            pointer: .init(x: pointerX, y: 300.5),
            hidEventCounters: counters,
            syntheticEventCounters: syntheticCounters)
    }

    static func topology(
        generation: UInt64 = 7,
        helperBootID: String = "helper_boot_demo",
        displayID: UInt32 = 1,
        isAsleep: Bool = false
    ) -> DisplayTopologyV1 {
        DisplayTopologyV1(
            schemaVersion: 1,
            topologyID: "topo_mixed_001",
            helperBootID: helperBootID,
            generation: generation,
            capturedAt: "2026-07-22T12:03:00Z",
            mainDisplayID: displayID,
            displays: [.init(
                displayID: displayID,
                isMain: true,
                isBuiltin: true,
                isActive: true,
                isOnline: true,
                isAsleep: isAsleep,
                quartzBounds: .init(x: 0, y: 0, width: 1280, height: 800),
                appKitFrame: .init(x: 0, y: 0, width: 1280, height: 800),
                appKitVisibleFrame: .init(x: 0, y: 25, width: 1280, height: 750),
                backingScaleFactor: 2,
                pixelWidth: 2560,
                pixelHeight: 1600,
                rotationDegrees: 0,
                mirrorMasterDisplayID: nil)])
    }

    static func window(
        ownerPID: Int = 4242,
        isOnScreen: Bool = true,
        bounds: DisplayTopologyRectV1 = .init(x: 100, y: 100, width: 800, height: 600)
    ) -> CaptureCoordinateWindowWindowSnapshot {
        .init(
            windowID: 7001,
            ownerPID: ownerPID,
            layer: 0,
            isOnScreen: isOnScreen,
            bounds: bounds)
    }
}

private func coordinateMouseTestDate(_ value: String) -> Date {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: value) {
        return date
    }
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.date(from: value)!
}
