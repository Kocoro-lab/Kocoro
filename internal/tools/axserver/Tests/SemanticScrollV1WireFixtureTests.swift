import Foundation
import XCTest
@testable import ax_server

final class SemanticScrollV1WireFixtureTests: XCTestCase {
    func testCanonicalRequestAndResultsRoundTripStrictly() throws {
        let request = try decodeSemanticScrollRPCRequestV1(
            fixture("semantic_scroll.request.v1.json"))
        XCTAssertEqual(request.method, "semantic_scroll_v1")
        XCTAssertEqual(request.params.steps, 3)
        for name in [
            "semantic_scroll.response.verified.v1.json",
            "semantic_scroll.response.fallback_required.v1.json",
            "semantic_scroll.response.commit_unknown.v1.json",
        ] {
            let expected = try decodeSemanticScrollResultV1(fixture(name))
            let encoded = try makeWireEncoder().encode(expected)
            XCTAssertEqual(
                try JSONSerialization.jsonObject(with: encoded) as? NSDictionary,
                try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary)
        }
    }

    func testUnknownDuplicateAndDispatchFailureAreStrict() throws {
        let original = String(
            decoding: try fixture("semantic_scroll.request.v1.json"), as: UTF8.self)
        let unknown = original.replacingOccurrences(
            of: "\"steps\": 3", with: "\"steps\": 3, \"unknown\": true")
        XCTAssertThrowsError(try decodeSemanticScrollRPCRequestV1(Data(unknown.utf8)))
        let duplicate = original.replacingOccurrences(
            of: "\"pid\": 42", with: "\"pid\": 42, \"pid\": 43")
        XCTAssertThrowsError(try decodeSemanticScrollRPCRequestV1(Data(duplicate.utf8)))

        let response = dispatchWireRequest(Data(unknown.utf8))
        let data = try makeWireEncoder().encode(response)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        let result = try XCTUnwrap(object["result"] as? [String: Any])
        XCTAssertEqual(result["status"] as? String, "failed")
        XCTAssertEqual(result["commit_state"] as? String, "not_committed")
        XCTAssertEqual(result["failure_code"] as? String, "invalid_request")
    }

    func testNotCommittedResultCannotReportCompletedSteps() throws {
        let impossible = [
            SemanticScrollResultV1(
                status: "cancelled", commitState: "not_committed", phase: "cancelled",
                failureCode: "controller_cancelled", stepsCompleted: 1, expectedSteps: 3),
            SemanticScrollResultV1(
                status: "user_interference", commitState: "not_committed",
                phase: "user_interference", failureCode: "physical_input_interference",
                stepsCompleted: 1, expectedSteps: 3),
        ]
        for result in impossible {
            XCTAssertThrowsError(try makeWireEncoder().encode(result))
        }
    }

    func testCancellationMarkerMatchesGoFixtureAndProductionDependency() throws {
        let envelope = try decodeSemanticScrollRPCRequestV1(
            fixture("semantic_scroll.request.v1.json"))
        let markerFixture = try XCTUnwrap(
            JSONSerialization.jsonObject(with: fixture(
                "semantic_scroll.cancellation_marker.v1.json")) as? [String: Any])
        let requestID = try XCTUnwrap(
            markerFixture["request_id"] as? NSNumber).int64Value
        let basename = try XCTUnwrap(markerFixture["basename"] as? String)
        let url = semanticScrollCancellationMarkerURLV1(
            requestID: requestID, request: envelope.params)
        XCTAssertEqual(url.lastPathComponent, basename)
        defer { try? FileManager.default.removeItem(at: url) }
        try? FileManager.default.removeItem(at: url)
        let dependencies = productionSemanticScrollDependenciesV1(
            requestID: requestID, request: envelope.params)
        XCTAssertFalse(dependencies.isCancelled())
        XCTAssertTrue(FileManager.default.createFile(
            atPath: url.path, contents: Data(),
            attributes: [.posixPermissions: 0o600]))
        XCTAssertTrue(dependencies.isCancelled())
    }

    private func fixture(_ name: String) throws -> Data {
        try Data(contentsOf: URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent("testdata/\(name)"))
    }
}
