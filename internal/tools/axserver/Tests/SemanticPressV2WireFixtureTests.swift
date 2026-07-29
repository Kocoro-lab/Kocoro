import Foundation
import XCTest
@testable import ax_server

final class SemanticPressV2WireFixtureTests: XCTestCase {
    func testCanonicalRequestUsesStrictProductionDecoder() throws {
        let payload = try fixture("semantic_press.request.v2.json")
        let request = try decodeSemanticPressRPCRequestV2(payload)
        XCTAssertEqual(request.id, 702)
        XCTAssertEqual(request.method, "semantic_press_v2")
        XCTAssertEqual(request.params.schemaVersion, 2)
        XCTAssertEqual(request.params.bundleID, "com.apple.Notes")
        XCTAssertEqual(request.params.ref, "e1")
    }

    func testStrictDecoderRejectsUnknownAndDuplicateMembers() throws {
        let original = String(
            decoding: try fixture("semantic_press.request.v2.json"), as: UTF8.self)
        let unknown = original.replacingOccurrences(
            of: "\"fallback_policy\": \"none\"",
            with: "\"fallback_policy\": \"none\", \"unknown\": true")
        XCTAssertThrowsError(try decodeSemanticPressRPCRequestV2(Data(unknown.utf8)))
        let duplicate = original.replacingOccurrences(
            of: "\"pid\": 42", with: "\"pid\": 42, \"pid\": 43")
        XCTAssertThrowsError(try decodeSemanticPressRPCRequestV2(Data(duplicate.utf8)))
    }

    func testCanonicalResultsRoundTripStrictTaggedUnion() throws {
        for name in [
            "semantic_press.response.completed_unverified.v2.json",
            "semantic_press.response.user_interference_precommit.v2.json",
            "semantic_press.response.user_interference_postcommit.v2.json",
            "semantic_press.response.target_foreground_interference.v2.json",
            "semantic_press.response.failed.v2.json",
            "semantic_press.response.target_became_frontmost.v2.json",
            "semantic_press.response.monitor_lost_postcommit.v2.json",
            "semantic_press.response.commit_unknown.v2.json",
        ] {
            let expected = try decodeSemanticPressResultV2(fixture(name))
            let encoded = try makeWireEncoder().encode(expected)
            XCTAssertEqual(
                try JSONSerialization.jsonObject(with: encoded) as? NSDictionary,
                try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary,
                name)
        }
    }

    func testDispatchRejectsUnknownFieldAsStructuredInvalidRequest() throws {
        let original = String(
            decoding: try fixture("semantic_press.request.v2.json"), as: UTF8.self)
        let invalid = original.replacingOccurrences(
            of: "\"fallback_policy\": \"none\"",
            with: "\"fallback_policy\": \"none\", \"unknown\": true")
        let response = dispatchWireRequest(Data(invalid.utf8))
        let responseData = try makeWireEncoder().encode(response)
        let responseObject = try XCTUnwrap(
            JSONSerialization.jsonObject(with: responseData) as? [String: Any])
        let resultObject = try XCTUnwrap(responseObject["result"] as? [String: Any])
        XCTAssertEqual(resultObject["status"] as? String, "failed")
        XCTAssertEqual(resultObject["failure_code"] as? String, "invalid_request")
        XCTAssertEqual(resultObject["commit_state"] as? String, "not_committed")
    }

    private func fixture(_ name: String) throws -> Data {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("testdata/\(name)")
        return try Data(contentsOf: url)
    }
}
