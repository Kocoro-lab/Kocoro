import Foundation
import XCTest
@testable import ax_server

final class CaptureCoordinateWindowTests: XCTestCase {
    func testCanonicalRequestDecoderAndResultEncoders() throws {
        let requestEnvelope = try JSONDecoder().decode(
            Request.self,
            from: fixture("capture_coordinate_window.request.v1.json"))
        let request = try CaptureCoordinateWindowRequestV1(
            params: try XCTUnwrap(requestEnvelope.params))
        XCTAssertEqual(requestEnvelope.method, "capture_coordinate_window")
        XCTAssertEqual(request.topologyRef.topologyID, "topo_mixed_001")
        XCTAssertEqual(request.pid, 4242)
        XCTAssertEqual(request.bundleID, "com.example.fixture")
        XCTAssertEqual(request.windowID, 7001)

        let success = CaptureCoordinateWindowResultV1.captured(
            topologyRef: request.topologyRef,
            helperBootID: "helper_boot_demo",
            pid: request.pid,
            bundleID: request.bundleID,
            windowID: request.windowID,
            windowQuartzBounds: request.expectedQuartzBounds,
            displayID: 2,
            backingScaleFactor: 1,
            png: Self.png,
            widthPX: 1,
            heightPX: 1,
            capturedAt: "2026-07-22T12:03:30Z")
        try assertResult(success, matches: "capture_coordinate_window.response.success.v1.json")
        try assertResult(
            .failed(code: "window_bounds_mismatch", retrySafe: true),
            matches: "capture_coordinate_window.response.failure.v1.json")
    }

    func testInjectedCaptureRunsExactPreflightCapturePostflightSequence() throws {
        var sequence: [String] = []
        var topologyCalls = 0
        let request = Self.request()
        let result = captureCoordinateWindow(
            request: request,
            dependencies: CaptureCoordinateWindowDependencies(
                observeTopology: {
                    sequence.append("topology")
                    topologyCalls += 1
                    return Self.topology(capturedAt: topologyCalls == 1
                        ? "2026-07-22T12:03:00Z"
                        : "2026-07-22T12:03:30Z")
                },
                bundleIDForPID: { pid in
                    sequence.append("bundle:\(pid)")
                    return "com.example.fixture"
                },
                exactWindow: { windowID in
                    sequence.append("window:\(windowID)")
                    return Self.window()
                },
                capturePNG: { windowID, timeout, _ in
                    sequence.append("capture:\(windowID):\(Int(timeout))")
                    return Self.png
                },
                rawByteCap: 1024,
                ndjsonByteCap: 4096,
                captureTimeout: 8))

        XCTAssertEqual(result.status, "captured")
        XCTAssertEqual(sequence, [
            "topology", "bundle:4242", "window:7001", "capture:7001:8",
            "topology", "bundle:4242", "window:7001",
        ])
        try assertResult(result, matches: "capture_coordinate_window.response.success.v1.json")
    }

    func testInjectedCaptureFailsClosedAcrossIdentityGeometryAndRaceGates() throws {
        let request = Self.request()
        let tests: [(String, String, (inout CaptureFixtureState) -> Void)] = [
            ("stale topology", "stale_topology", { $0.preTopologyGeneration = 8 }),
            ("bundle mismatch", "process_identity_mismatch", { $0.bundleID = "com.attacker" }),
            ("wrong pid", "window_identity_mismatch", { $0.preWindowPID = 999 }),
            ("wrong exact window id", "window_identity_mismatch", { $0.preWindowID = 7002 }),
            ("nonzero layer", "window_not_actionable", { $0.preWindowLayer = 1 }),
            ("offscreen", "window_not_actionable", { $0.preWindowOnScreen = false }),
            ("bounds mismatch", "window_bounds_mismatch", { $0.preWindowBounds = .init(x: -100, y: 200, width: 4, height: 1) }),
            ("topology changed", "topology_changed", { $0.postTopologyGeneration = 8 }),
            ("helper boot changed", "topology_changed", { $0.postHelperBootID = "helper_restarted" }),
            ("structure changed without generation", "topology_changed", { $0.postDisplayTwoX = -1599 }),
            ("post bundle changed", "process_identity_mismatch", { $0.postBundleID = "com.attacker" }),
            ("post window missing", "window_changed", { $0.postWindowMissing = true }),
            ("post pid changed", "window_changed", { $0.postWindowPID = 999 }),
            ("post layer changed", "window_changed", { $0.postWindowLayer = 1 }),
            ("post onscreen changed", "window_changed", { $0.postWindowOnScreen = false }),
            ("window changed", "window_changed", { $0.postWindowBounds = .init(x: -99, y: 200, width: 1, height: 1) }),
            ("wrong png dimensions", "image_dimensions_mismatch", { $0.pngWidth = 2 }),
            ("raw too large", "image_too_large", { $0.rawByteCap = 10 }),
            ("response too large", "response_too_large", { $0.ndjsonByteCap = 100 }),
        ]
        for (name, code, mutate) in tests {
            var state = CaptureFixtureState()
            mutate(&state)
            let result = captureCoordinateWindow(
                request: request,
                dependencies: state.dependencies())
            XCTAssertEqual(result.status, "failed", name)
            XCTAssertEqual(result.failureCode, code, name)
            if code == "image_dimensions_mismatch" {
                XCTAssertTrue(result.retrySafe, name)
                let diagnostics = try XCTUnwrap(result.failureDiagnostics, name)
                XCTAssertEqual(diagnostics.stage, "decoded_dimensions", name)
                XCTAssertEqual(diagnostics.pid, 4242, name)
                XCTAssertEqual(diagnostics.bundleID, "com.example.fixture", name)
                XCTAssertEqual(diagnostics.windowID, 7001, name)
                XCTAssertEqual(diagnostics.preWindowQuartzBounds, diagnostics.postWindowQuartzBounds, name)
                XCTAssertEqual(diagnostics.displayID, 2, name)
                XCTAssertEqual(diagnostics.backingScaleFactor, 1, name)
                XCTAssertEqual(diagnostics.expectedWidthPX, 1, name)
                XCTAssertEqual(diagnostics.expectedHeightPX, 1, name)
                XCTAssertEqual(diagnostics.metadataWidthPX, 2, name)
                XCTAssertEqual(diagnostics.metadataHeightPX, 1, name)
                XCTAssertEqual(diagnostics.decodedWidthPX, 2, name)
                XCTAssertEqual(diagnostics.decodedHeightPX, 1, name)
            }
        }
    }

    func testNonIntegralExpectedDimensionsCarryIdentityWithoutInventingActualPixels() throws {
        var state = CaptureFixtureState()
        state.preWindowBounds = .init(x: -100, y: 200, width: 1.5, height: 1)
        state.postWindowBounds = state.preWindowBounds
        let result = captureCoordinateWindow(
            request: Self.request(width: 1.5),
            dependencies: state.dependencies())

        XCTAssertEqual(result.failureCode, "image_dimensions_mismatch")
        let diagnostics = try XCTUnwrap(result.failureDiagnostics)
        XCTAssertEqual(diagnostics.stage, "non_integral_expected_dimensions")
        XCTAssertEqual(diagnostics.expectedWidthPX, 1.5)
        XCTAssertNil(diagnostics.metadataWidthPX)
        XCTAssertNil(diagnostics.decodedWidthPX)
    }

    func testInjectedCaptureRejectsCrossDisplayMirrorFollowerAndRotation() throws {
        let request = Self.request()
        for (name, topology) in [
            ("cross display", Self.topology(windowNotFullyContained: true)),
            ("mirror follower", Self.topology(displayTwoMirrorMaster: 1)),
            ("rotated", Self.topology(displayTwoRotation: 90)),
            ("inactive", Self.topology(displayTwoIsActive: false)),
            ("offline", Self.topology(displayTwoIsOnline: false)),
            ("asleep", Self.topology(displayTwoIsAsleep: true)),
            ("ambiguous overlap", Self.topology(displayOneContainsWindow: true)),
        ] {
            var state = CaptureFixtureState()
            state.preTopology = topology
            state.postTopology = topology
            let result = captureCoordinateWindow(request: request, dependencies: state.dependencies())
            XCTAssertEqual(result.failureCode, "display_not_actionable", name)
        }
    }

    func testInjectedCaptureMapsTimeoutAndInvalidPNGToStructuredFailures() throws {
        var timeout = CaptureFixtureState()
        timeout.captureError = CaptureCoordinateWindowLiveError.timeout
        XCTAssertEqual(
            captureCoordinateWindow(request: Self.request(), dependencies: timeout.dependencies()).failureCode,
            "capture_timeout")

        var tooLarge = CaptureFixtureState()
        tooLarge.captureError = CaptureCoordinateWindowLiveError.tooLarge
        let tooLargeResult = captureCoordinateWindow(
            request: Self.request(), dependencies: tooLarge.dependencies())
        XCTAssertEqual(tooLargeResult.failureCode, "image_too_large")
        XCTAssertFalse(tooLargeResult.retrySafe)

        var invalid = CaptureFixtureState()
        invalid.png = Data("not png".utf8)
        XCTAssertEqual(
            captureCoordinateWindow(request: Self.request(), dependencies: invalid.dependencies()).failureCode,
            "invalid_png")

        var truncated = CaptureFixtureState()
        truncated.png = Data(Self.png.dropLast(20))
        XCTAssertEqual(
            captureCoordinateWindow(request: Self.request(), dependencies: truncated.dependencies()).failureCode,
            "invalid_png")

        var corruptIDAT = CaptureFixtureState()
        var corruptBytes = Array(Self.png)
        corruptBytes[50] ^= 0xff
        corruptIDAT.png = Data(corruptBytes)
        XCTAssertEqual(
            captureCoordinateWindow(request: Self.request(), dependencies: corruptIDAT.dependencies()).failureCode,
            "invalid_png")
    }

    func testProductionProcessWatchdogReapsAndTemporaryFileAlwaysCleansUp() throws {
        let started = Date()
        XCTAssertThrowsError(try runCoordinateWindowCaptureProcess(
            executableURL: URL(fileURLWithPath: "/bin/sleep"),
            arguments: ["10"],
            timeout: 0.01)) { error in
            XCTAssertEqual(error as? CaptureCoordinateWindowLiveError, .timeout)
        }
        XCTAssertLessThan(Date().timeIntervalSince(started), 2)

        var capturedURL: URL?
        XCTAssertThrowsError(try withCoordinateWindowTemporaryFile { url -> Data in
            capturedURL = url
            try Self.png.write(to: url)
            throw CaptureCoordinateWindowLiveError.failed
        })
        XCTAssertFalse(FileManager.default.fileExists(atPath: try XCTUnwrap(capturedURL).path))

        let stubborn = FakeCaptureProcess(exitOnStart: nil)
        XCTAssertThrowsError(try runCoordinateWindowCaptureProcess(
            process: stubborn,
            timeout: 0.001,
            terminationGrace: 0.001,
            killGrace: 0.01)) { error in
            XCTAssertEqual(error as? CaptureCoordinateWindowLiveError, .timeout)
        }
        XCTAssertEqual(stubborn.events, ["start", "term", "kill", "wait"])
        XCTAssertFalse(stubborn.isRunning)

        let successful = FakeCaptureProcess(exitOnStart: 0)
        try runCoordinateWindowCaptureProcess(process: successful, timeout: 0.01)
        XCTAssertEqual(successful.events, ["start", "wait"])

        let failed = FakeCaptureProcess(exitOnStart: 2)
        XCTAssertThrowsError(try runCoordinateWindowCaptureProcess(
            process: failed,
            timeout: 0.01))
        XCTAssertEqual(failed.events, ["start", "wait"])

        for outcome in ["success", "failure", "timeout"] {
            var urlAfterScope: URL?
            do {
                _ = try withCoordinateWindowTemporaryFile { url -> Data in
                    urlAfterScope = url
                    try Self.png.write(to: url)
                    if outcome != "success" { throw CaptureCoordinateWindowLiveError.failed }
                    return Self.png
                }
            } catch {
                XCTAssertNotEqual(outcome, "success")
            }
            XCTAssertFalse(FileManager.default.fileExists(atPath: try XCTUnwrap(urlAfterScope).path))
        }
    }

    func testWindowCaptureArgumentsPreserveBackgroundIsolationAndForegroundSurfaces() {
        let outputURL = URL(fileURLWithPath: "/tmp/exact-window.png")

        XCTAssertEqual(
            coordinateWindowScreencaptureArguments(
                windowID: 7001,
                foregroundCompositeBounds: nil,
                outputURL: outputURL),
            ["-x", "-o", "-a", "-l7001", "/tmp/exact-window.png"])
        XCTAssertEqual(
            coordinateWindowScreencaptureArguments(
                windowID: 7001,
                foregroundCompositeBounds: DisplayTopologyRectV1(
                    x: -100, y: 200, width: 800, height: 600),
                outputURL: outputURL),
            ["-x", "-R-100,200,800,600", "/tmp/exact-window.png"])
        XCTAssertEqual(
            coordinateWindowScreencaptureArguments(
                windowID: 7001,
                foregroundCompositeBounds: DisplayTopologyRectV1(
                    x: -100.5, y: 200, width: 800, height: 600),
                outputURL: outputURL),
            ["-x", "-o", "-a", "-l7001", "/tmp/exact-window.png"])
    }

    func testBoundedCaptureFileReadAcceptsExactCapAndRejectsOneByteOver() throws {
        try withCoordinateWindowTemporaryFile { url in
            try Self.png.write(to: url)
            XCTAssertEqual(
                try readCoordinateWindowCaptureFile(url, rawByteCap: Self.png.count),
                Self.png)
            XCTAssertThrowsError(try readCoordinateWindowCaptureFile(
                url,
                rawByteCap: Self.png.count - 1)) { error in
                XCTAssertEqual(error as? CaptureCoordinateWindowLiveError, .tooLarge)
            }
        }
    }

    private struct CaptureFixtureState {
        var preTopology = CaptureCoordinateWindowTests.topology(capturedAt: "2026-07-22T12:03:00Z")
        var postTopology = CaptureCoordinateWindowTests.topology(capturedAt: "2026-07-22T12:03:30Z")
        var preTopologyGeneration: UInt64?
        var postTopologyGeneration: UInt64?
        var bundleID = "com.example.fixture"
        var postBundleID: String?
        var preWindowPID = 4242
        var preWindowID: UInt32 = 7001
        var preWindowLayer = 0
        var preWindowOnScreen = true
        var postWindowPID: Int?
        var postWindowLayer: Int?
        var postWindowOnScreen: Bool?
        var postWindowMissing = false
        var preWindowBounds = DisplayTopologyRectV1(x: -100, y: 200, width: 1, height: 1)
        var postWindowBounds = DisplayTopologyRectV1(x: -100, y: 200, width: 1, height: 1)
        var postHelperBootID: String?
        var postDisplayTwoX: Double?
        var png = CaptureCoordinateWindowTests.png
        var pngWidth = 1
        var rawByteCap = 1024
        var ndjsonByteCap = 4096
        var captureError: Error?

        func dependencies() -> CaptureCoordinateWindowDependencies {
            var topologyCall = 0
            var windowCall = 0
            var bundleCall = 0
            return CaptureCoordinateWindowDependencies(
                observeTopology: {
                    topologyCall += 1
                    var value = topologyCall == 1 ? preTopology : postTopology
                    if topologyCall == 1, let preTopologyGeneration {
                        value = CaptureCoordinateWindowTests.topology(
                            generation: preTopologyGeneration,
                            capturedAt: value.capturedAt)
                    }
                    if topologyCall == 2, let postTopologyGeneration {
                        value = CaptureCoordinateWindowTests.topology(
                            generation: postTopologyGeneration,
                            capturedAt: value.capturedAt)
                    }
                    if topologyCall == 2, let postHelperBootID {
                        value = CaptureCoordinateWindowTests.topology(
                            generation: value.generation,
                            capturedAt: value.capturedAt,
                            helperBootID: postHelperBootID)
                    }
                    if topologyCall == 2, let postDisplayTwoX {
                        value = CaptureCoordinateWindowTests.topology(
                            generation: value.generation,
                            capturedAt: value.capturedAt,
                            displayTwoX: postDisplayTwoX)
                    }
                    return value
                },
                bundleIDForPID: { _ in
                    bundleCall += 1
                    return bundleCall == 1 ? bundleID : (postBundleID ?? bundleID)
                },
                exactWindow: { _ in
                    windowCall += 1
                    if windowCall == 2 && postWindowMissing { return nil }
                    return CaptureCoordinateWindowWindowSnapshot(
                        windowID: preWindowID,
                        ownerPID: windowCall == 1 ? preWindowPID : (postWindowPID ?? preWindowPID),
                        layer: windowCall == 1 ? preWindowLayer : (postWindowLayer ?? preWindowLayer),
                        isOnScreen: windowCall == 1 ? preWindowOnScreen : (postWindowOnScreen ?? preWindowOnScreen),
                        bounds: windowCall == 1 ? preWindowBounds : postWindowBounds)
                },
                capturePNG: { _, _, _ in
                    if let captureError { throw captureError }
                    if pngWidth == 1 { return png }
                    return CaptureCoordinateWindowTests.png2x1
                },
                rawByteCap: rawByteCap,
                ndjsonByteCap: ndjsonByteCap,
                captureTimeout: 8)
        }
    }

    private final class FakeCaptureProcess: CoordinateWindowProcessControlling {
        var events: [String] = []
        var isRunning = false
        var terminationStatus: Int32 = 0
        private let exitOnStart: Int32?
        private var onTermination: (() -> Void)?

        init(exitOnStart: Int32?) {
            self.exitOnStart = exitOnStart
        }

        func start(onTermination: @escaping () -> Void) throws {
            events.append("start")
            self.onTermination = onTermination
            isRunning = true
            if let exitOnStart {
                terminationStatus = exitOnStart
                isRunning = false
                onTermination()
            }
        }

        func terminate() {
            events.append("term")
        }

        func kill() {
            events.append("kill")
            terminationStatus = SIGKILL
            isRunning = false
            onTermination?()
        }

        func waitUntilExit() {
            events.append("wait")
        }
    }

    private static var png: Data {
        Data(base64Encoded: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")!
    }

    private static var png2x1: Data {
        Data(base64Encoded: "iVBORw0KGgoAAAANSUhEUgAAAAIAAAABCAYAAAD0In+KAAAAC0lEQVR4nGNggAIAAAkAAftSuKkAAAAASUVORK5CYII=")!
    }

    private static func request(width: Double = 1) -> CaptureCoordinateWindowRequestV1 {
        CaptureCoordinateWindowRequestV1(
            schemaVersion: 1,
            topologyRef: CaptureCoordinateWindowTopologyRefV1(
                topologyID: "topo_mixed_001", generation: 7),
            pid: 4242,
            bundleID: "com.example.fixture",
            windowID: 7001,
            expectedQuartzBounds: DisplayTopologyRectV1(
                x: -100,
                y: 200,
                width: width,
                height: 1))
    }

    private static func window() -> CaptureCoordinateWindowWindowSnapshot {
        CaptureCoordinateWindowWindowSnapshot(
            windowID: 7001,
            ownerPID: 4242,
            layer: 0,
            isOnScreen: true,
            bounds: DisplayTopologyRectV1(x: -100, y: 200, width: 1, height: 1))
    }

    private static func topology(
        generation: UInt64 = 7,
        capturedAt: String = "2026-07-22T12:03:30Z",
        helperBootID: String = "helper_boot_demo",
        displayTwoMirrorMaster: UInt32? = nil,
        displayTwoRotation: Double = 0,
        displayTwoX: Double = -1600,
        displayTwoIsActive: Bool = true,
        displayTwoIsOnline: Bool = true,
        displayTwoIsAsleep: Bool = false,
        displayOneContainsWindow: Bool = false,
        windowNotFullyContained: Bool = false
    ) -> DisplayTopologyV1 {
        let firstBounds = displayOneContainsWindow
            ? DisplayTopologyRectV1(x: -200, y: 100, width: 1280, height: 800)
            : DisplayTopologyRectV1(x: 0, y: 0, width: 1280, height: 800)
        let secondBounds = windowNotFullyContained
            ? DisplayTopologyRectV1(x: -99.5, y: 200, width: 100, height: 100)
            : DisplayTopologyRectV1(x: displayTwoX, y: 100, width: 1600, height: 900)
        return DisplayTopologyV1(
            schemaVersion: 1,
            topologyID: "topo_mixed_001",
            helperBootID: helperBootID,
            generation: generation,
            capturedAt: capturedAt,
            mainDisplayID: 1,
            displays: [
                DisplayTopologyDisplayV1(
                    displayID: 1, isMain: true, isBuiltin: true, isActive: true, isOnline: true, isAsleep: false,
                    quartzBounds: firstBounds,
                    appKitFrame: .init(x: firstBounds.x, y: 0, width: 1280, height: 800),
                    appKitVisibleFrame: .init(x: firstBounds.x, y: 25, width: 1280, height: 750),
                    backingScaleFactor: 2, pixelWidth: 2560, pixelHeight: 1600,
                    rotationDegrees: 0, mirrorMasterDisplayID: nil),
                DisplayTopologyDisplayV1(
                    displayID: 2, isMain: false, isBuiltin: false,
                    isActive: displayTwoIsActive, isOnline: displayTwoIsOnline, isAsleep: displayTwoIsAsleep,
                    quartzBounds: secondBounds,
                    appKitFrame: .init(x: secondBounds.x, y: -200, width: secondBounds.width, height: secondBounds.height),
                    appKitVisibleFrame: .init(x: secondBounds.x, y: -200, width: secondBounds.width, height: secondBounds.height - 25),
                    backingScaleFactor: 1, pixelWidth: Int(secondBounds.width), pixelHeight: Int(secondBounds.height),
                    rotationDegrees: displayTwoRotation,
                    mirrorMasterDisplayID: displayTwoMirrorMaster),
            ])
    }

    private func fixture(_ name: String) throws -> Data {
        try Data(contentsOf: URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("testdata/\(name)"))
    }

    private func assertResult(_ result: CaptureCoordinateWindowResultV1, matches name: String) throws {
        let produced = try JSONSerialization.jsonObject(with: makeWireEncoder().encode(result)) as? NSDictionary
        let expected = try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary
        XCTAssertEqual(produced, expected, name)
    }
}
