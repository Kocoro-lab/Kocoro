import XCTest
@testable import ax_server

final class WindowCaptureTests: XCTestCase {
    func testAnnotateResultCarriesExactWindowIdentityAndLogicalBounds() throws {
        let result = AnnotateResult(
            app: "Editor",
            appName: "Editor",
            bundleID: "com.example.editor",
            pid: 42,
            window: "Document",
            windowID: 7001,
            windowFrame: AXFrame(x: -1000, y: 200, width: 800, height: 600),
            annotations: [],
            refPaths: [:])
        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: makeWireEncoder().encode(result)) as? [String: Any])
        XCTAssertEqual(object["window_id"] as? Int, 7001)
        let frame = try XCTUnwrap(object["window_frame"] as? [String: Any])
        XCTAssertEqual(frame["x"] as? Double, -1000)
        XCTAssertEqual(frame["width"] as? Double, 800)
        XCTAssertEqual(object["bundle_id"] as? String, "com.example.editor")
        XCTAssertNotNil(object["content_signature"] as? String)
    }

    func testAnnotationContentSignatureChangesWithWindowOrElementLayout() {
        let frame = AXFrame(x: -1000, y: 200, width: 800, height: 600)
        let annotation = AnnotationEntry(
            label: 1, ref: "a1", role: "AXButton", title: "Save",
            x: -900, y: 250, width: 80, height: 30)
        let baseline = annotationContentSignature(
            windowTitle: "Document", windowID: 7001, windowFrame: frame,
            annotations: [annotation])
        XCTAssertFalse(baseline.isEmpty)
        XCTAssertNotEqual(baseline, annotationContentSignature(
            windowTitle: "Document", windowID: 7001, windowFrame: frame,
            annotations: [AnnotationEntry(
                label: 1, ref: "a1", role: "AXButton", title: "Save",
                x: -800, y: 250, width: 80, height: 30)]))
        XCTAssertNotEqual(baseline, annotationContentSignature(
            windowTitle: "Other", windowID: 7001, windowFrame: frame,
            annotations: [annotation]))
    }

    func testCaptureWindowResultCarriesTypedPostCaptureSignature() throws {
        let signature = String(repeating: "a", count: 64)
        let result = CaptureWindowResult.success(
            "cG5n", 1280, 720, contentSignature: signature)
        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: makeWireEncoder().encode(result)) as? [String: Any])
        XCTAssertEqual(object["content_signature"] as? String, signature)
    }

    func testExactWindowSelectionRequiresIDPIDOnScreenAndLogicalBounds() {
        let expected = AXFrame(x: -1000, y: 200, width: 800, height: 600)
        let exact = WindowCaptureCandidate(
            id: 7001, ownerPID: 42, layer: 0, area: 480_000,
            title: "Document", onScreen: true,
            frame: AXFrame(x: -999, y: 201, width: 799, height: 601))
        let wrongPID = WindowCaptureCandidate(
            id: 7001, ownerPID: 99, layer: 0, area: 480_000,
            title: "Document", onScreen: true, frame: expected)
        XCTAssertEqual(
            selectExactWindowCaptureCandidate(
                pid: 42, windowID: 7001, expectedBounds: expected,
                candidates: [wrongPID, exact]),
            exact)

        let offscreen = WindowCaptureCandidate(
            id: 7001, ownerPID: 42, layer: 0, area: 480_000,
            title: "Document", onScreen: false, frame: expected)
        XCTAssertNil(selectExactWindowCaptureCandidate(
            pid: 42, windowID: 7001, expectedBounds: expected,
            candidates: [offscreen]))

        let moved = WindowCaptureCandidate(
            id: 7001, ownerPID: 42, layer: 0, area: 480_000,
            title: "Document", onScreen: true,
            frame: AXFrame(x: -997, y: 200, width: 800, height: 600))
        XCTAssertNil(selectExactWindowCaptureCandidate(
            pid: 42, windowID: 7001, expectedBounds: expected,
            candidates: [moved]))
    }
}
