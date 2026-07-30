import XCTest
@testable import ax_server

final class CoordinatePixelScrollWireFixtureTests: XCTestCase {
    func testCanonicalFixturesRoundTripAndPreserveExactProviderSemantics() throws {
        let requestPayload = try fixture("coordinate_pixel_scroll.request.v1.json")
        let request = try decodeCoordinatePixelScrollRPCRequestV1(requestPayload)
        XCTAssertEqual(request.id, 1201)
        XCTAssertEqual(request.params.providerDeltaX, 37)
        XCTAssertEqual(request.params.providerDeltaY, -618)
        XCTAssertEqual(
            try jsonObject(requestPayload),
            try jsonObject(JSONEncoder().encode(request)))

        for name in [
            "coordinate_pixel_scroll.response.committed_unverified.v1.json",
            "coordinate_pixel_scroll.response.user_interference.v1.json",
            "coordinate_pixel_scroll.response.failed.v1.json",
            "coordinate_pixel_scroll.response.commit_unknown.v1.json",
        ] {
            let payload = try fixture(name)
            let result = try decodeCoordinatePixelScrollResultV1(payload)
            XCTAssertFalse(result.retrySafe)
            XCTAssertEqual(
                try jsonObject(payload),
                try jsonObject(JSONEncoder().encode(result)), name)
        }
    }

    func testCancellationMarkerMatchesGoFixture() throws {
        let payload = try XCTUnwrap(JSONSerialization.jsonObject(
            with: fixture("coordinate_pixel_scroll.cancellation_marker.v1.json"))
            as? [String: Any])
        let requestID = try XCTUnwrap(payload["request_id"] as? Int64)
        let helperBootID = try XCTUnwrap(payload["helper_boot_id"] as? String)
        let expected = try XCTUnwrap(payload["basename"] as? String)
        XCTAssertEqual(
            coordinatePixelScrollCancellationMarkerURL(
                requestID: requestID, helperBootID: helperBootID).lastPathComponent,
            expected)
    }

    func testStrictDecoderRejectsUnknownDuplicateAndMismatchedCGMapping() throws {
        var unknown = try String(
            data: fixture("coordinate_pixel_scroll.request.v1.json"),
            encoding: .utf8)!
        unknown = unknown.replacingOccurrences(
            of: "\"unit\": \"pixel\",",
            with: "\"unit\": \"pixel\", \"extra\": true,")
        XCTAssertThrowsError(
            try decodeCoordinatePixelScrollRPCRequestV1(Data(unknown.utf8)))

        var duplicate = try String(
            data: fixture("coordinate_pixel_scroll.request.v1.json"),
            encoding: .utf8)!
        duplicate = duplicate.replacingOccurrences(
            of: "\"provider_delta_y\": -618",
            with: "\"provider_delta_y\": -618, \"\\u0070rovider_delta_y\": -618")
        XCTAssertThrowsError(
            try decodeCoordinatePixelScrollRPCRequestV1(Data(duplicate.utf8)))

        var requestScale = try String(
            data: fixture("coordinate_pixel_scroll.request.v1.json"),
            encoding: .utf8)!
        requestScale = requestScale.replacingOccurrences(
            of: "\"provider_to_quartz_scale_y\": 1",
            with: "\"provider_to_quartz_scale_y\": 0.6666666666666666")
        XCTAssertThrowsError(
            try decodeCoordinatePixelScrollRPCRequestV1(Data(requestScale.utf8)))

        var mismatched = try String(
            data: fixture(
                "coordinate_pixel_scroll.response.committed_unverified.v1.json"),
            encoding: .utf8)!
        mismatched = mismatched.replacingOccurrences(
            of: "\"cg_point_delta_axis1\": 618",
            with: "\"cg_point_delta_axis1\": 617")
        XCTAssertThrowsError(
            try decodeCoordinatePixelScrollResultV1(Data(mismatched.utf8)))

        var responseScale = try String(
            data: fixture(
                "coordinate_pixel_scroll.response.committed_unverified.v1.json"),
            encoding: .utf8)!
        responseScale = responseScale.replacingOccurrences(
            of: "\"provider_to_quartz_scale_x\": 1",
            with: "\"provider_to_quartz_scale_x\": 2")
        XCTAssertThrowsError(
            try decodeCoordinatePixelScrollResultV1(Data(responseScale.utf8)))
    }

    func testImpossibleCommitStateAndFailureCodeTuplesAreRejected() throws {
        let payload = try String(
            data: fixture(
                "coordinate_pixel_scroll.response.committed_unverified.v1.json"),
            encoding: .utf8)!
        let cancelledBeforeButCommitted = payload.replacingOccurrences(
            of: "\"failure_code\": \"scroll_postcondition_not_declared\"",
            with: "\"failure_code\": \"cancelled_before_scroll\"")
        XCTAssertThrowsError(try decodeCoordinatePixelScrollResultV1(
            Data(cancelledBeforeButCommitted.utf8)))

        let cancelledAfterButNotCommitted = payload
            .replacingOccurrences(
                of: "\"scroll_commit_state\": \"committed\"",
                with: "\"scroll_commit_state\": \"not_committed\"")
            .replacingOccurrences(
                of: "\"phase\": \"post_verification\"",
                with: "\"phase\": \"between_commits\"")
            .replacingOccurrences(
                of: "\"failure_code\": \"scroll_postcondition_not_declared\"",
                with: "\"failure_code\": \"cancelled_after_scroll\"")
        XCTAssertThrowsError(try decodeCoordinatePixelScrollResultV1(
            Data(cancelledAfterButNotCommitted.utf8)))
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
