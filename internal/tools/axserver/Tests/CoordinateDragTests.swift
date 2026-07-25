import CoreGraphics
import XCTest
@testable import ax_server

final class CoordinateDragTests: XCTestCase {
    func testMinimumJerkPathIsBoundedAndIncludesExactEndpoints() {
        let path = coordinateDragMinimumJerkPath(
            start: .init(x: 10, y: 20), end: .init(x: 110, y: 70), durationMS: 240)
        XCTAssertEqual(path.first, .init(x: 10, y: 20))
        XCTAssertEqual(path.last, .init(x: 110, y: 70))
        XCTAssertGreaterThanOrEqual(path.count, 8)
        XCTAssertLessThanOrEqual(path.count, 48)
        XCTAssertTrue(path.allSatisfy { (10...110).contains($0.x) && (20...70).contains($0.y) })
    }

    func testPolylinePathPreservesEveryProviderWaypointWithinBoundedSamples() {
        let waypoints = [
            CoordinateMouseEventPointV1(x: 10, y: 20),
            CoordinateMouseEventPointV1(x: 60, y: 90),
            CoordinateMouseEventPointV1(x: 110, y: 70),
        ]
        let path = coordinateDragMinimumJerkPolylinePath(
            waypoints: waypoints, durationMS: 240)
        XCTAssertEqual(path.first, waypoints.first)
        XCTAssertEqual(path.last, waypoints.last)
        XCTAssertLessThanOrEqual(path.count, 48)
        for waypoint in waypoints {
            XCTAssertTrue(path.contains(waypoint), "lost provider waypoint \(waypoint)")
        }
    }

    func testPreflightOcclusionNeverPreparesOrPostsEvents() throws {
        for (name, ids, code) in [
            ("start", [9999, 7001], "start_point_occluded"),
            ("end", [7001, 9999], "end_point_occluded"),
        ] {
            let harness = DragHarness()
            harness.frontmostWindowIDs = ids.map { UInt32($0) }
            let result = runCoordinateDrag(
                request: try dragRequest(), dependencies: harness.dependencies())
            XCTAssertEqual(result.status, "failed", name)
            XCTAssertEqual(result.failureCode, code, name)
            XCTAssertEqual(harness.prepareCount, 0, name)
            XCTAssertEqual(harness.posts, [], name)
            XCTAssertFalse(result.retrySafe)
        }
    }

    func testAllNormalEventsArePreparedBeforeFirstSideEffect() throws {
        let harness = DragHarness()
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "drop_postcondition_not_declared")
        XCTAssertNil(result.postcondition)
        XCTAssertTrue(result.dragCommitted)
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertEqual(harness.events.first, "prepare")
        XCTAssertEqual(harness.prepareCount, 1)
        XCTAssertEqual(harness.posts.first, "down")
        XCTAssertEqual(harness.posts.last, "up@500.5,300.5")
        XCTAssertFalse(harness.modifierPosted)
    }

    func testCancellationAfterMouseDownAlwaysPostsMouseUpAtObservedPointer() throws {
        let harness = DragHarness()
        harness.cancelValues = [false, true]
        harness.observedPointers = [.init(x: 200.5, y: 300.5), .init(x: 245, y: 304)]
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "cancelled_during_drag")
        XCTAssertTrue(result.mouseDownCommitted)
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertTrue(result.possibleDropSideEffect)
        XCTAssertEqual(harness.posts.last, "up@245.0,304.0")
        XCTAssertFalse(harness.buttonDown)
        XCTAssertFalse(harness.modifierPosted)
        XCTAssertFalse(result.retrySafe)
    }

    func testCancellationDuringPolylineAlwaysPostsMouseUpAfterMultipleSamples() throws {
        let harness = DragHarness()
        harness.cancelValues = Array(repeating: false, count: 10) + [true]
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "cancelled_during_drag")
        XCTAssertGreaterThan(harness.dragPosts, 1)
        XCTAssertTrue(result.mouseDownCommitted)
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertFalse(harness.buttonDown)
        XCTAssertTrue(harness.posts.last?.hasPrefix("up@") == true)
    }

    func testDeadlineAfterMouseDownAlwaysCleansUpAtCurrentPointer() throws {
        let harness = DragHarness()
        harness.nowValues = [
            dragTestDate("2026-07-22T12:03:30Z"),
            dragTestDate("2026-07-22T12:03:30Z"),
            dragTestDate("2026-07-22T12:03:32Z"),
        ]
        harness.observedPointers = [.init(x: 200.5, y: 300.5), .init(x: 225, y: 302)]
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "request_expired_during_drag")
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertEqual(harness.posts.last, "up@225.0,302.0")
        XCTAssertFalse(harness.buttonDown)
    }

    func testPointerInterferenceAfterDownAlwaysCleansUp() throws {
        let harness = DragHarness()
        harness.observedPointerOffsetAfterFirstDrag = .init(x: 20, y: 0)
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "pointer_interference")
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertTrue(result.possibleDropSideEffect)
        XCTAssertFalse(harness.buttonDown)
    }

    func testPhysicalInputBetweenSamplesReleasesBeforeOverwritingUserPointer() throws {
        let harness = DragHarness()
        let path = DragHarness.defaultPreparedPath()
        harness.physicalInputSnapshots = [
            DragHarness.physicalSnapshot(pointer: path[0]),
            DragHarness.physicalSnapshot(
                pointer: path[0], changes: [(.leftMouseDown, 1)], heldButtons: 1),
            DragHarness.physicalSnapshot(
                pointer: path[0], changes: [(.leftMouseDown, 1)], heldButtons: 1),
            DragHarness.physicalSnapshot(
                pointer: path[1],
                changes: [(.leftMouseDown, 1), (.leftMouseDragged, 1)],
                heldButtons: 1),
            DragHarness.physicalSnapshot(
                pointer: .init(x: path[1].x + 12, y: path[1].y),
                changes: [
                    (.leftMouseDown, 1), (.leftMouseDragged, 1),
                ],
                externalChanges: [(.mouseMoved, 1)],
                heldButtons: 1),
        ]

        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertEqual(harness.dragPosts, 1)
        XCTAssertEqual(harness.posts.last, "up@\(path[1].x),\(path[1].y)")
        XCTAssertFalse(harness.buttonDown)
    }

    func testPhysicallyHeldMouseButtonYieldsBeforeMouseDown() throws {
        let harness = DragHarness()
        let counters = Array(
            repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        harness.physicalInputSnapshots = [
            .init(
                pointer: .init(x: 200.5, y: 300.5),
                hidEventCounters: counters,
                syntheticEventCounters: Array(
                    repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count),
                heldMouseButtons: 1),
        ]

        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.phase, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertFalse(result.dragCommitted)
        XCTAssertFalse(result.mouseDownCommitted)
        XCTAssertFalse(result.possibleDropSideEffect)
        XCTAssertNil(result.pointerEndpoint)
        XCTAssertEqual(harness.posts, [])
    }

    func testDragFailsClosedBeforeMouseDownWhenInterferenceObservationUnavailable() throws {
        let harness = DragHarness()
        harness.physicalInputSnapshots = [nil]

        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "interference_detection_unavailable")
        XCTAssertEqual(harness.posts, [])
    }

    func testCleanupRetriesPrecreatedMouseUpWithoutPostingModifiers() throws {
        let harness = DragHarness()
        harness.cancelValues = [false, true]
        harness.upResults = [false, true]
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertEqual(harness.posts.filter { $0.hasPrefix("up@") }.count, 2)
        XCTAssertFalse(harness.buttonDown)
        XCTAssertFalse(harness.modifierPosted)
    }

    func testUnconfirmedMouseUpUsesStrictCompletedUnverifiedShape() throws {
        let harness = DragHarness()
        harness.cancelValues = [false, true]
        harness.upResults = [false, false, false]
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "mouse_up_post_unverified")
        XCTAssertFalse(result.mouseUpCommitted)
        XCTAssertEqual(harness.posts.filter { $0.hasPrefix("up@") }.count, 3)
        XCTAssertFalse(result.retrySafe)
    }

    func testProductionCancellationMarkerIsRequestScopedAndObservable() throws {
        let url = coordinateDragCancellationMarkerURL(
            requestID: 901, helperBootID: "helper_boot_demo")
        defer { try? FileManager.default.removeItem(at: url) }
        try? FileManager.default.removeItem(at: url)
        let dependencies = productionCoordinateDragDependencies(
            requestID: 901, helperBootID: "helper_boot_demo")
        XCTAssertFalse(dependencies.isCancelled())
        try Data("cancel\n".utf8).write(to: url, options: .atomic)
        XCTAssertTrue(dependencies.isCancelled())
    }

    func testEndTargetChangedAfterDownReleasesWithoutDroppingAtRequestedEnd() throws {
        let harness = DragHarness()
        harness.frontmostWindowIDs = [7001, 7001, 7001, 9999]
        let result = runCoordinateDrag(
            request: try dragRequest(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "end_target_changed")
        XCTAssertTrue(result.mouseUpCommitted)
        XCTAssertFalse(harness.buttonDown)
    }

    func testDropTimeFullAuthorityChangesCleanUpAtCurrentPointer() throws {
        let scenarios: [(String, String, Int, (DragHarness) -> Void)] = [
            ("topology generation", "stale_topology", 2, { harness in
                harness.topologies = [
                    DragHarness.topology(), DragHarness.topology(),
                    DragHarness.topology(generation: 8),
                ]
            }),
            ("exact window bounds", "window_bounds_mismatch", 3, { harness in
                harness.windows = [
                    DragHarness.window(), DragHarness.window(),
                    DragHarness.window(
                        bounds: .init(x: 101, y: 100, width: 800, height: 600)),
                ]
            }),
            ("display actionability", "start_display_not_actionable", 3, { harness in
                harness.topologies = [
                    DragHarness.topology(), DragHarness.topology(),
                    DragHarness.topology(isAsleep: true),
                ]
            }),
        ]
        for (name, failureCode, expectedWindowCalls, mutate) in scenarios {
            let harness = DragHarness()
            mutate(harness)
            let result = runCoordinateDrag(
                request: try dragRequest(), dependencies: harness.dependencies())
            XCTAssertEqual(result.status, "user_interference", name)
            XCTAssertEqual(result.failureCode, failureCode, name)
            XCTAssertTrue(result.dragCommitted, name)
            XCTAssertTrue(result.mouseDownCommitted, name)
            XCTAssertTrue(result.mouseUpCommitted, name)
            XCTAssertTrue(result.possibleDropSideEffect, name)
            XCTAssertEqual(result.phase, "cleanup", name)
            XCTAssertEqual(harness.topologyCalls, 3, name)
            XCTAssertEqual(harness.windowCalls, expectedWindowCalls, name)
            XCTAssertEqual(harness.posts.last, "up@500.5,300.5", name)
            XCTAssertFalse(harness.buttonDown, name)
            XCTAssertFalse(result.retrySafe, name)
            let encoded = try JSONEncoder().encode(result)
            XCTAssertNoThrow(try decodeCoordinateDragResultV1(encoded), name)
        }
    }

    private func dragRequest() throws -> CoordinateDragRequestV1 {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent(
                "testdata/coordinate_drag.request.v1.json")
        return try decodeCoordinateDragRPCRequestV1(Data(contentsOf: url)).params
    }
}

private final class DragHarness {
    var topologies = [DragHarness.topology()]
    var windows: [CaptureCoordinateWindowWindowSnapshot?] = [DragHarness.window()]
    var frontmostWindowIDs: [UInt32?] = [7001, 7001, 7001, 7001]
    var nowValues = Array(repeating: dragTestDate("2026-07-22T12:03:30Z"), count: 100)
    var cancelValues = Array(repeating: false, count: 100)
    var observedPointers: [CoordinateMouseEventPointV1] = []
    var observedPointerOffsetAfterFirstDrag: CoordinateMouseEventPointV1?
    var prepareCount = 0
    var preparedPath: [CoordinateMouseEventPointV1]?
    var topologyCalls = 0
    var windowCalls = 0
    var frontmostCalls = 0
    var nowCalls = 0
    var cancelCalls = 0
    var observeCalls = 0
    var dragPosts = 0
    var posts: [String] = []
    var events: [String] = []
    var buttonDown = false
    var modifierPosted = false
    var upResults = [true]
    var upCalls = 0
    var physicalInputCalls = 0
    var physicalInputSnapshots: [PhysicalInputInterferenceSnapshotV1?] = []

    func dependencies() -> CoordinateDragDependencies {
        CoordinateDragDependencies(
            observeTopology: {
                defer { self.topologyCalls += 1 }
                return self.topologies[min(self.topologyCalls, self.topologies.count - 1)]
            },
            isPIDLive: { _ in true },
            bundleIDForPID: { _ in "com.example.fixture" },
            exactWindow: { _ in
                defer { self.windowCalls += 1 }
                return self.windows[min(self.windowCalls, self.windows.count - 1)]
            },
            frontmostWindowAtPoint: { _ in
                defer { self.frontmostCalls += 1 }
                return self.frontmostWindowIDs[
                    min(self.frontmostCalls, self.frontmostWindowIDs.count - 1)]
            },
            prepare: { path, _ in
                self.prepareCount += 1
                self.events.append("prepare")
                self.preparedPath = path
                return CoordinateDragPreparedSequence(
                    path: path,
                    postDown: {
                        self.posts.append("down")
                        self.buttonDown = true
                        return true
                    },
                    postDrag: { index in
                        self.dragPosts += 1
                        self.posts.append("drag:\(index)")
                        return true
                    },
                    postUp: { point in
                        self.posts.append("up@\(point.x),\(point.y)")
                        let result = self.upResults[min(self.upCalls, self.upResults.count - 1)]
                        self.upCalls += 1
                        if result { self.buttonDown = false }
                        return result
                    })
            },
            observePointer: {
                defer { self.observeCalls += 1 }
                if self.observeCalls < self.observedPointers.count {
                    return self.observedPointers[self.observeCalls]
                }
                let path = self.preparedPath ?? Self.defaultPreparedPath()
                let index = min(max(self.dragPosts, 0), path.count - 1)
                var value = path[index]
                if self.dragPosts == 1, let offset = self.observedPointerOffsetAfterFirstDrag {
                    value = .init(x: value.x + offset.x, y: value.y + offset.y)
                }
                return value
            },
            observePhysicalInput: {
                defer { self.physicalInputCalls += 1 }
                if !self.physicalInputSnapshots.isEmpty {
                    return self.physicalInputSnapshots[
                        min(self.physicalInputCalls, self.physicalInputSnapshots.count - 1)]
                }
                let path = self.preparedPath ?? Self.defaultPreparedPath()
                var changes: [(CGEventType, UInt32)] = []
                if self.posts.contains("down") { changes.append((.leftMouseDown, 1)) }
                if self.dragPosts > 0 {
                    changes.append((.leftMouseDragged, UInt32(self.dragPosts)))
                }
                let upCount = self.posts.filter { $0.hasPrefix("up@") }.count
                if upCount > 0 { changes.append((.leftMouseUp, UInt32(upCount))) }
                return Self.physicalSnapshot(
                    pointer: path[min(self.dragPosts, path.count - 1)],
                    changes: changes,
                    heldButtons: self.buttonDown ? 1 : 0)
            },
            isCancelled: {
                defer { self.cancelCalls += 1 }
                return self.cancelValues[min(self.cancelCalls, self.cancelValues.count - 1)]
            },
            now: {
                defer { self.nowCalls += 1 }
                return self.nowValues[min(self.nowCalls, self.nowValues.count - 1)]
            },
            sleep: { _ in })
    }

    static func physicalSnapshot(
        pointer: CoordinateMouseEventPointV1,
        changes: [(CGEventType, UInt32)] = [],
        externalChanges: [(CGEventType, UInt32)] = [],
        heldButtons: UInt32 = 0
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
            pointer: pointer,
            hidEventCounters: counters,
            syntheticEventCounters: syntheticCounters,
            heldMouseButtons: heldButtons,
            syntheticHeldMouseButtons: heldButtons)
    }

    static func defaultPreparedPath() -> [CoordinateMouseEventPointV1] {
        coordinateDragMinimumJerkPolylinePath(
            waypoints: [
                .init(x: 200.5, y: 300.5),
                .init(x: 350.5, y: 450.5),
                .init(x: 500.5, y: 300.5),
            ],
            durationMS: 240)
    }

    static func topology(
        generation: UInt64 = 7,
        isAsleep: Bool = false
    ) -> DisplayTopologyV1 {
        DisplayTopologyV1(
            schemaVersion: 1, topologyID: "topo_mixed_001",
            helperBootID: "helper_boot_demo", generation: generation,
            capturedAt: "2026-07-22T12:03:00Z", mainDisplayID: 1,
            displays: [.init(
                displayID: 1, isMain: true, isBuiltin: true, isActive: true,
                isOnline: true, isAsleep: isAsleep,
                quartzBounds: .init(x: 0, y: 0, width: 1280, height: 800),
                appKitFrame: .init(x: 0, y: 0, width: 1280, height: 800),
                appKitVisibleFrame: .init(x: 0, y: 25, width: 1280, height: 750),
                backingScaleFactor: 2, pixelWidth: 2560, pixelHeight: 1600,
                rotationDegrees: 0, mirrorMasterDisplayID: nil)])
    }

    static func window(
        bounds: DisplayTopologyRectV1 = .init(x: 100, y: 100, width: 800, height: 600)
    ) -> CaptureCoordinateWindowWindowSnapshot {
        .init(windowID: 7001, ownerPID: 4242, layer: 0, isOnScreen: true,
              bounds: bounds)
    }
}

private func dragTestDate(_ value: String) -> Date {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: value) { return date }
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.date(from: value)!
}
