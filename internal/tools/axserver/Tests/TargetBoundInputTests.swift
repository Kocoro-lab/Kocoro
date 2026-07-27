import AppKit
import CoreGraphics
import Foundation
import XCTest
@testable import ax_server

final class TargetBoundInputTests: XCTestCase {
    func testFocusedAXWindowOutranksSameProcessCGOverlayOrSiblingWindow() {
        XCTAssertTrue(targetBoundInputWindowAuthorityMatches(
            requestedWindowID: 7001,
            focusedAXWindowID: 7001,
            frontmostCGWindowID: 7002))
        XCTAssertFalse(targetBoundInputWindowAuthorityMatches(
            requestedWindowID: 7001,
            focusedAXWindowID: 7002,
            frontmostCGWindowID: 7001))
    }

    func testCGFrontmostWindowRemainsFallbackWithoutAXWindowIdentity() {
        XCTAssertTrue(targetBoundInputWindowAuthorityMatches(
            requestedWindowID: 7001,
            focusedAXWindowID: nil,
            frontmostCGWindowID: 7001))
        XCTAssertFalse(targetBoundInputWindowAuthorityMatches(
            requestedWindowID: 7001,
            focusedAXWindowID: nil,
            frontmostCGWindowID: 7002))
    }

    func testStrictWireAdmitsEveryFunctionKeySupportedByInputDriver() {
        for number in 1...12 {
            let key = "f\(number)"
            XCTAssertTrue(strictInputKeyV1(key))
            XCTAssertNotNil(keyCodeMap[key])
        }
        XCTAssertFalse(strictInputKeyV1("f13"))
    }

    func testProductionTypeAlwaysUsesClipboardToBypassActiveIME() {
        let dependencies = productionTargetBoundInputDependenciesV1(isCancelled: { false })

        XCTAssertTrue(dependencies.requiresClipboard("Zoro"))
        XCTAssertTrue(dependencies.requiresClipboard("你好👋"))
    }

    func testPhysicalAssessmentWaitsForDeliveredKeyboardCounters() {
        let baseline = TargetBoundInputHarness.physicalSnapshot()
        let delivered = TargetBoundInputHarness.physicalSnapshot(
            changes: [(.keyDown, 1), (.keyUp, 1)]
        )
        var observations = [baseline, delivered]
        var settleCount = 0

        let result = targetBoundInputAssessPhysicalInputV1(
            baseline: baseline,
            expectedPointer: baseline.pointer,
            expectedSyntheticEvents: [(.keyDown, 1), (.keyUp, 1)],
            observe: { observations.removeFirst() },
            settle: { settleCount += 1 },
            maximumSettleAttempts: 2
        )

        XCTAssertEqual(result.assessment, .unchanged)
        XCTAssertEqual(result.snapshot, delivered)
        XCTAssertEqual(settleCount, 1)
    }

    func testAXToCGWindowBoundsUseTwoPointCalibrationWithoutClaimingExactQuartz() {
        let ax = DisplayTopologyRectV1(x: 100, y: 200, width: 800, height: 600)
        XCTAssertTrue(targetBoundInputAXBoundsCorrelateWithCG(
            ax, DisplayTopologyRectV1(x: 102, y: 198, width: 798, height: 602)))
        XCTAssertFalse(targetBoundInputAXBoundsCorrelateWithCG(
            ax, DisplayTopologyRectV1(x: 102.01, y: 200, width: 800, height: 600)))
    }

    func testExpectedTypeEditUsesUTF16SelectionAndRejectsOutOfBoundsRange() {
        XCTAssertEqual(
            targetBoundExpectedTextValueV1(
                before: "A😀B", selectedRange: NSRange(location: 1, length: 2),
                insertedText: "猫"),
            "A猫B")
        XCTAssertNil(targetBoundExpectedTextValueV1(
            before: "A😀B", selectedRange: NSRange(location: 2, length: 3),
            insertedText: "x"))
    }

    func testCanonicalCrossLanguageFixtures() throws {
        let type = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.type.v1.json"))
        XCTAssertEqual(type.id, 902)
        XCTAssertEqual(type.params.action, "type")
        XCTAssertEqual(type.params.ref, "e2")
        XCTAssertEqual(type.params.expectedFingerprint, "axf_e2")
        let coordinateType = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.coordinate_type.v1.json"))
        XCTAssertNil(coordinateType.params.ref)
        XCTAssertNil(coordinateType.params.path)
        let hotkey = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.hotkey.v1.json"))
        XCTAssertEqual(hotkey.id, 903)
        XCTAssertEqual(hotkey.params.modifiers ?? [], ["command", "shift"])
        let keypress = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.keypress.v1.json"))
        XCTAssertEqual(keypress.id, 904)
        XCTAssertEqual(keypress.params.keys ?? [], ["p", "a", "g", "e", "down"])
        XCTAssertEqual(keypress.params.modifiers ?? [], ["command", "shift"])

        let typeHarness = TargetBoundInputHarness()
        typeHarness.forceClipboard = true
        let typeResult = runTargetBoundInput(
            request: type.params, dependencies: typeHarness.dependencies())
        XCTAssertEqual(
            try jsonObject(JSONEncoder().encode(typeResult)),
            try jsonObject(fixture(
                "target_bound_input.response.type.completed_unverified.v1.json")))

        let verifiedHarness = TargetBoundInputHarness()
        verifiedHarness.forceClipboard = true
        verifiedHarness.typeVerificationPreparation = .ready(.init(observe: { _ in .matched }))
        let verifiedTypeResult = runTargetBoundInput(
            request: type.params, dependencies: verifiedHarness.dependencies())
        XCTAssertEqual(
            try jsonObject(JSONEncoder().encode(verifiedTypeResult)),
            try jsonObject(fixture(
                "target_bound_input.response.type.verified.v1.json")))

        let hotkeyHarness = TargetBoundInputHarness()
        hotkeyHarness.authorityFailures = ["frontmost_process_mismatch"]
        let hotkeyResult = runTargetBoundInput(
            request: hotkey.params, dependencies: hotkeyHarness.dependencies())
        XCTAssertEqual(
            try jsonObject(JSONEncoder().encode(hotkeyResult)),
            try jsonObject(fixture(
                "target_bound_input.response.hotkey.user_interference.v1.json")))
    }

    func testWindowBoundTypeNeverRestoresLostFocusAndStaysUnverified() throws {
        let request = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.coordinate_type.v1.json")).params

        let harness = TargetBoundInputHarness()
        harness.forceClipboard = true
        let result = runTargetBoundInput(
            request: request, dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertEqual(harness.postCount, 1)

        let lostWindow = TargetBoundInputHarness()
        lostWindow.authorityFailures = ["frontmost_process_mismatch"]
        lostWindow.restoreAuthoritySucceeds = true
        let lostResult = runTargetBoundInput(
            request: request, dependencies: lostWindow.dependencies())
        XCTAssertEqual(lostResult.status, "failed")
        XCTAssertEqual(lostResult.failureCode, "frontmost_process_mismatch")
        XCTAssertEqual(lostWindow.restoreAuthorityCount, 0)
    }

    func testCancellationMarkerMatchesCrossLanguageFixture() throws {
        let object = try XCTUnwrap(JSONSerialization.jsonObject(
            with: fixture("target_bound_input.cancellation_marker.v1.json"))
            as? [String: Any])
        let requestID = try XCTUnwrap(object["request_id"] as? Int64)
        let keypress = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.keypress.v1.json"))
        XCTAssertEqual(
            targetBoundInputCancellationMarkerURLV1(
                requestID: requestID, request: keypress.params).lastPathComponent,
            try XCTUnwrap(object["basename"] as? String))
    }

    func testKeypressRunsEveryOrdinaryKeyAndReturnsContentFreeAck() throws {
        let request = try decodeTargetBoundInputRPCRequestV1(
            fixture("target_bound_input.request.keypress.v1.json")).params
        let harness = TargetBoundInputHarness()
        harness.physicalInputSnapshots = [
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(
                changes: [(.keyDown, 7), (.keyUp, 7)]),
        ]

        let result = runTargetBoundInput(
            request: request, dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.action, "keypress")
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(harness.postCount, 5)
        let encoded = String(
            data: try JSONEncoder().encode(result), encoding: .utf8)!
        XCTAssertFalse(encoded.contains("page"))
    }

    func testKeypressRejectsAliasDuplicateModifierAndUnknownKeyOnStrictWire() throws {
        let source = try XCTUnwrap(JSONSerialization.jsonObject(
            with: fixture("target_bound_input.request.keypress.v1.json"))
            as? [String: Any])
        for (modifiers, keys) in [
            (["cmd"], ["a"]),
            (["command", "command"], ["a"]),
            (["command"], ["CAPSLOCK"]),
        ] {
            var envelope = source
            var params = envelope["params"] as! [String: Any]
            params["modifiers"] = modifiers
            params["keys"] = keys
            envelope["params"] = params
            let payload = try JSONSerialization.data(
                withJSONObject: envelope, options: [.sortedKeys])
            XCTAssertThrowsError(try decodeTargetBoundInputRPCRequestV1(payload))
        }
    }

    func testHotkeyRevalidatesExactTargetImmediatelyBeforeCommit() throws {
        let harness = TargetBoundInputHarness()
        harness.authorityFailures = [nil, "frontmost_process_mismatch"]

        let result = runTargetBoundInput(
            request: try request(action: "hotkey"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "frontmost_process_mismatch")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(harness.prepareHotkeyCount, 1)
        XCTAssertEqual(harness.postCount, 0)
        XCTAssertEqual(harness.authorityCalls, 2)
    }

    func testHotkeyRestoresExactTargetAfterApprovalFocusShift() throws {
        let harness = TargetBoundInputHarness()
        harness.authorityFailures = ["frontmost_process_mismatch", nil, nil]
        harness.restoreAuthoritySucceeds = true

        let result = runTargetBoundInput(
            request: try request(action: "hotkey"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(harness.restoreAuthorityCount, 1)
        XCTAssertEqual(harness.postCount, 1)
        XCTAssertEqual(harness.authorityCalls, 3)
    }

    func testHotkeyYieldsBeforePostingWhenPhysicalModifierIsAlreadyHeld() throws {
        let harness = TargetBoundInputHarness()
        harness.physicalInputSnapshots = [
            .init(
                pointer: .init(x: 100, y: 200),
                hidEventCounters: [10],
                heldModifierFlags: 1),
        ]

        let result = runTargetBoundInput(
            request: try request(action: "hotkey"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testHotkeyReportsPhysicalCompetitionDuringHeldInputSequence() throws {
        let harness = TargetBoundInputHarness()
        harness.physicalInputSnapshots = [
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(
                changes: [(.keyDown, 1), (.keyUp, 1)],
                externalChanges: [(.mouseMoved, 1)]),
        ]

        let result = runTargetBoundInput(
            request: try request(action: "hotkey"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(harness.postCount, 1)
    }

    func testHotkeyFailsClosedWhenInterferenceObservationIsUnavailable() throws {
        let harness = TargetBoundInputHarness()
        harness.physicalInputSnapshots = [nil]

        let result = runTargetBoundInput(
            request: try request(action: "hotkey"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "interference_detection_unavailable")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testTypeRejectsWrongFrontmostTargetBeforeClipboardOrCGEvent() throws {
        let harness = TargetBoundInputHarness()
        harness.authorityFailures = ["frontmost_window_mismatch"]

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "sensitive text 🔐"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "frontmost_window_mismatch")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertFalse(result.clipboardTouched)
        XCTAssertEqual(harness.prepareClipboardCount, 0)
        XCTAssertEqual(harness.prepareDirectCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testRecoveryBlockRejectsBeforeAuthorityClipboardPreparationOrInput() throws {
        let harness = TargetBoundInputHarness()
        harness.canAdmitInput = false

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "sensitive text 🔐"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "input_recovery_blocked")
        XCTAssertFalse(result.clipboardTouched)
        XCTAssertEqual(harness.authorityCalls, 0)
        XCTAssertEqual(harness.prepareClipboardCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    func testClipboardTypeRestoresClipboardAndReturnsContentFreeTypedAck() throws {
        let harness = TargetBoundInputHarness()
        let secret = "never echo this 🔐"

        let result = runTargetBoundInput(
            request: try request(action: "type", text: secret),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertTrue(result.clipboardTouched)
        XCTAssertTrue(result.clipboardRestored)
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertEqual(harness.postCount, 1)
        XCTAssertEqual(harness.restoreCount, 1)
        let encoded = try JSONEncoder().encode(result)
        XCTAssertFalse(String(decoding: encoded, as: UTF8.self).contains(secret))
    }

    func testSafeTargetBoundTypeVerifiesExactExpectedEditAfterAXSettles() throws {
        let harness = TargetBoundInputHarness()
        harness.typeVerificationPreparation = .ready(.init(observe: { timeout in
            harness.verifyTimeouts.append(timeout)
            defer { harness.verifyObserveCount += 1 }
            return harness.verifyObserveCount < 2
                ? .mismatch
                : .matched
        }))
        let secret = "must remain redacted"

        let result = runTargetBoundInput(
            request: try request(action: "type", text: secret),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "verified")
        XCTAssertEqual(result.postcondition, "target_value_matches_expected_edit")
        XCTAssertNil(result.failureCode)
        XCTAssertTrue(result.inputCommitted)
        XCTAssertEqual(harness.verifyObserveCount, 3)
        XCTAssertEqual(harness.verifyTimeouts, [0.1, 0.1, 0.1])
        XCTAssertEqual(harness.verifySleepCount, 2)
        XCTAssertFalse(String(decoding: try JSONEncoder().encode(result), as: UTF8.self).contains(secret))
    }

    func testPhysicalCompetitionDuringVerifierWindowPreventsVerifiedClaim() throws {
        let harness = TargetBoundInputHarness()
        harness.typeVerificationPreparation = .ready(.init(observe: { _ in
            harness.verifyObserveCount += 1
            return .matched
        }))
        harness.physicalInputSnapshots = [
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(
                changes: [(.keyDown, 1), (.keyUp, 1)]),
            TargetBoundInputHarness.physicalSnapshot(
                changes: [(.keyDown, 1), (.keyUp, 1)]),
            TargetBoundInputHarness.physicalSnapshot(
                changes: [(.keyDown, 1), (.keyUp, 1)],
                externalChanges: [(.mouseMoved, 1)]),
        ]

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "hello"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(harness.verifyObserveCount, 0)
    }

    func testSensitiveTargetBoundTypeNeverReadsBackAndStaysUnverified() throws {
        let harness = TargetBoundInputHarness()
        harness.typeVerificationPreparation = .unavailable(
            failureCode: "verification_redacted_sensitive_target")

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "secret"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "verification_redacted_sensitive_target")
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(harness.verifyObserveCount, 0)
        XCTAssertEqual(harness.verifySleepCount, 0)
    }

    func testPersistentTargetValueMismatchIsBoundedAndNeverClaimsVerified() throws {
        let harness = TargetBoundInputHarness()
        harness.typeVerificationPreparation = .ready(.init(observe: { _ in
            harness.verifyObserveCount += 1
            return .mismatch
        }))

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "hello"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "target_value_mismatch")
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(
            harness.verifyObserveCount, targetedAXPostconditionBudgetV1.maxAttempts)
        XCTAssertEqual(
            harness.verifySleepCount, targetedAXPostconditionBudgetV1.maxAttempts - 1)
    }

    func testClipboardTypeRestoresWithoutInputWhenTargetChangesBeforePaste() throws {
        let harness = TargetBoundInputHarness()
        harness.authorityFailures = [nil, "frontmost_window_mismatch"]

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "sensitive text 🔐"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "frontmost_window_mismatch")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertTrue(result.clipboardTouched)
        XCTAssertTrue(result.clipboardRestored)
        XCTAssertEqual(harness.postCount, 0)
        XCTAssertEqual(harness.restoreCount, 1)
        XCTAssertEqual(harness.authorityCalls, 2)
    }

    func testClipboardOwnershipLossNeverOverwritesNewUserContent() throws {
        let pasteboard = NSPasteboard(
            name: NSPasteboard.Name("target-bound-input-tests-\(UUID().uuidString)"))
        defer { pasteboard.clearContents() }
        pasteboard.clearContents()
        XCTAssertTrue(pasteboard.setString("original", forType: .string))
        guard case let .prepared(transaction) = makeTargetBoundClipboardTransaction(
            "temporary secret",
            pasteboard: pasteboard,
            waitBeforeRestore: {}) else {
            return XCTFail("clipboard preparation unexpectedly failed")
        }
        XCTAssertEqual(pasteboard.string(forType: .string), "temporary secret")

        pasteboard.clearContents()
        XCTAssertTrue(pasteboard.setString("new user value", forType: .string))
        XCTAssertEqual(transaction.post(), .ownershipLost)
        XCTAssertFalse(transaction.restore())
        XCTAssertEqual(pasteboard.string(forType: .string), "new user value")
    }

    func testClipboardOwnershipLossBeforePasteIsTypedWithoutInputCommit() throws {
        let harness = TargetBoundInputHarness()
        harness.clipboardPostOutcome = .ownershipLost
        harness.physicalInputSnapshots = [
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(),
            TargetBoundInputHarness.physicalSnapshot(),
        ]
        let result = runTargetBoundInput(
            request: try request(action: "type", text: "sensitive text 🔐"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "clipboard_ownership_lost_before_input")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertTrue(result.clipboardTouched)
        XCTAssertFalse(result.clipboardRestored)
        XCTAssertEqual(harness.postCount, 1)
        XCTAssertEqual(harness.restoreCount, 0)
    }

    func testTypeRevalidatesFocusedElementAuthorityBeforeClipboardAndPaste() throws {
        let harness = TargetBoundInputHarness()
        harness.authorityFailures = [nil, "focused_element_mismatch"]
        let result = runTargetBoundInput(
            request: try request(action: "type", text: "sensitive text 🔐"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "focused_element_mismatch")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(harness.postCount, 0)
        XCTAssertEqual(harness.restoreCount, 1)
    }

    func testTypeVerifierPreparationTargetChangeAbortsBeforePasteAndRestoresClipboard() throws {
        let harness = TargetBoundInputHarness()
        harness.typeVerificationPreparation = .failed(
            failureCode: "focused_element_mismatch")

        let result = runTargetBoundInput(
            request: try request(action: "type", text: "sensitive text 🔐"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "focused_element_mismatch")
        XCTAssertFalse(result.inputCommitted)
        XCTAssertEqual(harness.postCount, 0)
        XCTAssertEqual(harness.restoreCount, 1)
    }

    func testClipboardOwnershipLossAfterCommitIsTypedAndContentFree() throws {
        let harness = TargetBoundInputHarness()
        harness.restoreSucceeds = false
        harness.typeVerificationPreparation = .ready(.init(observe: { _ in
            harness.verifyObserveCount += 1
            return .matched
        }))
        let secret = "must remain redacted 🔐"
        let result = runTargetBoundInput(
            request: try request(action: "type", text: secret),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.inputCommitted)
        XCTAssertTrue(result.clipboardTouched)
        XCTAssertFalse(result.clipboardRestored)
        XCTAssertEqual(result.failureCode, "clipboard_restore_failed_after_commit")
        XCTAssertNil(result.postcondition)
        // Verification now runs BEFORE the restore is attempted: restoring first
        // lets a busy target service our Cmd+V after the pasteboard has already
        // been reverted, pasting the user's previous clipboard content. A failed
        // restore is still reported, it is just discovered after the read-back.
        XCTAssertEqual(harness.verifyObserveCount, 1)
        XCTAssertFalse(String(decoding: try JSONEncoder().encode(result), as: UTF8.self).contains(secret))
    }

    /// The pasteboard must not be reverted until the verifier has read the
    /// target back. Restoring first lets a busy or beachballing app service our
    /// synthetic Cmd+V afterwards, pasting the user's PREVIOUS clipboard content
    /// into the target field — detected only as a value mismatch, and not at all
    /// when the target is sensitive and read-back is skipped.
    func testClipboardIsRestoredOnlyAfterVerificationReadsTheTarget() throws {
        let harness = TargetBoundInputHarness()
        harness.forceClipboard = true
        var restoreCountWhenVerified = -1
        harness.typeVerificationPreparation = .ready(.init(observe: { _ in
            harness.verifyObserveCount += 1
            restoreCountWhenVerified = harness.restoreCount
            return .matched
        }))
        let result = runTargetBoundInput(
            request: try request(action: "type", text: "penguin"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "verified")
        XCTAssertTrue(result.clipboardTouched)
        XCTAssertTrue(result.clipboardRestored)
        XCTAssertEqual(harness.verifyObserveCount, 1)
        XCTAssertEqual(
            restoreCountWhenVerified, 0,
            "clipboard was restored before the verifier read the target back")
        XCTAssertEqual(harness.restoreCount, 1, "restore must still run exactly once")
    }

    func testStrictDecoderRejectsUnknownDuplicateAndEscapedDuplicateMembers() throws {
        let valid = try payload(action: "hotkey")
        var object = try XCTUnwrap(JSONSerialization.jsonObject(with: valid) as? [String: Any])
        object["extra"] = true
        XCTAssertThrowsError(try decodeTargetBoundInputRPCRequestV1(
            JSONSerialization.data(withJSONObject: object)))

        let text = String(decoding: valid, as: UTF8.self)
        XCTAssertThrowsError(try decodeTargetBoundInputRPCRequestV1(Data(
            text.replacingOccurrences(of: "\"id\":901", with: "\"id\":901,\"id\":901")
                .utf8)))
        XCTAssertThrowsError(try decodeTargetBoundInputRPCRequestV1(Data(
            text.replacingOccurrences(of: "\"id\":901", with: "\"id\":901,\"\\u0069d\":901")
                .utf8)))
    }

    func testWireDispatchReturnsContentFreeTypedInvalidRequest() throws {
        let secret = "must not appear 🔐"
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: payload(action: "type", text: secret))
                as? [String: Any])
        object["extra"] = true
        let response = dispatchWireRequest(
            try JSONSerialization.data(withJSONObject: object),
            targetBoundInputDependencies: TargetBoundInputHarness().dependencies())
        let encoded = try JSONEncoder().encode(response)
        let wire = String(decoding: encoded, as: UTF8.self)
        XCTAssertFalse(wire.contains(secret))
        XCTAssertTrue(wire.contains("invalid_request"))
        XCTAssertTrue(wire.contains("input_committed"))
    }

    func testExpiredRequestHasNoPreparedOrCommittedSideEffect() throws {
        let harness = TargetBoundInputHarness()
        harness.now = Date(timeIntervalSince1970: 2_000_000_010)
        let result = runTargetBoundInput(
            request: try request(action: "hotkey", deadline: "1970-01-24T03:33:20Z"),
            dependencies: harness.dependencies())

        XCTAssertEqual(result.failureCode, "request_expired")
        XCTAssertEqual(harness.authorityCalls, 0)
        XCTAssertEqual(harness.prepareHotkeyCount, 0)
        XCTAssertEqual(harness.postCount, 0)
    }

    private func request(
        action: String,
        text: String? = nil,
        deadline: String = "2033-05-18T03:33:21Z"
    ) throws -> TargetBoundInputRequestV1 {
        try decodeTargetBoundInputRPCRequestV1(
            payload(action: action, text: text, deadline: deadline)).params
    }

    private func payload(
        action: String,
        text: String? = nil,
        deadline: String = "2033-05-18T03:33:21Z"
    ) throws -> Data {
        var params: [String: Any] = [
            "schema_version": 1,
            "pid": 42,
            "bundle_id": "com.apple.Notes",
            "window_id": 7001,
            "expected_window_ax_bounds": [
                "x": 100.0, "y": 200.0, "width": 800.0, "height": 600.0,
            ],
            "action": action,
            "commit_deadline_at": deadline,
            "ref": NSNull(),
            "path": NSNull(),
            "expected_role": NSNull(),
            "expected_fingerprint": NSNull(),
            "text": NSNull(),
            "key": NSNull(),
            "keys": NSNull(),
            "modifiers": NSNull(),
        ]
        if action == "type" {
            params["text"] = text ?? "hello"
            params["ref"] = "e2"
            params["path"] = "window[0]/AXTextField[0]"
            params["expected_role"] = "AXTextField"
            params["expected_fingerprint"] = "axf_e2"
        } else {
            params["key"] = "p"
            params["modifiers"] = ["command", "shift"]
        }
        return try JSONSerialization.data(withJSONObject: [
            "id": 901, "method": "target_bound_input", "params": params,
        ], options: [.sortedKeys])
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

private final class TargetBoundInputHarness {
    var canAdmitInput = true
    var authorityFailures: [String?] = [nil, nil]
    var authorityCalls = 0
    var restoreAuthoritySucceeds = false
    var restoreAuthorityCount = 0
    var prepareHotkeyCount = 0
    var prepareDirectCount = 0
    var prepareClipboardCount = 0
    var postCount = 0
    var restoreCount = 0
    var restoreSucceeds = true
    var forceClipboard = false
    var clipboardPostOutcome: TargetBoundClipboardPostOutcome = .committed
    var now = Date(timeIntervalSince1970: 2_000_000_000)
    var typeVerificationPreparation: TargetBoundTypeVerificationPreparationV1 =
        .unavailable(failureCode: "postcondition_not_declared")
    var verifyObserveCount = 0
    var verifySleepCount = 0
    var verifyTimeouts: [TimeInterval] = []
    var physicalInputCalls = 0
    var physicalInputSnapshots: [PhysicalInputInterferenceSnapshotV1?] = [
        physicalSnapshot(),
        physicalSnapshot(),
        physicalSnapshot(changes: [(.keyDown, 1), (.keyUp, 1)]),
        physicalSnapshot(changes: [(.keyDown, 1), (.keyUp, 1)]),
    ]

    func dependencies() -> TargetBoundInputDependencies {
        TargetBoundInputDependencies(
            canAdmitInput: { self.canAdmitInput },
            authorityFailure: { _ in
                defer { self.authorityCalls += 1 }
                guard self.authorityCalls < self.authorityFailures.count else { return nil }
                return self.authorityFailures[self.authorityCalls]
            },
            restoreAuthority: { _, _ in
                self.restoreAuthorityCount += 1
                return self.restoreAuthoritySucceeds
            },
            requiresClipboard: {
                self.forceClipboard ||
                    $0.unicodeScalars.contains { $0.value > 0x7f } || $0.count > 20
            },
            prepareHotkey: { _, _ in
                self.prepareHotkeyCount += 1
                return TargetBoundPreparedInput { self.postCount += 1; return true }
            },
            prepareKeypress: { keys, _ in
                TargetBoundPreparedKeySequenceV1(post: {
                    self.postCount += keys.count
                    return .completed(keyPairsCommitted: keys.count)
                })
            },
            prepareDirectText: { _ in
                self.prepareDirectCount += 1
                return TargetBoundPreparedInput { self.postCount += 1; return true }
            },
            prepareClipboardText: { _ in
                self.prepareClipboardCount += 1
                return .prepared(TargetBoundClipboardTransaction(
                    post: {
                        self.postCount += 1
                        return self.clipboardPostOutcome
                    },
                    restore: {
                        self.restoreCount += 1
                        return self.restoreSucceeds
                    }))
            },
            prepareTypeVerification: { _, _ in self.typeVerificationPreparation },
            observePhysicalInput: {
                let value = self.physicalInputSnapshots[
                    min(self.physicalInputCalls, self.physicalInputSnapshots.count - 1)]
                self.physicalInputCalls += 1
                return value
            },
            now: { self.now },
            sleep: { _ in self.verifySleepCount += 1 })
    }

    static func physicalSnapshot(
        pointer: CoordinateMouseEventPointV1 = .init(x: 100, y: 200),
        changes: [(CGEventType, UInt32)] = [],
        externalChanges: [(CGEventType, UInt32)] = []
    ) -> PhysicalInputInterferenceSnapshotV1 {
        var counters = Array(
            repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        var syntheticCounters = Array(
            repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count)
        for (eventType, delta) in changes {
            let index = physicalInputHIDEventTypesV1.firstIndex(of: eventType)!
            counters[index] += delta
            syntheticCounters[index] += delta
        }
        for (eventType, delta) in externalChanges {
            counters[physicalInputHIDEventTypesV1.firstIndex(of: eventType)!] += delta
        }
        return .init(
            pointer: pointer,
            hidEventCounters: counters,
            syntheticEventCounters: syntheticCounters)
    }
}
