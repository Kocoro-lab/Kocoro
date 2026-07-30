import ApplicationServices
import XCTest
@testable import ax_server

final class SemanticTextSelectionTests: XCTestCase {
    func testPhysicalSamplerCheckpointsBracketAXMutationVerificationAndVerifiedReturn() throws {
        let harness = SelectionHarness()

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "verified")
        XCTAssertEqual(
            harness.timeline,
            ["physical", "set", "physical", "physical", "observe", "physical", "physical"])
        XCTAssertEqual(harness.physicalInputCalls, 5)
    }

    func testPrecommitMonitoringFailureFailsBeforeSelectionMutation() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [nil]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "failed")
        XCTAssertFalse(result.selectionCommitted)
        XCTAssertEqual(result.phase, "preflight")
        XCTAssertEqual(result.failureCode, "interference_detection_unavailable")
        XCTAssertEqual(harness.setRanges, [])
    }

    func testPrecommitPhysicalInputYieldsWithoutSelectionMutation() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(heldModifierFlags: 1),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertFalse(result.selectionCommitted)
        XCTAssertEqual(result.phase, "user_interference")
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertEqual(harness.setRanges, [])
    }

    func testPhysicalInputDuringAXMutationOverridesSuccessfulSet() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(externalChanges: [(.keyDown, 1)]),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertEqual(harness.setRanges, [.init(location: 5, length: 12)])
        XCTAssertEqual(harness.observeRangeCount, 0)
    }

    func testMonitoringLossAfterAXMutationIsCompletedUnverified() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [SelectionHarness.physicalSnapshot(), nil]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "interference_detection_unavailable")
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(harness.observeRangeCount, 0)
    }

    func testPhysicalInputBeforeTargetedReadPreventsAXObservation() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(externalChanges: [(.mouseMoved, 1)]),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertEqual(harness.observeRangeCount, 0)
    }

    func testPhysicalInputAfterMatchingReadOverridesVerifiedPostcondition() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(externalChanges: [(.keyUp, 1)]),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(harness.observeRangeCount, 1)
    }

    func testMonitoringLossAfterMatchingReadOverridesVerifiedPostcondition() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            nil,
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "interference_detection_unavailable")
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(harness.observeRangeCount, 1)
    }

    func testFinalCheckpointOverridesOtherwiseVerifiedSelection() throws {
        let harness = SelectionHarness()
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(externalChanges: [(.scrollWheel, 1)]),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertNil(result.postcondition)
    }

    func testFinalCheckpointInterferenceOverridesOrdinaryUnverifiedResult() throws {
        let harness = SelectionHarness()
        harness.rangeObservations = [.unsupported]
        harness.physicalInputSnapshots = [
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(),
            SelectionHarness.physicalSnapshot(externalChanges: [(.flagsChanged, 1)]),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "user_interference")
        XCTAssertTrue(result.selectionCommitted)
        XCTAssertEqual(result.failureCode, "physical_input_interference")
        XCTAssertNil(result.postcondition)
    }

    func testPhysicalInterferenceTaggedUnionShapesRoundTripStrictly() throws {
        let results = [
            SemanticTextSelectionResultV1(
                status: "user_interference", selectionCommitted: false,
                phase: "user_interference", failureCode: "physical_input_interference",
                postcondition: nil, selectedRange: nil),
            SemanticTextSelectionResultV1(
                status: "user_interference", selectionCommitted: true,
                phase: "user_interference", failureCode: "physical_input_interference",
                postcondition: nil, selectedRange: nil),
            SemanticTextSelectionResultV1(
                status: "failed", selectionCommitted: false, phase: "preflight",
                failureCode: "interference_detection_unavailable",
                postcondition: nil, selectedRange: nil),
            SemanticTextSelectionResultV1(
                status: "completed_unverified", selectionCommitted: true,
                phase: "post_verification",
                failureCode: "interference_detection_unavailable",
                postcondition: nil, selectedRange: nil),
        ]
        for result in results {
            XCTAssertEqual(
                try decodeSemanticTextSelectionResultV1(JSONEncoder().encode(result)), result)
        }
    }

    func testVerifiedSelectionUsesOnlyAXRangeAndNeverReadsOrEchoesText() throws {
        let harness = SelectionHarness()
        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "verified")
        XCTAssertEqual(result.postcondition, "selected_range_matches")
        XCTAssertEqual(result.selectedRange, .init(location: 5, length: 12))
        XCTAssertEqual(harness.setRanges, [.init(location: 5, length: 12)])
        XCTAssertEqual(harness.valueReadCount, 0)
        XCTAssertFalse(result.retrySafe)
    }

    func testUnsupportedElectronStyleTargetRequiresFramedCoordinateFallback() throws {
        let harness = SelectionHarness()
        harness.target = .init(
            role: "AXTextArea", fingerprint: "axf_fixture_text_area",
            enabled: true, sensitive: false,
            supportsParameterizedTextRange: false, selectedTextRangeSettable: false,
            setSelectedRange: { _ in .attributeUnsupported },
            observeSelectedRange: { _ in .unsupported })
        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "fallback_required")
        XCTAssertEqual(result.failureCode, "ax_text_range_unsupported")
        XCTAssertFalse(result.selectionCommitted)
        XCTAssertNil(result.selectedRange)
        XCTAssertEqual(harness.setRanges, [])
    }

    func testSensitiveTargetIsRejectedBeforeValueOrRangeRead() throws {
        let harness = SelectionHarness()
        harness.target = SelectionHarness.target(sensitive: true)
        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "sensitive_target")
        XCTAssertFalse(result.selectionCommitted)
        XCTAssertEqual(harness.valueReadCount, 0)
        XCTAssertEqual(harness.setRanges, [])
    }

    func testIdentityFailuresNeverMutate() throws {
        let cases: [(String, (SelectionHarness) -> Void)] = [
            ("process_identity_mismatch", { $0.bundleID = "other" }),
            ("window_not_found", { $0.window = .missing }),
            ("path_not_found", { $0.target = nil }),
            ("role_mismatch", { $0.target = SelectionHarness.target(role: "AXButton") }),
            ("fingerprint_mismatch", { $0.target = SelectionHarness.target(fingerprint: "other") }),
            ("fingerprint_ambiguous", { $0.fingerprintCount = 2 }),
        ]
        for (code, configure) in cases {
            let harness = SelectionHarness()
            configure(harness)
            let result = runSemanticTextSelection(
                request: try request(), dependencies: harness.dependencies())
            XCTAssertEqual(result.failureCode, code, code)
            XCTAssertFalse(result.selectionCommitted, code)
            XCTAssertEqual(harness.setRanges, [], code)
        }
    }

    func testAXFailureDoesNotClaimSuccess() throws {
        let harness = SelectionHarness()
        harness.setResult = .cannotComplete
        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())
        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.failureCode, "ax_selection_failed")
        XCTAssertFalse(result.selectionCommitted)
    }

    func testSelectionPollsOnlyTargetedRangeUntilDelayedAXStateMatches() throws {
        let harness = SelectionHarness()
        harness.rangeObservations = [
            .unavailable,
            .range(.init(location: 0, length: 3)),
            .range(.init(location: 5, length: 12)),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "verified")
        XCTAssertEqual(result.postcondition, "selected_range_matches")
        XCTAssertEqual(harness.observeRangeCount, 3)
        XCTAssertEqual(harness.verificationTimeouts, [0.1, 0.1, 0.1])
        XCTAssertEqual(harness.sleepCount, 2)
    }

    func testPersistentSelectionMismatchIsExplicitAndBounded() throws {
        let harness = SelectionHarness()
        harness.rangeObservations = [
            .range(.init(location: 1, length: 2)),
        ]

        let result = runSemanticTextSelection(
            request: try request(), dependencies: harness.dependencies())

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.failureCode, "selected_range_mismatch")
        XCTAssertEqual(result.selectedRange, .init(location: 1, length: 2))
        XCTAssertEqual(
            harness.observeRangeCount, targetedAXPostconditionBudgetV1.maxAttempts)
        XCTAssertEqual(
            harness.sleepCount, targetedAXPostconditionBudgetV1.maxAttempts - 1)
    }

    private func request() throws -> SemanticTextSelectionRequestV1 {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent(
                "testdata/semantic_text_selection.request.v1.json")
        return try decodeSemanticTextSelectionRPCRequestV1(Data(contentsOf: url)).params
    }
}

private final class SelectionHarness {
    var pidLive = true
    var bundleID: String? = "com.example.fixture"
    var window: SemanticTextSelectionWindowResolution = .unique
    var target: SemanticTextSelectionTarget? = SelectionHarness.target()
    var fingerprintCount = 1
    var setResult: AXError = .success
    var setRanges: [SemanticTextRangeV1] = []
    var valueReadCount = 0
    var rangeObservations: [SemanticTextRangeObservation] = []
    var observeRangeCount = 0
    var sleepCount = 0
    var verificationTimeouts: [TimeInterval] = []
    var physicalInputCalls = 0
    var physicalInputSnapshots: [PhysicalInputInterferenceSnapshotV1?] = [
        physicalSnapshot(), physicalSnapshot(), physicalSnapshot(),
        physicalSnapshot(), physicalSnapshot(),
    ]
    var timeline: [String] = []

    func dependencies() -> SemanticTextSelectionDependencies {
        SemanticTextSelectionDependencies(
            isPIDLive: { _ in self.pidLive },
            bundleIDForPID: { _ in self.bundleID },
            resolveWindow: { _, _ in self.window },
            resolveTarget: { _, _, _ in
                guard let target = self.target else { return nil }
                return SemanticTextSelectionTarget(
                    role: target.role, fingerprint: target.fingerprint,
                    enabled: target.enabled, sensitive: target.sensitive,
                    supportsParameterizedTextRange: target.supportsParameterizedTextRange,
                    selectedTextRangeSettable: target.selectedTextRangeSettable,
                    setSelectedRange: { range in
                        self.timeline.append("set")
                        self.setRanges.append(range)
                        return self.setResult
                    },
                    observeSelectedRange: { timeout in
                        self.timeline.append("observe")
                        self.verificationTimeouts.append(timeout)
                        defer { self.observeRangeCount += 1 }
                        if self.rangeObservations.isEmpty {
                            return .range(self.setRanges.last ?? .init(location: 0, length: 0))
                        }
                        return self.rangeObservations[
                            min(self.observeRangeCount, self.rangeObservations.count - 1)]
                    })
            },
            countFingerprint: { _, _, _ in self.fingerprintCount },
            observePhysicalInput: {
                self.timeline.append("physical")
                let value = self.physicalInputSnapshots[
                    min(self.physicalInputCalls, self.physicalInputSnapshots.count - 1)]
                self.physicalInputCalls += 1
                return value
            },
            now: { semanticSelectionTestDate("2026-07-22T12:03:30Z") },
            sleep: { _ in self.sleepCount += 1 })
    }

    static func physicalSnapshot(
        externalChanges: [(CGEventType, UInt32)] = [],
        heldModifierFlags: UInt64 = 0
    ) -> PhysicalInputInterferenceSnapshotV1 {
        var counters = Array(
            repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        let syntheticCounters = Array(
            repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count)
        for (eventType, delta) in externalChanges {
            counters[physicalInputHIDEventTypesV1.firstIndex(of: eventType)!] += delta
        }
        return .init(
            pointer: .init(x: 100, y: 200),
            hidEventCounters: counters,
            syntheticEventCounters: syntheticCounters,
            heldModifierFlags: heldModifierFlags)
    }

    static func target(
        role: String = "AXTextArea",
        fingerprint: String = "axf_fixture_text_area",
        sensitive: Bool = false
    ) -> SemanticTextSelectionTarget {
        .init(
            role: role, fingerprint: fingerprint, enabled: true,
            sensitive: sensitive, supportsParameterizedTextRange: true,
            selectedTextRangeSettable: true,
            setSelectedRange: { _ in .success },
            observeSelectedRange: { _ in .range(.init(location: 5, length: 12)) })
    }
}

private func semanticSelectionTestDate(_ value: String) -> Date {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: value) { return date }
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.date(from: value)!
}
