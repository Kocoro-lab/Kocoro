import XCTest
@testable import ax_server

final class Phase0CWireFixtureTests: XCTestCase {
    func testCoordinateDragCanonicalFixturesRoundTrip() throws {
        let request = try fixture("coordinate_drag.request.v1.json")
        let decoded = try decodeCoordinateDragRPCRequestV1(request)
        XCTAssertEqual(decoded.id, 901)
        XCTAssertEqual(decoded.params.endTargetPolicy, "same_window")
        XCTAssertEqual(decoded.params.waypoints.count, 3)
        XCTAssertEqual(
            decoded.params.waypoints[1].quartzPoint,
            .init(x: 350.5, y: 450.5))

        for name in [
            "coordinate_drag.response.completed_unverified.v1.json",
            "coordinate_drag.response.user_interference.v1.json",
            "coordinate_drag.response.mouse_up_unverified.v1.json",
            "coordinate_drag.response.mouse_up_unverified_before_motion.v1.json",
            "coordinate_drag.response.failed.v1.json",
        ] {
            let payload = try fixture(name)
            let result = try decodeCoordinateDragResultV1(payload)
            XCTAssertFalse(result.retrySafe, name)
            XCTAssertEqual(
                try jsonObject(payload),
                try jsonObject(JSONEncoder().encode(result)), name)
        }
    }

    func testCancellationMarkerBasenameMatchesCrossLanguageFixture() throws {
        let payload = try XCTUnwrap(JSONSerialization.jsonObject(
            with: fixture("coordinate_drag.cancellation_marker.v1.json")) as? [String: Any])
        let requestID = try XCTUnwrap(payload["request_id"] as? Int64)
        let helperBootID = try XCTUnwrap(payload["helper_boot_id"] as? String)
        let expected = try XCTUnwrap(payload["basename"] as? String)
        XCTAssertEqual(
            coordinateDragCancellationMarkerURL(
                requestID: requestID, helperBootID: helperBootID).lastPathComponent,
            expected)
    }

    func testCoordinateDragRejectsUnknownAndDuplicateMembers() throws {
        var unknown = try String(
            data: fixture("coordinate_drag.request.v1.json"), encoding: .utf8)!
        unknown = unknown.replacingOccurrences(
            of: "\"duration_ms\": 240,",
            with: "\"duration_ms\": 240, \"extra\": true,")
        XCTAssertThrowsError(try decodeCoordinateDragRPCRequestV1(Data(unknown.utf8)))

        var duplicate = try String(
            data: fixture("coordinate_drag.request.v1.json"), encoding: .utf8)!
        duplicate = duplicate.replacingOccurrences(
            of: "\"generation\": 7",
            with: "\"generation\": 7, \"\\u0067eneration\": 7")
        XCTAssertThrowsError(try decodeCoordinateDragRPCRequestV1(Data(duplicate.utf8)))

        var pointerOnly = try String(
            data: fixture("coordinate_drag.response.completed_unverified.v1.json"),
            encoding: .utf8)!
        pointerOnly = pointerOnly
            .replacingOccurrences(
                of: "\"status\": \"completed_unverified\"",
                with: "\"status\": \"verified\"")
            .replacingOccurrences(
                of: "\"failure_code\": \"drop_postcondition_not_declared\"",
                with: "\"failure_code\": null")
            .replacingOccurrences(
                of: "\"postcondition\": null",
                with: "\"postcondition\": \"pointer_endpoint_reached\"")
        XCTAssertThrowsError(try decodeCoordinateDragResultV1(Data(pointerOnly.utf8)))
    }

    func testCoordinateDragRejectsImpossiblePointerMotionFlags() throws {
        XCTAssertThrowsError(try decodeCoordinateDragResultV1(
            fixture("coordinate_drag.response.invalid_pointer_motion.v1.json")))

        var failed = try String(
            data: fixture("coordinate_drag.response.failed.v1.json"),
            encoding: .utf8)!
        failed = failed.replacingOccurrences(
            of: "\"pointer_motion_committed\": false",
            with: "\"pointer_motion_committed\": true")
        XCTAssertThrowsError(try decodeCoordinateDragResultV1(Data(failed.utf8)))
    }

    func testSemanticSelectionCanonicalFixturesRoundTrip() throws {
        let request = try decodeSemanticTextSelectionRPCRequestV1(
            fixture("semantic_text_selection.request.v1.json"))
        XCTAssertEqual(request.params.ref, "e12")
        XCTAssertEqual(request.params.fallbackPolicy, "coordinate_drag")

        for name in [
            "semantic_text_selection.response.verified.v1.json",
            "semantic_text_selection.response.mismatch.v1.json",
            "semantic_text_selection.response.fallback_required.v1.json",
        ] {
            let payload = try fixture(name)
            let result = try decodeSemanticTextSelectionResultV1(payload)
            XCTAssertFalse(result.retrySafe)
            XCTAssertEqual(
                try jsonObject(payload),
                try jsonObject(JSONEncoder().encode(result)), name)
        }
    }

    func testSemanticSelectionRejectsDuplicateEscapedMember() throws {
        var duplicate = try String(
            data: fixture("semantic_text_selection.request.v1.json"), encoding: .utf8)!
        duplicate = duplicate.replacingOccurrences(
            of: "\"location\": 5",
            with: "\"location\": 5, \"\\u006cocation\": 5")
        XCTAssertThrowsError(
            try decodeSemanticTextSelectionRPCRequestV1(Data(duplicate.utf8)))
    }

    private func fixture(_ name: String) throws -> Data {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent("testdata/\(name)")
        return try Data(contentsOf: url)
    }

    private func jsonObject(_ data: Data) throws -> NSObject {
        try JSONSerialization.jsonObject(with: data) as! NSObject
    }
}
