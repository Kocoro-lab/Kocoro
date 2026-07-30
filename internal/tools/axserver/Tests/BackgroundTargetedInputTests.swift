import Foundation
import XCTest
@testable import ax_server

final class BackgroundTargetedInputTests: XCTestCase {
    func testCanonicalCrossLanguageFixtureDecodesStrictly() throws {
        let payload = try fixture("background_targeted_input.request.type.v1.json")
        let request = try decodeBackgroundTargetedInputRPCRequestV1(payload)

        XCTAssertEqual(request.id, 905)
        XCTAssertEqual(request.method, "background_targeted_input")
        XCTAssertEqual(request.params.input.action, "type")
        XCTAssertEqual(request.params.focusedRef, "e2")
        XCTAssertEqual(request.params.targetLaunchDate, "2026-07-28T06:00:00Z")
        XCTAssertEqual(request.params.preservedFrontmostPID, 84)
    }

    func testStrictDecoderRejectsUnknownDuplicateTrailingAndControlText() throws {
        let valid = try fixture("background_targeted_input.request.type.v1.json")
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: valid) as? [String: Any])
        object["extra"] = true
        XCTAssertThrowsError(try decodeBackgroundTargetedInputRPCRequestV1(
            JSONSerialization.data(withJSONObject: object)))

        let source = String(decoding: valid, as: UTF8.self)
        XCTAssertThrowsError(try decodeBackgroundTargetedInputRPCRequestV1(Data(
            source.replacingOccurrences(
                of: "\"id\": 905",
                with: "\"id\": 905, \"\\u0069d\": 905")
                .utf8)))
        XCTAssertThrowsError(try decodeBackgroundTargetedInputRPCRequestV1(
            valid + Data("{\"trailing\":true}".utf8)))

        for text in ["line\nbreak", "tab\tvalue", "nul\u{0000}value"] {
            XCTAssertThrowsError(try decodeBackgroundTargetedInputRPCRequestV1(
                mutate(valid) { root in
                    var params = root["params"] as! [String: Any]
                    var input = params["input"] as! [String: Any]
                    input["text"] = text
                    params["input"] = input
                    root["params"] = params
                }))
        }
    }

    func testConsequentialKeysRejectedAndOrdinaryKeysAccepted() throws {
        for (keys, modifiers) in [
            (["return"], []),
            (["return"], ["command"]),
            (["delete"], ["shift"]),
        ] {
            XCTAssertThrowsError(try decodeBackgroundTargetedInputRPCRequestV1(
                try keypressPayload(keys: keys, modifiers: modifiers)))
        }
        for keys in [["space"], ["left"], ["down"]] {
            let request = try decodeBackgroundTargetedInputRPCRequestV1(
                try keypressPayload(keys: keys, modifiers: []))
            XCTAssertEqual(request.params.input.keys, keys)
        }
    }

    func testTypeVerifiesExactEditWithoutClipboardOrFocusRestoration() throws {
        let request = try runnableRequest(action: "type")
        let harness = BackgroundTargetedInputHarness()
        harness.typeVerificationPreparation = .ready(.init(observe: { _ in .matched }))

        let result = runBackgroundTargetedInputV1(
            request: request, dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "verified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(result.postcondition, "target_value_matches_expected_edit")
        XCTAssertFalse(result.clipboardTouched)
        XCTAssertFalse(result.clipboardRestored)
        XCTAssertEqual(harness.postTextCount, 1)
        XCTAssertEqual(harness.postKeypressCount, 0)
    }

    func testForegroundGuardAllowsUserToSwitchBetweenOtherApps() {
        XCTAssertNil(backgroundTargetedInputForegroundFailureV1(
            targetPID: 41, frontmostPID: 84))
        XCTAssertNil(backgroundTargetedInputForegroundFailureV1(
            targetPID: 41, frontmostPID: 96))
    }

    func testForegroundGuardRejectsTargetActivationAndUnavailableState() {
        XCTAssertEqual(
            backgroundTargetedInputForegroundFailureV1(
                targetPID: 41, frontmostPID: 41),
            "background_target_became_frontmost")
        XCTAssertEqual(
            backgroundTargetedInputForegroundFailureV1(
                targetPID: 41, frontmostPID: nil),
            "frontmost_process_unavailable")
    }

    func testBackgroundTargetActivationBeforeCommitNeverPosts() throws {
        let request = try runnableRequest(action: "type")
        let harness = BackgroundTargetedInputHarness()
        harness.authorityFailures = ["background_target_became_frontmost"]

        let result = runBackgroundTargetedInputV1(
            request: request, dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(result.failureCode, "background_target_became_frontmost")
        XCTAssertEqual(harness.postTextCount, 0)
    }

    func testBackgroundTargetActivationAfterCommitIsUnverifiedAndNeverReplayed() throws {
        let request = try runnableRequest(action: "type")
        let harness = BackgroundTargetedInputHarness()
        harness.authorityFailures = [nil, nil, "background_target_became_frontmost"]

        let result = runBackgroundTargetedInputV1(
            request: request, dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(
            result.failureCode, "background_target_became_frontmost_after_commit")
        XCTAssertEqual(harness.postTextCount, 1)
    }

    func testCancellationDuringTypePreparationWinsImmediatelyBeforeCommit() throws {
        let request = try runnableRequest(action: "type")
        let harness = BackgroundTargetedInputHarness()
        harness.cancelAfterChecks = 1

        let result = runBackgroundTargetedInputV1(
            request: request, dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(result.failureCode, "cancelled_before_input")
        XCTAssertEqual(harness.postTextCount, 0)
        XCTAssertEqual(harness.cancellationChecks, 2)
    }

    func testOrdinaryKeypressPostsOnceAndReturnsContentFreeUnverifiedAck() throws {
        let request = try runnableRequest(
            action: "keypress", keys: ["left"], modifiers: [])
        let harness = BackgroundTargetedInputHarness()

        let result = runBackgroundTargetedInputV1(
            request: request, dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertEqual(harness.postKeypressCount, 1)
        XCTAssertEqual(harness.postTextCount, 0)
        let encoded = String(
            decoding: try JSONEncoder().encode(result), as: UTF8.self)
        XCTAssertFalse(encoded.contains("left"))
    }

    func testWireDispatchReturnsTypedNotCommittedInvalidRequest() throws {
        var object = try XCTUnwrap(JSONSerialization.jsonObject(
            with: fixture("background_targeted_input.request.type.v1.json"))
            as? [String: Any])
        object["extra"] = true
        let response = dispatchWireRequest(
            try JSONSerialization.data(withJSONObject: object),
            backgroundTargetedInputDependencies:
                BackgroundTargetedInputHarness().dependencies())
        let wire = String(
            decoding: try JSONEncoder().encode(response), as: UTF8.self)

        XCTAssertTrue(wire.contains("invalid_request"))
        XCTAssertTrue(wire.contains("\"input_committed\":false"))
        XCTAssertFalse(wire.contains("fixture input"))
    }

    private func runnableRequest(
        action: String,
        keys: [String] = [],
        modifiers: [String] = []
    ) throws -> BackgroundTargetedInputRequestV1 {
        let payload: Data
        if action == "type" {
            payload = try fixture("background_targeted_input.request.type.v1.json")
        } else {
            payload = try keypressPayload(keys: keys, modifiers: modifiers)
        }
        return try decodeBackgroundTargetedInputRPCRequestV1(
            mutate(payload) { root in
                var params = root["params"] as! [String: Any]
                var input = params["input"] as! [String: Any]
                input["commit_deadline_at"] = "2033-05-18T03:33:21Z"
                params["input"] = input
                root["params"] = params
            }).params
    }

    private func keypressPayload(
        keys: [String],
        modifiers: [String]
    ) throws -> Data {
        try mutate(
            fixture("background_targeted_input.request.type.v1.json")
        ) { root in
            var params = root["params"] as! [String: Any]
            var input = params["input"] as! [String: Any]
            input["action"] = "keypress"
            input["ref"] = NSNull()
            input["path"] = NSNull()
            input["expected_role"] = NSNull()
            input["expected_fingerprint"] = NSNull()
            input["text"] = NSNull()
            input["keys"] = keys
            input["modifiers"] = modifiers
            params["input"] = input
            root["params"] = params
        }
    }

    private func mutate(
        _ data: Data,
        mutation: (inout [String: Any]) -> Void
    ) throws -> Data {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: data) as? [String: Any])
        mutation(&object)
        return try JSONSerialization.data(
            withJSONObject: object, options: [.sortedKeys])
    }

    private func fixture(_ name: String) throws -> Data {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent("testdata/\(name)")
        return try Data(contentsOf: url)
    }
}

private final class BackgroundTargetedInputHarness {
    var canAdmitInput = true
    var authorityFailures: [String?] = [nil, nil, nil, nil]
    var authorityCalls = 0
    var postTextCount = 0
    var postKeypressCount = 0
    var cancelled = false
    var cancelAfterChecks: Int?
    var cancellationChecks = 0
    let deadline = ISO8601DateFormatter().date(from: "2033-05-18T03:33:21Z")!
    var typeVerificationPreparation: TargetBoundTypeVerificationPreparationV1 =
        .unavailable(failureCode: "target_value_readback_unavailable")

    func dependencies() -> BackgroundTargetedInputDependenciesV1 {
        BackgroundTargetedInputDependenciesV1(
            canAdmitInput: { self.canAdmitInput },
            authorityFailure: { _ in
                defer { self.authorityCalls += 1 }
                guard self.authorityCalls < self.authorityFailures.count else {
                    return nil
                }
                return self.authorityFailures[self.authorityCalls]
            },
            postText: { _, _ in
                self.postTextCount += 1
                return .completed(keyPairsCommitted: 1)
            },
            postKeypress: { _, keys, _ in
                self.postKeypressCount += 1
                return .completed(keyPairsCommitted: keys.count)
            },
            prepareTypeVerification: { _, _ in
                self.typeVerificationPreparation
            },
            isCancelled: {
                defer { self.cancellationChecks += 1 }
                if let threshold = self.cancelAfterChecks,
                   self.cancellationChecks >= threshold {
                    return true
                }
                return self.cancelled
            },
            now: { self.deadline.addingTimeInterval(-1) },
            sleep: { _ in })
    }
}
