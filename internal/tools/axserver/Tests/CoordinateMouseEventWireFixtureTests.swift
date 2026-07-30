import Foundation
import XCTest
@testable import ax_server

final class CoordinateMouseEventWireFixtureTests: XCTestCase {
    func testProductionStrictDecoderAcceptsCanonicalClickMoveAndRiskRequests() throws {
        let click = try decodeCoordinateMouseEventRPCRequestV1(
            fixture("coordinate_mouse_event.request.click.v1.json"))
        XCTAssertEqual(click.id, 801)
        XCTAssertEqual(click.method, "coordinate_mouse_event")
        XCTAssertEqual(click.params.action, "click")
        XCTAssertEqual(click.params.button, "left")
        XCTAssertEqual(click.params.clickCount, 2)
        XCTAssertEqual(click.params.quartzPoint, .init(x: 200.5, y: 300.5))

        let move = try decodeCoordinateMouseEventRPCRequestV1(
            fixture("coordinate_mouse_event.request.move.v1.json"))
        XCTAssertEqual(move.id, 802)
        XCTAssertEqual(move.params.action, "move")
        XCTAssertNil(move.params.button)
        XCTAssertNil(move.params.clickCount)

        let risk = try decodeCoordinateMouseEventRPCRequestV1(
            fixture("coordinate_mouse_event.request.risk_click.v1.json"))
        XCTAssertEqual(risk.id, 803)
        XCTAssertEqual(risk.params.action, "click")
        XCTAssertEqual(risk.params.button, "left")
        XCTAssertEqual(risk.params.clickCount, 1)
        XCTAssertEqual(risk.params.riskAssertion?.kind, "consequential_click_v1")
        XCTAssertEqual(risk.params.riskAssertion?.riskKind, "send")
        XCTAssertEqual(
            risk.params.riskAssertion?.coordinateAuthority.elementPath,
            "window[0]/AXButton[0]")
    }

    func testStrictDecoderRejectsMissingNullAndUnknownFieldsAtEveryLevel() throws {
        let source = try object("coordinate_mouse_event.request.click.v1.json")
        let mutations: [(String, (inout [String: Any]) -> Void)] = [
            ("unknown envelope", { $0["unexpected"] = true }),
            ("missing method", { $0.removeValue(forKey: "method") }),
            ("unknown params", { root in
                var params = root["params"] as! [String: Any]
                params["unexpected"] = true
                root["params"] = params
            }),
            ("null required params", { root in
                var params = root["params"] as! [String: Any]
                params["helper_boot_id"] = NSNull()
                root["params"] = params
            }),
            ("unknown topology", { root in
                var params = root["params"] as! [String: Any]
                var topology = params["topology_ref"] as! [String: Any]
                topology["unexpected"] = true
                params["topology_ref"] = topology
                root["params"] = params
            }),
            ("unknown bounds", { root in
                var params = root["params"] as! [String: Any]
                var bounds = params["expected_window_quartz_bounds"] as! [String: Any]
                bounds["unexpected"] = true
                params["expected_window_quartz_bounds"] = bounds
                root["params"] = params
            }),
            ("unknown point", { root in
                var params = root["params"] as! [String: Any]
                var point = params["quartz_point"] as! [String: Any]
                point["unexpected"] = true
                params["quartz_point"] = point
                root["params"] = params
            }),
        ]

        for (name, mutation) in mutations {
            var candidate = source
            mutation(&candidate)
            XCTAssertThrowsError(
                try decodeCoordinateMouseEventRPCRequestV1(
                    JSONSerialization.data(withJSONObject: candidate)),
                name)
        }
    }

    func testRequestTaggedFieldsAndScalarBoundsAreStrict() throws {
        let source = try object("coordinate_mouse_event.request.click.v1.json")
        for button in ["left", "right", "wheel", "back", "forward"] {
            var candidate = source
            var params = candidate["params"] as! [String: Any]
            params["button"] = button
            candidate["params"] = params
            XCTAssertEqual(
                try decodeCoordinateMouseEventRPCRequestV1(
                    JSONSerialization.data(withJSONObject: candidate)).params.button,
                button)
        }
        let invalidParams: [(String, String, Any)] = [
            ("unknown action", "action", "drag"),
            ("unknown button", "button", "middle"),
            ("zero clicks", "click_count", 0),
            ("too many clicks", "click_count", 4),
            ("zero pid", "pid", 0),
            ("zero window", "window_id", 0),
            ("zero display", "display_id", 0),
            ("invalid deadline", "commit_deadline_at", "later"),
        ]
        for (name, key, value) in invalidParams {
            var candidate = source
            var params = candidate["params"] as! [String: Any]
            params[key] = value
            candidate["params"] = params
            XCTAssertThrowsError(
                try decodeCoordinateMouseEventRPCRequestV1(
                    JSONSerialization.data(withJSONObject: candidate)),
                name)
        }

        var move = try object("coordinate_mouse_event.request.move.v1.json")
        var moveParams = move["params"] as! [String: Any]
        moveParams["button"] = "left"
        move["params"] = moveParams
        XCTAssertThrowsError(try decodeCoordinateMouseEventRPCRequestV1(
            JSONSerialization.data(withJSONObject: move)))
    }

    func testPointerModifiersRequireCanonicalUniqueWireTokens() throws {
        let source = try object("coordinate_mouse_event.request.click.v1.json")
        var canonical = source
        var canonicalParams = canonical["params"] as! [String: Any]
        canonicalParams["modifiers"] = ["command", "shift"]
        canonical["params"] = canonicalParams
        XCTAssertEqual(
            try decodeCoordinateMouseEventRPCRequestV1(
                JSONSerialization.data(withJSONObject: canonical)).params.modifiers,
            ["command", "shift"])

        for modifiers in [["cmd"], ["shift", "shift"], ["capslock"]] {
            var invalid = source
            var params = invalid["params"] as! [String: Any]
            params["modifiers"] = modifiers
            invalid["params"] = params
            XCTAssertThrowsError(try decodeCoordinateMouseEventRPCRequestV1(
                JSONSerialization.data(withJSONObject: invalid)))
        }
    }

    func testConsequentialRiskAssertionIsStrictlyBoundToSingleLeftClickAndExactAuthority() throws {
        let source = try object("coordinate_mouse_event.request.risk_click.v1.json")
        let mutations: [(String, (inout [String: Any]) -> Void)] = [
            ("right click", { root in
                var params = root["params"] as! [String: Any]
                params["button"] = "right"
                root["params"] = params
            }),
            ("double click", { root in
                var params = root["params"] as! [String: Any]
                params["click_count"] = 2
                root["params"] = params
            }),
            ("mismatched point", { root in
                var params = root["params"] as! [String: Any]
                var assertion = params["risk_assertion"] as! [String: Any]
                var authority = assertion["coordinate_authority"] as! [String: Any]
                authority["quartz_point"] = ["x": 201.5, "y": 300.5]
                assertion["coordinate_authority"] = authority
                params["risk_assertion"] = assertion
                root["params"] = params
            }),
            ("mismatched topology", { root in
                var params = root["params"] as! [String: Any]
                var assertion = params["risk_assertion"] as! [String: Any]
                var authority = assertion["coordinate_authority"] as! [String: Any]
                authority["topology_ref"] = [
                    "topology_id": "topo_mixed_001", "generation": 8,
                ]
                assertion["coordinate_authority"] = authority
                params["risk_assertion"] = assertion
                root["params"] = params
            }),
            ("commit beyond frame", { root in
                var params = root["params"] as! [String: Any]
                params["commit_deadline_at"] = "2026-07-22T12:03:40.001Z"
                root["params"] = params
            }),
            ("unknown assertion field", { root in
                var params = root["params"] as! [String: Any]
                var assertion = params["risk_assertion"] as! [String: Any]
                assertion["unexpected"] = true
                params["risk_assertion"] = assertion
                root["params"] = params
            }),
            ("unknown coordinate authority field", { root in
                var params = root["params"] as! [String: Any]
                var assertion = params["risk_assertion"] as! [String: Any]
                var authority = assertion["coordinate_authority"] as! [String: Any]
                authority["unexpected"] = true
                assertion["coordinate_authority"] = authority
                params["risk_assertion"] = assertion
                root["params"] = params
            }),
            ("unknown source pixel field", { root in
                var params = root["params"] as! [String: Any]
                var assertion = params["risk_assertion"] as! [String: Any]
                var authority = assertion["coordinate_authority"] as! [String: Any]
                authority["source_pixel"] = ["x": 100, "y": 200, "z": 1]
                assertion["coordinate_authority"] = authority
                params["risk_assertion"] = assertion
                root["params"] = params
            }),
        ]
        for (name, mutation) in mutations {
            var candidate = source
            mutation(&candidate)
            XCTAssertThrowsError(
                try decodeCoordinateMouseEventRPCRequestV1(
                    JSONSerialization.data(withJSONObject: candidate)),
                name)
        }
    }

    func testAuthorityAndDeadlineRejectLeadingOrTrailingWhitespace() throws {
        let source = try object("coordinate_mouse_event.request.click.v1.json")
        let mutations: [(String, (inout [String: Any]) -> Void)] = [
            ("topology_id", { root in
                var params = root["params"] as! [String: Any]
                var topology = params["topology_ref"] as! [String: Any]
                topology["topology_id"] = " topo_mixed_001"
                params["topology_ref"] = topology
                root["params"] = params
            }),
            ("helper_boot_id", { root in
                var params = root["params"] as! [String: Any]
                params["helper_boot_id"] = "helper_boot_demo "
                root["params"] = params
            }),
            ("bundle_id", { root in
                var params = root["params"] as! [String: Any]
                params["bundle_id"] = " com.example.fixture"
                root["params"] = params
            }),
            ("commit_deadline_at", { root in
                var params = root["params"] as! [String: Any]
                params["commit_deadline_at"] = "2026-07-22T12:03:30.500Z "
                root["params"] = params
            }),
        ]
        for (name, mutation) in mutations {
            var candidate = source
            mutation(&candidate)
            XCTAssertThrowsError(
                try decodeCoordinateMouseEventRPCRequestV1(
                    JSONSerialization.data(withJSONObject: candidate)),
                name)
        }
    }

    func testStrictDecoderRejectsDuplicateMembersIncludingEscapedEquivalentKeys() throws {
        let source = try XCTUnwrap(String(
            data: fixture("coordinate_mouse_event.request.click.v1.json"),
            encoding: .utf8))
        let cases: [(String, String)] = [
            ("top level", source.replacingOccurrences(
                of: #""id": 801,"#,
                with: #""id": 801, "id": 801,"#)),
            ("nested", source.replacingOccurrences(
                of: #""action": "click","#,
                with: #""action": "click", "action": "click","#)),
            ("escaped equivalent", source.replacingOccurrences(
                of: #""id": 801,"#,
                with: #""id": 801, "\u0069d": 801,"#)),
        ]

        for (name, candidate) in cases {
            XCTAssertThrowsError(
                try decodeCoordinateMouseEventRPCRequestV1(Data(candidate.utf8)),
                name)
        }

        let risk = try XCTUnwrap(String(
            data: fixture("coordinate_mouse_event.request.risk_click.v1.json"),
            encoding: .utf8))
        XCTAssertThrowsError(try decodeCoordinateMouseEventRPCRequestV1(Data(
            risk.replacingOccurrences(
                of: #""frame_id": "frame_00112233445566778899aabbccddeeff","#,
                with: #""frame_id": "frame_00112233445566778899aabbccddeeff", "frame_id": "frame_00112233445566778899aabbccddeeff","#)
                .utf8)))
    }

    func testRawDispatcherUsesStrictCoordinateDecoderButKeepsLegacyCompatibility() throws {
        var coordinate = try object("coordinate_mouse_event.request.move.v1.json")
        coordinate["unexpected"] = true
        let rejected = dispatchWireRequest(
            try JSONSerialization.data(withJSONObject: coordinate))
        let rejectedObject = try responseObject(rejected)
        let rejectedResult = try XCTUnwrap(rejectedObject["result"] as? [String: Any])
        XCTAssertEqual(rejectedResult["status"] as? String, "failed")
        XCTAssertEqual(rejectedResult["action"] as? String, "unknown")
        XCTAssertEqual(rejectedResult["failure_code"] as? String, "invalid_request")
        XCTAssertEqual(rejectedResult["retry_safe"] as? Bool, false)

        let legacy = dispatchWireRequest(Data(
            #"{"id":99,"method":"ping","params":{"legacy_unknown":true}}"#.utf8))
        let legacyObject = try responseObject(legacy)
        XCTAssertNil(legacyObject["error"])
        XCTAssertEqual((legacyObject["result"] as? [String: Any])?["ok"] as? Bool, true)
    }

    func testProductionResultEncoderMatchesCanonicalFixturesIncludingNulls() throws {
        let point = CoordinateMouseEventPointV1(x: 200.5, y: 300.5)
        let exact = CoordinateMouseEventPointerEndpointV1(
            requested: point,
            observed: point,
            tolerance: 2,
            verified: true)
        let missed = CoordinateMouseEventPointerEndpointV1(
            requested: point,
            observed: .init(x: 210.5, y: 300.5),
            tolerance: 2,
            verified: false)
        let cases: [(String, CoordinateMouseEventResultV1)] = [
            ("coordinate_mouse_event.response.move.completed.v1.json", .init(
                status: "completed", action: "move",
                primaryActionCommitted: true, pointerMotionCommitted: true,
                phase: "post_verification", failureCode: nil,
                pointerEndpoint: exact)),
            ("coordinate_mouse_event.response.click.completed_unverified.v1.json", .init(
                status: "completed_unverified", action: "click",
                primaryActionCommitted: true, pointerMotionCommitted: true,
                phase: "post_verification", failureCode: "click_postcondition_not_declared",
                pointerEndpoint: exact)),
            ("coordinate_mouse_event.response.failed.stale_topology.v1.json", .init(
                status: "failed", action: "click",
                primaryActionCommitted: false, pointerMotionCommitted: false,
                phase: "preflight", failureCode: "stale_topology",
                pointerEndpoint: nil)),
            ("coordinate_mouse_event.response.failed.point_occluded.v1.json", .init(
                status: "failed", action: "move",
                primaryActionCommitted: false, pointerMotionCommitted: false,
                phase: "preflight", failureCode: "point_occluded",
                pointerEndpoint: nil)),
            ("coordinate_mouse_event.response.failed.endpoint.v1.json", .init(
                status: "failed", action: "click",
                primaryActionCommitted: false, pointerMotionCommitted: true,
                phase: "pointer_move", failureCode: "pointer_endpoint_not_verified",
                pointerEndpoint: missed)),
        ]
        for (name, result) in cases {
            let produced = try JSONSerialization.jsonObject(
                with: makeWireEncoder().encode(result)) as? NSDictionary
            let expected = try JSONSerialization.jsonObject(with: fixture(name)) as? NSDictionary
            XCTAssertEqual(produced, expected, name)
            XCTAssertEqual(result.retrySafe, false)
        }
    }

    private func object(_ name: String) throws -> [String: Any] {
        try XCTUnwrap(JSONSerialization.jsonObject(with: fixture(name)) as? [String: Any])
    }

    private func responseObject(_ response: Response) throws -> [String: Any] {
        try XCTUnwrap(JSONSerialization.jsonObject(
            with: makeWireEncoder().encode(response)) as? [String: Any])
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
