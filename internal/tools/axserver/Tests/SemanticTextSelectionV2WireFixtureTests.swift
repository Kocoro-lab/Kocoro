import Foundation
import XCTest
@testable import ax_server

final class SemanticTextSelectionV2WireFixtureTests: XCTestCase {
    func testCanonicalRequestUsesStrictProductionDecoder() throws {
        let request = try decodeSemanticTextSelectionRPCRequestV2(
            fixture("semantic_text_selection.request.v2.json"))
        XCTAssertEqual(request.id, 703)
        XCTAssertEqual(request.method, "semantic_text_selection_v2")
        XCTAssertEqual(request.params.schemaVersion, 2)
        XCTAssertEqual(request.params.range, .init(location: 5, length: 12))
        XCTAssertEqual(request.params.fallbackPolicy, "report_unsupported")
    }

    func testStrictDecoderRejectsUnknownAndDuplicateMembers() throws {
        let original = String(
            decoding: try fixture("semantic_text_selection.request.v2.json"), as: UTF8.self)
        let unknown = original.replacingOccurrences(
            of: "\"fallback_policy\": \"report_unsupported\"",
            with: "\"fallback_policy\": \"report_unsupported\", \"unknown\": true")
        XCTAssertThrowsError(
            try decodeSemanticTextSelectionRPCRequestV2(Data(unknown.utf8)))
        let duplicate = original.replacingOccurrences(
            of: "\"pid\": 42", with: "\"pid\": 42, \"pid\": 43")
        XCTAssertThrowsError(
            try decodeSemanticTextSelectionRPCRequestV2(Data(duplicate.utf8)))
    }

    func testCanonicalResultsRoundTripStrictTaggedUnion() throws {
        for name in [
            "semantic_text_selection.response.verified.v2.json",
            "semantic_text_selection.response.mismatch.v2.json",
            "semantic_text_selection.response.fallback_required.v2.json",
            "semantic_text_selection.response.failed.v2.json",
            "semantic_text_selection.response.user_interference_precommit.v2.json",
            "semantic_text_selection.response.user_interference_postcommit.v2.json",
            "semantic_text_selection.response.commit_unknown.v2.json",
        ] {
            let expected = try decodeSemanticTextSelectionResultV2(fixture(name))
            let encoded = try makeWireEncoder().encode(expected)
            XCTAssertEqual(
                try JSONSerialization.jsonObject(with: encoded) as? NSDictionary,
                try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary,
                name)
        }
    }

    func testResultDecoderRejectsUnknownAndDuplicateMembers() throws {
        let original = String(
            decoding: try fixture(
                "semantic_text_selection.response.commit_unknown.v2.json"), as: UTF8.self)
        let unknown = original.replacingOccurrences(
            of: "\"selected_range\": null",
            with: "\"selected_range\": null, \"selection_content\": \"secret\"")
        XCTAssertThrowsError(try decodeSemanticTextSelectionResultV2(Data(unknown.utf8)))
        let duplicate = original.replacingOccurrences(
            of: "\"commit_state\": \"unknown\"",
            with: "\"commit_state\": \"unknown\", \"commit_state\": \"committed\"")
        XCTAssertThrowsError(try decodeSemanticTextSelectionResultV2(Data(duplicate.utf8)))
    }

    func testDispatchRejectsUnknownFieldAsStructuredNotCommittedResult() throws {
        let original = String(
            decoding: try fixture("semantic_text_selection.request.v2.json"), as: UTF8.self)
        let invalid = original.replacingOccurrences(
            of: "\"fallback_policy\": \"report_unsupported\"",
            with: "\"fallback_policy\": \"report_unsupported\", \"unknown\": true")
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
