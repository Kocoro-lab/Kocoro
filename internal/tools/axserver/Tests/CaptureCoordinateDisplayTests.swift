import Foundation
import XCTest
@testable import ax_server

final class CaptureCoordinateDisplayTests: XCTestCase {
    func testCanonicalStrictRequestAndResultFixtures() throws {
        let request = try decodeCaptureCoordinateDisplayRPCRequestV1(
            fixture("capture_coordinate_display.request.v1.json"))
        XCTAssertEqual(request.id, 802)
        XCTAssertEqual(request.params.displayID, 2)
        XCTAssertEqual(request.params.topologyRef.topologyID, "topo_display_001")

        let success = CaptureCoordinateDisplayResultV1.captured(
            topologyRef: request.params.topologyRef,
            helperBootID: "helper_display_001",
            displayID: request.params.displayID,
            displayQuartzBounds: .init(x: -100, y: 200, width: 1, height: 1),
            backingScaleFactor: 1,
            png: Self.png,
            widthPX: 1,
            heightPX: 1,
            capturedAt: "2026-07-23T01:02:03Z")
        try assertResult(success, matches: "capture_coordinate_display.response.success.v1.json")
        try assertResult(
            .failed(code: "display_not_actionable", retrySafe: true),
            matches: "capture_coordinate_display.response.failure.v1.json")
    }

    func testStrictRequestRejectsUnknownDuplicateAndTrailingJSON() throws {
        let canonical = String(
            data: try fixture("capture_coordinate_display.request.v1.json"),
            encoding: .utf8)!
        for invalid in [
            canonical.replacingOccurrences(of: "\"display_id\": 2", with: "\"display_id\": 2, \"extra\": 1"),
            canonical.replacingOccurrences(of: "\"display_id\": 2", with: "\"display_id\": 2, \"display_id\": 2"),
            canonical + "{}",
        ] {
            XCTAssertThrowsError(try decodeCaptureCoordinateDisplayRPCRequestV1(Data(invalid.utf8)))
        }
    }

    func testCaptureBindsExactDisplayAcrossTopologyCaptureTopology() {
        var state = FixtureState()
        var sequence: [String] = []
        state.sequence = { sequence.append($0) }
        let result = captureCoordinateDisplay(
            request: Self.request(),
            dependencies: state.dependencies())
        XCTAssertEqual(result.status, "captured")
        XCTAssertEqual(result.displayID, 2)
        XCTAssertEqual(result.displayQuartzBounds, Self.display().quartzBounds)
        XCTAssertEqual(result.widthPX, 1)
        XCTAssertEqual(result.heightPX, 1)
        XCTAssertEqual(sequence, ["topology", "capture:2", "topology"])
    }

    func testRawWireDispatcherUsesStrictDisplayCaptureAndExactNegativeRectangle() throws {
        let state = FixtureState()
        let response = dispatchWireRequest(
            try fixture("capture_coordinate_display.request.v1.json"),
            coordinateDisplayDependencies: state.dependencies())
        let encoded = try JSONEncoder().encode(response)
        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: encoded) as? [String: Any])
        let result = try XCTUnwrap(object["result"] as? [String: Any])
        XCTAssertEqual(result["status"] as? String, "captured")
        XCTAssertEqual(result["display_id"] as? Int, 2)

        let outputURL = URL(fileURLWithPath: "/tmp/kocoro display.png")
        XCTAssertEqual(
            try coordinateDisplayScreencaptureArguments(
                display: Self.display(), outputURL: outputURL),
            ["-x", "-R-100,200,1,1", outputURL.path])

        let fractionalBounds = DisplayTopologyRectV1(x: -100.5, y: 200, width: 1, height: 1)
        let fractionalDisplay = DisplayTopologyDisplayV1(
            displayID: 2, isMain: false, isBuiltin: false,
            isActive: true, isOnline: true, isAsleep: false,
            quartzBounds: fractionalBounds, appKitFrame: fractionalBounds,
            appKitVisibleFrame: fractionalBounds, backingScaleFactor: 1,
            pixelWidth: 1, pixelHeight: 1, rotationDegrees: 0,
            mirrorMasterDisplayID: nil)
        XCTAssertThrowsError(try coordinateDisplayScreencaptureArguments(
            display: fractionalDisplay, outputURL: outputURL))
    }

    func testCaptureRejectsUnsupportedOrChangedDisplayWithoutPublishingBytes() {
        let cases: [(String, (inout FixtureState) -> Void, String)] = [
            ("stale topology", { $0.requestTopologyGeneration = 8 }, "stale_topology"),
            ("missing display", { $0.omitDisplay = true }, "display_not_found"),
            ("inactive", { $0.isActive = false }, "display_not_actionable"),
            ("offline", { $0.isOnline = false }, "display_not_actionable"),
            ("asleep", { $0.isAsleep = true }, "display_not_actionable"),
            ("mirror follower", { $0.mirrorMaster = 1 }, "display_not_actionable"),
            ("rotated", { $0.rotation = 90 }, "display_not_actionable"),
            ("capture timeout", { $0.captureError = CaptureCoordinateDisplayLiveError.timeout }, "capture_timeout"),
            ("capture too large", { $0.captureError = CaptureCoordinateDisplayLiveError.tooLarge }, "image_too_large"),
            ("capture failed", { $0.captureError = CaptureCoordinateDisplayLiveError.failed }, "capture_failed"),
            ("topology changed", { $0.postGeneration = 10 }, "topology_changed"),
            ("pixel dimensions changed", { $0.postPixelWidth = 2 }, "topology_changed"),
            ("wrong image dimensions", { $0.png = Self.png2x1 }, "image_dimensions_mismatch"),
            ("invalid PNG", { $0.png = Data("not png".utf8) }, "invalid_png"),
            ("raw cap", { $0.rawByteCap = Self.png.count - 1 }, "image_too_large"),
            ("response cap", { $0.ndjsonByteCap = 100 }, "response_too_large"),
        ]
        for (name, mutate, expectedCode) in cases {
            var state = FixtureState()
            mutate(&state)
            var request = Self.request()
            if let generation = state.requestTopologyGeneration {
                request = .init(
                    schemaVersion: 1,
                    topologyRef: .init(topologyID: "topo_display_001", generation: generation),
                    displayID: 2)
            }
            let result = captureCoordinateDisplay(request: request, dependencies: state.dependencies())
            XCTAssertEqual(result.status, "failed", name)
            XCTAssertEqual(result.failureCode, expectedCode, name)
            XCTAssertNil(result.imageBase64, name)
        }
    }

    private struct FixtureState {
        var requestTopologyGeneration: UInt64?
        var postGeneration: UInt64?
        var postPixelWidth: Int?
        var omitDisplay = false
        var isActive = true
        var isOnline = true
        var isAsleep = false
        var mirrorMaster: UInt32?
        var rotation = 0.0
        var png = CaptureCoordinateDisplayTests.png
        var captureError: Error?
        var rawByteCap = 1024
        var ndjsonByteCap = 4096
        var sequence: ((String) -> Void)?

        func dependencies() -> CaptureCoordinateDisplayDependencies {
            var topologyCall = 0
            return CaptureCoordinateDisplayDependencies(
                observeTopology: {
                    topologyCall += 1
                    sequence?("topology")
                    return CaptureCoordinateDisplayTests.topology(
                        generation: topologyCall == 2 ? (postGeneration ?? 9) : 9,
                        pixelWidth: topologyCall == 2 ? (postPixelWidth ?? 1) : 1,
                        omitDisplay: omitDisplay,
                        isActive: isActive,
                        isOnline: isOnline,
                        isAsleep: isAsleep,
                        mirrorMaster: mirrorMaster,
                        rotation: rotation)
                },
                capturePNG: { display, _, _ in
                    sequence?("capture:\(display.displayID)")
                    if let captureError { throw captureError }
                    return png
                },
                rawByteCap: rawByteCap,
                ndjsonByteCap: ndjsonByteCap,
                captureTimeout: 8)
        }
    }

    private static func request() -> CaptureCoordinateDisplayRequestV1 {
        .init(
            schemaVersion: 1,
            topologyRef: .init(topologyID: "topo_display_001", generation: 9),
            displayID: 2)
    }

    private static func display(pixelWidth: Int = 1) -> DisplayTopologyDisplayV1 {
        let bounds = DisplayTopologyRectV1(x: -100, y: 200, width: 1, height: 1)
        return DisplayTopologyDisplayV1(
            displayID: 2, isMain: false, isBuiltin: false,
            isActive: true, isOnline: true, isAsleep: false,
            quartzBounds: bounds, appKitFrame: bounds, appKitVisibleFrame: bounds,
            backingScaleFactor: 1, pixelWidth: pixelWidth, pixelHeight: 1,
            rotationDegrees: 0, mirrorMasterDisplayID: nil)
    }

    private static func topology(
        generation: UInt64,
        pixelWidth: Int,
        omitDisplay: Bool,
        isActive: Bool,
        isOnline: Bool,
        isAsleep: Bool,
        mirrorMaster: UInt32?,
        rotation: Double
    ) -> DisplayTopologyV1 {
        let mainBounds = DisplayTopologyRectV1(x: 0, y: 0, width: 1, height: 1)
        let main = DisplayTopologyDisplayV1(
            displayID: 1, isMain: true, isBuiltin: true,
            isActive: true, isOnline: true, isAsleep: false,
            quartzBounds: mainBounds, appKitFrame: mainBounds, appKitVisibleFrame: mainBounds,
            backingScaleFactor: 1, pixelWidth: 1, pixelHeight: 1,
            rotationDegrees: 0, mirrorMasterDisplayID: nil)
        var displays = [main]
        if !omitDisplay {
            let bounds = DisplayTopologyRectV1(x: -100, y: 200, width: 1, height: 1)
            displays.append(DisplayTopologyDisplayV1(
                displayID: 2, isMain: false, isBuiltin: false,
                isActive: isActive, isOnline: isOnline, isAsleep: isAsleep,
                quartzBounds: bounds, appKitFrame: bounds, appKitVisibleFrame: bounds,
                backingScaleFactor: 1, pixelWidth: pixelWidth, pixelHeight: 1,
                rotationDegrees: rotation, mirrorMasterDisplayID: mirrorMaster))
        }
        return DisplayTopologyV1(
            schemaVersion: 1, topologyID: "topo_display_001",
            helperBootID: "helper_display_001", generation: generation,
            capturedAt: "2026-07-23T01:02:03Z",
            mainDisplayID: 1, displays: displays)
    }

    private var pngFixtureURL: URL {
        URL(fileURLWithPath: #filePath)
    }

    private func fixture(_ name: String) throws -> Data {
        try Data(contentsOf: pngFixtureURL
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("testdata/\(name)"))
    }

    private func assertResult(_ result: CaptureCoordinateDisplayResultV1, matches name: String) throws {
        let produced = try JSONSerialization.jsonObject(with: makeWireEncoder().encode(result)) as? NSDictionary
        let expected = try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary
        XCTAssertEqual(produced, expected, name)
    }

    private static var png: Data {
        Data(base64Encoded: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")!
    }

    private static var png2x1: Data {
        Data(base64Encoded: "iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAYAAAD0In+KAAAAC0lEQVR4nGNggAIAAAkAAftSuKkAAAAASUVORK5CYII=")!
    }
}
