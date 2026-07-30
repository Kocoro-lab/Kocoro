import ApplicationServices
import XCTest
@testable import ax_server

final class SemanticPressTests: XCTestCase {
    func testFallbackPolicyRejectsAnythingExceptNone() {
        XCTAssertNoThrow(try makeRequest(fallbackPolicy: "none"))
        XCTAssertThrowsError(try makeRequest(fallbackPolicy: "synthetic"))
    }

    func testPreflightFailuresNeverCommitPress() throws {
        enum Failure {
            case pid, windowMissing, windowAmbiguous, path, role, fingerprint
            case fingerprintMissing, fingerprintDuplicate, enabledUnknown, disabled, action
        }
        let cases: [(String, String, Failure)] = [
            ("pid", "pid_not_live", .pid),
            ("window missing", "window_not_found", .windowMissing),
            ("window ambiguous", "window_ambiguous", .windowAmbiguous),
            ("path", "path_not_found", .path),
            ("role", "role_mismatch", .role),
            ("fingerprint", "fingerprint_mismatch", .fingerprint),
            ("fingerprint missing", "fingerprint_not_found", .fingerprintMissing),
            ("duplicate fingerprint", "fingerprint_ambiguous", .fingerprintDuplicate),
            ("enabled unknown", "enabled_unknown", .enabledUnknown),
            ("disabled", "target_disabled", .disabled),
            ("action", "ax_press_unavailable", .action),
        ]

        for (name, code, failure) in cases {
            var pressCount = 0
            let press: () -> AXError = {
                pressCount += 1
                return AXError.success
            }
            let dependencies: SemanticPressDependencies
            switch failure {
            case .pid:
                dependencies = Self.dependencies(pidLive: false, performPress: press)
            case .windowMissing:
                dependencies = Self.dependencies(window: .missing, performPress: press)
            case .windowAmbiguous:
                dependencies = Self.dependencies(window: .ambiguous, performPress: press)
            case .path:
                dependencies = Self.dependencies(target: nil, performPress: press)
            case .role:
                dependencies = Self.dependencies(target: Self.target(role: "AXLink"), performPress: press)
            case .fingerprint:
                dependencies = Self.dependencies(target: Self.target(fingerprint: "axf_other"), performPress: press)
            case .fingerprintMissing:
                dependencies = Self.dependencies(fingerprintCount: 0, performPress: press)
            case .fingerprintDuplicate:
                dependencies = Self.dependencies(fingerprintCount: 2, performPress: press)
            case .enabledUnknown:
                dependencies = Self.dependencies(target: Self.target(enabled: nil), performPress: press)
            case .disabled:
                dependencies = Self.dependencies(target: Self.target(enabled: false), performPress: press)
            case .action:
                dependencies = Self.dependencies(target: Self.target(actions: []), performPress: press)
            }
            let result = runSemanticPress(try makeRequest(), dependencies: dependencies)
            XCTAssertEqual(result.status, "failed", name)
            XCTAssertEqual(result.phase, "preflight", name)
            XCTAssertEqual(result.failureCode, code, name)
            XCTAssertFalse(result.pressCommitted, name)
            XCTAssertEqual(pressCount, 0, "\(name) executed AXPress during preflight")
        }
    }

    func testAXPressErrorIsStructuredFailure() throws {
        let result = runSemanticPress(
            try makeRequest(),
            dependencies: Self.dependencies(performPress: { .actionUnsupported }))

        XCTAssertEqual(result.status, "failed")
        XCTAssertEqual(result.phase, "action")
        XCTAssertEqual(result.failureCode, "ax_press_failed")
        XCTAssertFalse(result.pressCommitted)
        XCTAssertFalse(result.retrySafe)
    }

    func testProductionSemanticPressSourceHasNoSyntheticInputFallback() throws {
        let sourceURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Sources/SemanticPress.swift")
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        XCTAssertFalse(source.contains("InputDriver"))
        XCTAssertFalse(source.contains("mouse_event"))
        XCTAssertFalse(source.contains("movePointer"))
    }

    func testUnrelatedTargetAttributeChangeIsCompletedUnverified() throws {
        let changed = Self.signature(title: "Saved")
        let result = runSemanticPress(
            try makeRequest(),
            dependencies: Self.dependencies(observations: [.signature(changed)]))

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.phase, "post_observation")
        XCTAssertNil(result.postcondition)
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertTrue(result.pressCommitted)
        XCTAssertFalse(result.retrySafe)
    }

    func testTargetMissingWithoutDeclaredPredicateIsCompletedUnverified() throws {
        let result = runSemanticPress(
            try makeRequest(),
            dependencies: Self.dependencies(observations: [.missing]))

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.phase, "post_observation")
        XCTAssertEqual(result.failureCode, "postcondition_not_declared")
        XCTAssertNil(result.postcondition)
        XCTAssertTrue(result.pressCommitted)
        XCTAssertFalse(result.retrySafe)
    }

    func testUnchangedTargetTimesOutAsCompletedUnverifiedWithoutRetry() throws {
        var now = 0.0
        var pressCount = 0
        let unchanged = Self.signature()
        let deps = SemanticPressDependencies(
            isPIDLive: { _ in true },
            resolveWindow: { _, _ in .unique },
            resolveTarget: { _, _, _ in
                Self.target(performPress: {
                    pressCount += 1
                    return .success
                })
            },
            countFingerprint: { _, _, _ in 1 },
            observeTarget: { _, _, _ in .signature(unchanged) },
            now: { now },
            sleep: { now += $0 })

        let result = runSemanticPress(
            try makeRequest(),
            dependencies: deps,
            timeout: 0.5,
            pollInterval: 0.05)

        XCTAssertEqual(result.status, "completed_unverified")
        XCTAssertEqual(result.phase, "post_observation")
        XCTAssertEqual(result.failureCode, "postcondition_not_observed")
        XCTAssertTrue(result.pressCommitted)
        XCTAssertFalse(result.retrySafe)
        XCTAssertEqual(pressCount, 1)
        XCTAssertLessThanOrEqual(now, 0.5)
    }

    private func makeRequest(fallbackPolicy: String = "none") throws -> SemanticPressRequest {
        try SemanticPressRequest(
            pid: 42,
            windowID: 7001,
            path: "window[0]/AXButton[0]",
            expectedRole: "AXButton",
            expectedFingerprint: "axf_target",
            fallbackPolicy: fallbackPolicy)
    }

    private static func target(
        role: String = "AXButton",
        fingerprint: String = "axf_target",
        enabled: Bool? = true,
        actions: [String] = ["AXPress"],
        performPress: @escaping () -> AXError = { .success }
    ) -> SemanticPressTarget {
        SemanticPressTarget(
            role: role,
            fingerprint: fingerprint,
            enabled: enabled,
            actions: actions,
            signature: signature(),
            performPress: performPress)
    }

    private static func signature(title: String = "Save") -> SemanticPressTargetSignature {
        SemanticPressTargetSignature(
            fingerprint: "axf_target",
            title: title,
            description: nil,
            value: nil,
            valueRedacted: false,
            enabled: true,
            focused: false,
            selected: false,
            actions: ["AXPress"])
    }

    private static func dependencies(
        pidLive: Bool = true,
        window: SemanticPressWindowResolution = .unique,
        target: SemanticPressTarget? = target(),
        fingerprintCount: Int = 1,
        performPress: (() -> AXError)? = nil,
        observations: [SemanticPressTargetObservation] = [.signature(signature(title: "Saved"))]
    ) -> SemanticPressDependencies {
        var currentTime = 0.0
        var remaining = observations
        var resolvedTarget = target
        if target != nil {
            resolvedTarget = SemanticPressTests.target(
                role: target!.role,
                fingerprint: target!.fingerprint,
                enabled: target!.enabled,
                actions: target!.actions,
                performPress: performPress ?? target!.performPress)
        }
        return SemanticPressDependencies(
            isPIDLive: { _ in pidLive },
            resolveWindow: { _, _ in window },
            resolveTarget: { _, _, _ in resolvedTarget },
            countFingerprint: { _, _, _ in fingerprintCount },
            observeTarget: { _, _, _ in
                remaining.isEmpty ? .signature(signature()) : remaining.removeFirst()
            },
            now: { currentTime },
            sleep: { currentTime += $0 })
    }
}
