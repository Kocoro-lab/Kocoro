import Foundation
import XCTest
@testable import ax_server

final class DisplayTopologyLiveTests: XCTestCase {
    func testInjectedBuilderKeepsGenerationForSameStructureAndRefreshesTimestamp() throws {
        let builder = DisplayTopologyObservationBuilder(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live")
        let first = try builder.observe(
            snapshot: Self.snapshot(),
            capturedAt: "2026-07-22T12:00:00.001Z")
        let reordered = DisplayTopologyRawSnapshot(
            mainDisplayID: 1,
            quartzDisplays: Array(Self.snapshot().quartzDisplays.reversed()),
            appKitScreens: Array(Self.snapshot().appKitScreens.reversed()))
        let second = try builder.observe(
            snapshot: reordered,
            capturedAt: "2026-07-22T12:00:00.002Z")

        XCTAssertEqual(first.helperBootID, "helper_boot_live")
        XCTAssertEqual(first.topologyID, "topo_live")
        XCTAssertEqual(first.generation, 1)
        XCTAssertEqual(second.generation, 1)
        XCTAssertNotEqual(first.capturedAt, second.capturedAt)
        XCTAssertEqual(second.displays.map(\.displayID), [1, 2])
    }

    func testInjectedBuilderIncrementsOnlyAfterValidStructuralChange() throws {
        let builder = DisplayTopologyObservationBuilder(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live")
        XCTAssertEqual(try builder.observe(
            snapshot: Self.snapshot(), capturedAt: "2026-07-22T12:00:00.001Z").generation, 1)

        var missing = Self.snapshot()
        missing.appKitScreens.removeLast()
        XCTAssertThrowsError(try builder.observe(
            snapshot: missing, capturedAt: "2026-07-22T12:00:00.002Z"))

        var changed = Self.snapshot()
        changed.appKitScreens[1] = DisplayTopologyAppKitScreenSnapshot(
            displayID: 2,
            frame: DisplayTopologyRectV1(x: -1600, y: -210, width: 1600, height: 900),
            visibleFrame: DisplayTopologyRectV1(x: -1600, y: -210, width: 1600, height: 875),
            backingScaleFactor: 1,
            pixelWidth: 1600,
            pixelHeight: 900)
        XCTAssertEqual(try builder.observe(
            snapshot: changed, capturedAt: "2026-07-22T12:00:00.003Z").generation, 2)

        XCTAssertEqual(try builder.observe(
            snapshot: changed, capturedAt: "2026-07-22T12:00:00.004Z").generation, 2)
    }

    func testInjectedBuilderRejectsMissingAmbiguousAndInconsistentScreens() throws {
        let cases: [DisplayTopologyRawSnapshot] = [
            DisplayTopologyRawSnapshot(
                mainDisplayID: 1,
                quartzDisplays: Self.snapshot().quartzDisplays,
                appKitScreens: [Self.snapshot().appKitScreens[0]]),
            DisplayTopologyRawSnapshot(
                mainDisplayID: 1,
                quartzDisplays: Self.snapshot().quartzDisplays,
                appKitScreens: [
                    Self.snapshot().appKitScreens[0],
                    Self.snapshot().appKitScreens[0],
                    Self.snapshot().appKitScreens[1],
                ]),
            DisplayTopologyRawSnapshot(
                mainDisplayID: 1,
                quartzDisplays: [
                    Self.snapshot().quartzDisplays[0],
                    Self.snapshot().quartzDisplays[0],
                ],
                appKitScreens: Self.snapshot().appKitScreens),
        ]
        for snapshot in cases {
            let builder = DisplayTopologyObservationBuilder(
                helperBootID: "helper_boot_live",
                topologyID: "topo_live")
            XCTAssertThrowsError(try builder.observe(
                snapshot: snapshot,
                capturedAt: "2026-07-22T12:00:00.001Z"))
        }
    }

    func testCollectorRefreshesAppKitBeforeReadingScreens() throws {
        var order: [String] = []
        let snapshot = try collectDisplayTopologyRawSnapshot(
            refresh: { order.append("refresh") },
            readQuartz: {
                order.append("quartz")
                return (mainDisplayID: 1, displays: Self.snapshot().quartzDisplays)
            },
            readAppKit: {
                order.append("appkit")
                return Self.snapshot().appKitScreens
            })
        XCTAssertEqual(order.first, "refresh")
        XCTAssertEqual(order, ["refresh", "quartz", "appkit"])
        XCTAssertEqual(snapshot.mainDisplayID, 1)
    }

    func testLiveServiceKeepsHelperIdentityAndMakesCapturedAtChange() throws {
        let fixedDate = Date(timeIntervalSince1970: 1_753_185_600)
        let service = LiveDisplayTopologyService(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live",
            now: { fixedDate },
            collect: { Self.snapshot() })

        let first = try service.observe()
        let second = try service.observe()
        XCTAssertEqual(first.helperBootID, second.helperBootID)
        XCTAssertEqual(first.topologyID, second.topologyID)
        XCTAssertEqual(first.generation, second.generation)
        XCTAssertNotEqual(first.capturedAt, second.capturedAt)
    }

    func testLiveServicePreparesAppKitBeforeFirstCollection() throws {
        var order: [String] = []
        let service = LiveDisplayTopologyService(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live",
            prepareAppKit: { order.append("prepare") },
            collect: {
                order.append("collect")
                return Self.snapshot()
            })

        XCTAssertEqual(order, ["prepare"])
        _ = try service.observe()
        XCTAssertEqual(order, ["prepare", "collect"])
    }

    func testLiveServiceRetriesTransientDisplaySetMismatch() throws {
        var mismatched = Self.snapshot()
        mismatched.appKitScreens.removeLast()
        var collections = 0
        var settles = 0
        let service = LiveDisplayTopologyService(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live",
            collect: {
                collections += 1
                return collections == 1 ? mismatched : Self.snapshot()
            },
            settleBeforeRetry: { settles += 1 })

        let topology = try service.observe()

        XCTAssertEqual(topology.displays.map(\.displayID), [1, 2])
        XCTAssertEqual(collections, 2)
        XCTAssertEqual(settles, 1)
    }

    func testLiveServiceDefaultSettleProcessesDisplayChangeBeforeRetry() throws {
        var mismatched = Self.snapshot()
        mismatched.appKitScreens.removeLast()
        var collections = 0
        var displayChangeDelivered = false
        var displayChangeTimer: Timer?
        defer { displayChangeTimer?.invalidate() }

        let service = LiveDisplayTopologyService(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live",
            collect: {
                collections += 1
                if collections == 1 {
                    let timer = Timer(
                        timeInterval: 0.01,
                        repeats: false
                    ) { _ in
                        displayChangeDelivered = true
                    }
                    displayChangeTimer = timer
                    RunLoop.current.add(timer, forMode: .default)
                }
                return displayChangeDelivered ? Self.snapshot() : mismatched
            })

        let topology = try service.observe()

        XCTAssertEqual(topology.displays.map(\.displayID), [1, 2])
        XCTAssertEqual(collections, 2)
        XCTAssertTrue(displayChangeDelivered)
    }

    func testLiveServiceRejectsPersistentDisplaySetMismatchAfterBoundedRetries() {
        var mismatched = Self.snapshot()
        mismatched.appKitScreens.removeLast()
        var collections = 0
        var settles = 0
        let service = LiveDisplayTopologyService(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live",
            collect: {
                collections += 1
                return mismatched
            },
            settleBeforeRetry: { settles += 1 })

        XCTAssertThrowsError(try service.observe())
        XCTAssertEqual(collections, 3)
        XCTAssertEqual(settles, 2)
    }

    func testTypedRPCUsesInjectedProviderAndSurfacesCollectorFailure() throws {
        let topology = try DisplayTopologyObservationBuilder(
            helperBootID: "helper_boot_live",
            topologyID: "topo_live").observe(
                snapshot: Self.snapshot(),
                capturedAt: "2026-07-22T12:00:00.001Z")
        let request = try JSONDecoder().decode(
            Request.self,
            from: Data(#"{"id":91,"method":"display_topology","params":{}}"#.utf8))
        let success = dispatch(
            id: request.id,
            method: request.method,
            params: try XCTUnwrap(request.params),
            displayTopologyProvider: { topology })
        let successJSON = try XCTUnwrap(
            JSONSerialization.jsonObject(with: makeWireEncoder().encode(success)) as? [String: Any])
        let result = try XCTUnwrap(successJSON["result"] as? [String: Any])
        XCTAssertEqual(result["topology_id"] as? String, "topo_live")
        XCTAssertNil(success.error)

        let failure = dispatch(
            id: request.id,
            method: request.method,
            params: try XCTUnwrap(request.params),
            displayTopologyProvider: {
                throw DisplayTopologyLiveError.invalid(displayTopologySetMismatchReason)
            })
        XCTAssertNil(failure.result)
        XCTAssertEqual(failure.error?.code, -32001)
    }

    private static func snapshot() -> DisplayTopologyRawSnapshot {
        DisplayTopologyRawSnapshot(
            mainDisplayID: 1,
            quartzDisplays: [
                DisplayTopologyQuartzDisplaySnapshot(
                    displayID: 1, isMain: true, isBuiltin: true,
                    isActive: true, isOnline: true, isAsleep: false,
                    bounds: DisplayTopologyRectV1(x: 0, y: 0, width: 1280, height: 800),
                    rotationDegrees: 0, mirrorMasterDisplayID: nil),
                DisplayTopologyQuartzDisplaySnapshot(
                    displayID: 2, isMain: false, isBuiltin: false,
                    isActive: true, isOnline: true, isAsleep: false,
                    bounds: DisplayTopologyRectV1(x: -1600, y: 100, width: 1600, height: 900),
                    rotationDegrees: 0, mirrorMasterDisplayID: nil),
            ],
            appKitScreens: [
                DisplayTopologyAppKitScreenSnapshot(
                    displayID: 1,
                    frame: DisplayTopologyRectV1(x: 0, y: 0, width: 1280, height: 800),
                    visibleFrame: DisplayTopologyRectV1(x: 0, y: 25, width: 1280, height: 750),
                    backingScaleFactor: 2, pixelWidth: 2560, pixelHeight: 1600),
                DisplayTopologyAppKitScreenSnapshot(
                    displayID: 2,
                    frame: DisplayTopologyRectV1(x: -1600, y: -200, width: 1600, height: 900),
                    visibleFrame: DisplayTopologyRectV1(x: -1600, y: -200, width: 1600, height: 875),
                    backingScaleFactor: 1, pixelWidth: 1600, pixelHeight: 900),
            ])
    }
}
