import Foundation
import XCTest
@testable import ax_server

final class ActionPostconditionVerifierTests: XCTestCase {
    func testTimedObservationReceivesSingleCallLimitCappedByRemainingBudget() {
        var currentTime = 100.0
        var observationDurations = [0.08, 0.16, 0.10, 0.01]
        var timeouts: [TimeInterval] = []
        let budget = TargetedAXPostconditionBudgetV1(
            maxDuration: 0.35, maxAttempts: 8, retryInterval: 0,
            maxSynchronousCallDuration: 0.1)

        let outcome: TargetedAXPostconditionOutcomeV1<String> =
            runTargetedAXPostconditionVerificationV1(
                budget: budget,
                now: { Date(timeIntervalSince1970: currentTime) },
                sleep: { _ in },
                observeWithTimeout: { timeout in
                    timeouts.append(timeout)
                    currentTime += observationDurations.removeFirst()
                    return .retryable(failureCode: "not_settled", observation: nil)
                })

        guard case let .inconclusive(code, _, attempts) = outcome else {
            return XCTFail("expected inconclusive, got \(outcome)")
        }
        XCTAssertEqual(code, "not_settled")
        XCTAssertEqual(attempts, 4)
        XCTAssertEqual(timeouts.count, 4)
        XCTAssertEqual(timeouts[0], 0.1, accuracy: 0.000_001)
        XCTAssertEqual(timeouts[1], 0.1, accuracy: 0.000_001)
        XCTAssertEqual(timeouts[2], 0.1, accuracy: 0.000_001)
        XCTAssertEqual(timeouts[3], 0.01, accuracy: 0.000_001)
    }

    func testElementMessagingTimeoutIsScopedAndRestoredToGlobalDefault() {
        var configured: [(String, Float)] = []
        var operationCalls = 0

        let outcome = withTargetedAXMessagingTimeoutV1(
            element: "exact-target-ref",
            timeout: 0.08,
            setTimeout: { element, timeout in
                configured.append((element, timeout))
                return .success
            },
            operation: {
                operationCalls += 1
                return "observed"
            })

        guard case let .completed(value) = outcome else {
            return XCTFail("expected completed, got \(outcome)")
        }
        XCTAssertEqual(value, "observed")
        XCTAssertEqual(operationCalls, 1)
        XCTAssertEqual(configured.map(\.0), ["exact-target-ref", "exact-target-ref"])
        XCTAssertEqual(configured[0].1, 0.08, accuracy: 0.000_001)
        XCTAssertEqual(configured[1].1, 0, accuracy: 0.000_001)
    }

    func testElementMessagingTimeoutFailsClosedBeforeAXCallWhenConfigurationFails() {
        var operationCalls = 0

        let outcome: TargetedAXMessagingTimeoutOutcomeV1<String> =
            withTargetedAXMessagingTimeoutV1(
                element: "exact-app-ref",
                timeout: 0.08,
                setTimeout: { _, _ in .invalidUIElement },
                operation: {
                    operationCalls += 1
                    return "must-not-run"
                })

        guard case .unavailable = outcome else {
            return XCTFail("expected unavailable, got \(outcome)")
        }
        XCTAssertEqual(operationCalls, 0)
    }

    func testElementMessagingTimeoutReportsUnavailableWhenDefaultRestoreFails() {
        var calls = 0
        var operationCalls = 0

        let outcome: TargetedAXMessagingTimeoutOutcomeV1<String> =
            withTargetedAXMessagingTimeoutV1(
                element: "exact-target-ref",
                timeout: 0.08,
                setTimeout: { _, _ in
                    defer { calls += 1 }
                    return calls == 0 ? .success : .invalidUIElement
                },
                operation: {
                    operationCalls += 1
                    return "observed"
                })

        guard case .unavailable = outcome else {
            return XCTFail("expected unavailable, got \(outcome)")
        }
        XCTAssertEqual(operationCalls, 1)
        XCTAssertEqual(calls, 2)
    }

    func testImmediateTargetedAXMatchDoesNotSleep() {
        var observations = 0
        var sleeps: [TimeInterval] = []

        let outcome = runTargetedAXPostconditionVerificationV1(
            now: { Date(timeIntervalSince1970: 100) },
            sleep: { sleeps.append($0) },
            observe: {
                observations += 1
                return .matched("exact")
            })

        guard case let .verified(value, attempts) = outcome else {
            return XCTFail("expected verified, got \(outcome)")
        }
        XCTAssertEqual(value, "exact")
        XCTAssertEqual(attempts, 1)
        XCTAssertEqual(observations, 1)
        XCTAssertEqual(sleeps, [])
    }

    func testTargetedAXSettlePollingIsBoundedByAttemptBudget() {
        var observations = 0
        var sleeps: [TimeInterval] = []

        let outcome: TargetedAXPostconditionOutcomeV1<String> =
            runTargetedAXPostconditionVerificationV1(
                now: { Date(timeIntervalSince1970: 100) },
                sleep: { sleeps.append($0) },
                observe: {
                    observations += 1
                    return .retryable(
                        failureCode: "target_value_mismatch", observation: "redacted")
                })

        guard case let .inconclusive(code, observation, attempts) = outcome else {
            return XCTFail("expected inconclusive, got \(outcome)")
        }
        XCTAssertEqual(code, "target_value_mismatch")
        XCTAssertEqual(observation, "redacted")
        XCTAssertEqual(attempts, targetedAXPostconditionBudgetV1.maxAttempts)
        XCTAssertEqual(observations, targetedAXPostconditionBudgetV1.maxAttempts)
        XCTAssertEqual(sleeps.count, targetedAXPostconditionBudgetV1.maxAttempts - 1)
        XCTAssertLessThanOrEqual(
            sleeps.reduce(0, +), targetedAXPostconditionBudgetV1.maxDuration)
    }

    func testTerminalTargetedAXObservationDoesNotEscalateOrPoll() {
        var observations = 0
        var sleeps = 0

        let outcome: TargetedAXPostconditionOutcomeV1<String> =
            runTargetedAXPostconditionVerificationV1(
                now: { Date(timeIntervalSince1970: 100) },
                sleep: { _ in sleeps += 1 },
                observe: {
                    observations += 1
                    return .terminal(
                        failureCode: "verification_redacted_sensitive_target",
                        observation: nil)
                })

        guard case let .inconclusive(code, observation, attempts) = outcome else {
            return XCTFail("expected inconclusive, got \(outcome)")
        }
        XCTAssertEqual(code, "verification_redacted_sensitive_target")
        XCTAssertNil(observation)
        XCTAssertEqual(attempts, 1)
        XCTAssertEqual(observations, 1)
        XCTAssertEqual(sleeps, 0)
    }

    func testTargetedAXPollingStopsAtDurationBudgetBeforeAttemptCap() {
        var currentTime = 100.0
        var observations = 0
        var totalSleep: TimeInterval = 0
        let budget = TargetedAXPostconditionBudgetV1(
            maxDuration: 0.09, maxAttempts: 20, retryInterval: 0.04)

        let outcome: TargetedAXPostconditionOutcomeV1<String> =
            runTargetedAXPostconditionVerificationV1(
                budget: budget,
                now: { Date(timeIntervalSince1970: currentTime) },
                sleep: { duration in
                    totalSleep += duration
                    currentTime += duration
                },
                observe: {
                    observations += 1
                    return .retryable(failureCode: "not_settled", observation: nil)
                })

        guard case let .inconclusive(code, _, attempts) = outcome else {
            return XCTFail("expected inconclusive, got \(outcome)")
        }
        XCTAssertEqual(code, "not_settled")
        XCTAssertEqual(attempts, 3)
        XCTAssertEqual(observations, 3)
        XCTAssertEqual(totalSleep, 0.09, accuracy: 0.000_001)
    }
}
