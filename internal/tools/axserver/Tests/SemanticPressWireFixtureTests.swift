import Foundation
import XCTest
@testable import ax_server

final class SemanticPressWireFixtureTests: XCTestCase {
    func testProductionRequestDecoderMatchesCanonicalFixture() throws {
        let request = try JSONDecoder().decode(
            Request.self,
            from: fixture("semantic_press.request.v1.json"))

        XCTAssertEqual(request.id, 701)
        XCTAssertEqual(request.method, "semantic_press")
        XCTAssertEqual(request.params?.pid, 42)
        XCTAssertEqual(request.params?.windowID, 7001)
        XCTAssertEqual(request.params?.path, "window[0]/AXButton[0]")
        XCTAssertEqual(request.params?.expectedRole, "AXButton")
        XCTAssertEqual(request.params?.expectedFingerprint, "axf_target")
        XCTAssertEqual(request.params?.fallbackPolicy, "none")
    }

    func testProductionResultEncoderMatchesCanonicalFixtures() throws {
        let cases: [(String, SemanticPressResult)] = [
            ("semantic_press.response.changed_unverified.v1.json", SemanticPressResult(
                status: "completed_unverified", pressCommitted: true, phase: "post_observation",
                failureCode: "postcondition_not_declared", postcondition: nil, retrySafe: false)),
            ("semantic_press.response.completed_unverified.v1.json", SemanticPressResult(
                status: "completed_unverified", pressCommitted: true, phase: "post_observation",
                failureCode: "postcondition_not_observed", postcondition: nil, retrySafe: false)),
            ("semantic_press.response.failed.v1.json", SemanticPressResult(
                status: "failed", pressCommitted: false, phase: "preflight",
                failureCode: "fingerprint_ambiguous", postcondition: nil, retrySafe: false)),
        ]

        for (name, result) in cases {
            let produced = try JSONSerialization.jsonObject(with: makeWireEncoder().encode(result)) as? NSDictionary
            let expected = try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary
            XCTAssertEqual(produced, expected, name)
        }
    }

    func testDispatchRejectsUnsupportedFallbackAsRPCError() throws {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: fixture("semantic_press.request.v1.json")) as? [String: Any])
        var params = try XCTUnwrap(object["params"] as? [String: Any])
        params["fallback_policy"] = "synthetic"
        object["params"] = params
        let request = try JSONDecoder().decode(
            Request.self,
            from: JSONSerialization.data(withJSONObject: object))

        let response = dispatch(id: request.id, method: request.method, params: try XCTUnwrap(request.params))
        XCTAssertNotNil(response.error)
        XCTAssertNil(response.result)
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
