import Foundation
import XCTest
@testable import ax_server

final class DisplayTopologyTests: XCTestCase {
    func testProductionEncoderMatchesMixedHorizontalFixture() throws {
        let produced = try encodeDisplayTopologyV1(Self.mixedTopology())
        let fixture = try Data(contentsOf: Self.fixtureURL("display_topology.mixed_horizontal.v1.json"))
        let producedObject = try JSONSerialization.jsonObject(with: produced) as? NSDictionary
        let fixtureObject = try JSONSerialization.jsonObject(with: fixture) as? NSDictionary
        XCTAssertEqual(producedObject, fixtureObject)
    }

    func testValidatorRejectsInvalidAuthorityAndGeometry() throws {
        let valid = Self.mixedTopology()
        let tests: [(String, DisplayTopologyV1)] = [
            ("zero main display id", Self.topology(valid, mainDisplayID: 0)),
            ("zero display id", Self.topology(valid, displays: [
                Self.display(valid.displays[0], displayID: 0), valid.displays[1],
            ])),
            ("zero main and matching display id", Self.topology(
                valid,
                mainDisplayID: 0,
                displays: [Self.display(valid.displays[0], displayID: 0), valid.displays[1]])),
            ("duplicate display id", Self.topology(valid, displays: [valid.displays[0], valid.displays[0]])),
            ("negative size", Self.topology(valid, displays: [
                Self.display(valid.displays[0], quartzBounds: DisplayTopologyRectV1(x: 0, y: 0, width: -1, height: 800)),
                valid.displays[1],
            ])),
            ("zero size", Self.topology(valid, displays: [
                Self.display(valid.displays[0], quartzBounds: DisplayTopologyRectV1(x: 0, y: 0, width: 0, height: 800)),
                valid.displays[1],
            ])),
            ("zero pixels", Self.topology(valid, displays: [
                Self.display(valid.displays[0], pixelWidth: 0), valid.displays[1],
            ])),
            ("non finite scale", Self.topology(valid, displays: [
                Self.display(valid.displays[0], backingScaleFactor: .infinity),
                valid.displays[1],
            ])),
            ("logical size mismatch", Self.topology(valid, displays: [
                Self.display(valid.displays[0], appKitFrame: DisplayTopologyRectV1(x: 0, y: 0, width: 1279, height: 800)),
                valid.displays[1],
            ])),
            ("main not unique", Self.topology(valid, displays: [
                valid.displays[0], Self.display(valid.displays[1], isMain: true),
            ])),
            ("mirror self", Self.topology(valid, displays: [
                Self.display(valid.displays[0], mirrorMasterDisplayID: 1), valid.displays[1],
            ])),
            ("zero mirror master", Self.topology(valid, displays: [
                valid.displays[0], Self.display(valid.displays[1], mirrorMasterDisplayID: 0),
            ])),
        ]
        for (name, topology) in tests {
            XCTAssertThrowsError(try topology.validate(), name)
        }
    }

    private static func mixedTopology() -> DisplayTopologyV1 {
        DisplayTopologyV1(
            schemaVersion: 1,
            topologyID: "topo_mixed_001",
            helperBootID: "helper_boot_demo",
            generation: 7,
            capturedAt: "2026-07-22T12:00:00Z",
            mainDisplayID: 1,
            displays: [
                DisplayTopologyDisplayV1(
                    displayID: 1, isMain: true, isBuiltin: true, isActive: true, isOnline: true, isAsleep: false,
                    quartzBounds: DisplayTopologyRectV1(x: 0, y: 0, width: 1280, height: 800),
                    appKitFrame: DisplayTopologyRectV1(x: 0, y: 0, width: 1280, height: 800),
                    appKitVisibleFrame: DisplayTopologyRectV1(x: 0, y: 25, width: 1280, height: 750),
                    backingScaleFactor: 2, pixelWidth: 2560, pixelHeight: 1600,
                    rotationDegrees: 0, mirrorMasterDisplayID: nil),
                DisplayTopologyDisplayV1(
                    displayID: 2, isMain: false, isBuiltin: false, isActive: true, isOnline: true, isAsleep: false,
                    quartzBounds: DisplayTopologyRectV1(x: -1600, y: 100, width: 1600, height: 900),
                    appKitFrame: DisplayTopologyRectV1(x: -1600, y: -200, width: 1600, height: 900),
                    appKitVisibleFrame: DisplayTopologyRectV1(x: -1600, y: -200, width: 1600, height: 875),
                    backingScaleFactor: 1, pixelWidth: 1600, pixelHeight: 900,
                    rotationDegrees: 0, mirrorMasterDisplayID: nil),
            ])
    }

    private static func topology(
        _ source: DisplayTopologyV1,
        mainDisplayID: UInt32? = nil,
        displays: [DisplayTopologyDisplayV1]? = nil
    ) -> DisplayTopologyV1 {
        DisplayTopologyV1(
            schemaVersion: source.schemaVersion, topologyID: source.topologyID,
            helperBootID: source.helperBootID, generation: source.generation,
            capturedAt: source.capturedAt, mainDisplayID: mainDisplayID ?? source.mainDisplayID,
            displays: displays ?? source.displays)
    }

    private static func display(
        _ source: DisplayTopologyDisplayV1,
        displayID: UInt32? = nil,
        isMain: Bool? = nil,
        quartzBounds: DisplayTopologyRectV1? = nil,
        appKitFrame: DisplayTopologyRectV1? = nil,
        backingScaleFactor: Double? = nil,
        pixelWidth: Int? = nil,
        mirrorMasterDisplayID: UInt32?? = nil
    ) -> DisplayTopologyDisplayV1 {
        DisplayTopologyDisplayV1(
            displayID: displayID ?? source.displayID,
            isMain: isMain ?? source.isMain,
            isBuiltin: source.isBuiltin,
            isActive: source.isActive,
            isOnline: source.isOnline,
            isAsleep: source.isAsleep,
            quartzBounds: quartzBounds ?? source.quartzBounds,
            appKitFrame: appKitFrame ?? source.appKitFrame,
            appKitVisibleFrame: source.appKitVisibleFrame,
            backingScaleFactor: backingScaleFactor ?? source.backingScaleFactor,
            pixelWidth: pixelWidth ?? source.pixelWidth,
            pixelHeight: source.pixelHeight,
            rotationDegrees: source.rotationDegrees,
            mirrorMasterDisplayID: mirrorMasterDisplayID ?? source.mirrorMasterDisplayID)
    }

    private static func fixtureURL(_ name: String) -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("testdata/\(name)")
    }
}
