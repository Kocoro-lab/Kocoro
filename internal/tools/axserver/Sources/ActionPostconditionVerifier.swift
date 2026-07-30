import ApplicationServices
import Foundation

/// The helper's action verifier is intentionally a narrow, bounded ladder:
///
/// 1. read only action-specific AX state on exact already-bound app/target refs;
/// 2. retry that same narrow state briefly for asynchronous AX/Electron settling;
/// 3. return inconclusive so the caller can use an explicitly declared,
///    bounded visual predicate if it has one, or report completed_unverified.
///
/// This layer never walks a full AX tree and never captures a full window.
struct TargetedAXPostconditionBudgetV1: Equatable {
    let maxDuration: TimeInterval
    let maxAttempts: Int
    let retryInterval: TimeInterval
    let maxSynchronousCallDuration: TimeInterval

    init(
        maxDuration: TimeInterval,
        maxAttempts: Int,
        retryInterval: TimeInterval,
        maxSynchronousCallDuration: TimeInterval = 0.1
    ) {
        self.maxDuration = maxDuration
        self.maxAttempts = maxAttempts
        self.retryInterval = retryInterval
        self.maxSynchronousCallDuration = maxSynchronousCallDuration
    }
}

let targetedAXPostconditionBudgetV1 = TargetedAXPostconditionBudgetV1(
    maxDuration: 0.35,
    maxAttempts: 8,
    retryInterval: 0.04,
    maxSynchronousCallDuration: 0.1)

private let targetedAXMinimumUsefulCallDurationV1: TimeInterval = 0.001

enum TargetedAXMessagingTimeoutOutcomeV1<Value> {
    case completed(Value)
    case unavailable
}

/// Applies a timeout to one exact AX element reference for one synchronous
/// operation, then restores that reference to the current process-global
/// default. Apple documents that an element timeout does not propagate even to
/// other AX references that compare equal, so callers must scope every exact
/// application or target reference that performs messaging.
func withTargetedAXMessagingTimeoutV1<Element, Value>(
    element: Element,
    timeout: TimeInterval,
    setTimeout: (Element, Float) -> AXError,
    operation: () -> Value
) -> TargetedAXMessagingTimeoutOutcomeV1<Value> {
    guard timeout > 0, timeout.isFinite else { return .unavailable }
    let boundedTimeout = Float(timeout)
    guard boundedTimeout > 0, boundedTimeout.isFinite,
          setTimeout(element, boundedTimeout) == .success else {
        return .unavailable
    }
    let value = operation()
    // Zero on a non-system-wide element restores the current global default;
    // it does not set a zero-second timeout.
    guard setTimeout(element, 0) == .success else { return .unavailable }
    return .completed(value)
}

func withTargetedAXMessagingTimeoutV1<Value>(
    element: AXUIElement,
    timeout: TimeInterval,
    operation: () -> Value
) -> TargetedAXMessagingTimeoutOutcomeV1<Value> {
    withTargetedAXMessagingTimeoutV1(
        element: element,
        timeout: timeout,
        setTimeout: AXUIElementSetMessagingTimeout,
        operation: operation)
}

enum TargetedAXPostconditionSampleV1<Observation> {
    case matched(Observation)
    case retryable(failureCode: String, observation: Observation?)
    case terminal(failureCode: String, observation: Observation?)
}

enum TargetedAXPostconditionOutcomeV1<Observation>: CustomStringConvertible {
    case verified(Observation, attempts: Int)
    case inconclusive(failureCode: String, observation: Observation?, attempts: Int)

    var description: String {
        switch self {
        case let .verified(_, attempts):
            return "verified(attempts: \(attempts))"
        case let .inconclusive(code, _, attempts):
            return "inconclusive(\(code), attempts: \(attempts))"
        }
    }
}

func runTargetedAXPostconditionVerificationV1<Observation>(
    budget: TargetedAXPostconditionBudgetV1 = targetedAXPostconditionBudgetV1,
    now: () -> Date,
    sleep: (TimeInterval) -> Void,
    observeWithTimeout: (TimeInterval) -> TargetedAXPostconditionSampleV1<Observation>
) -> TargetedAXPostconditionOutcomeV1<Observation> {
    precondition(budget.maxDuration >= 0)
    precondition(budget.maxAttempts > 0)
    precondition(budget.retryInterval >= 0)
    precondition(budget.maxSynchronousCallDuration > 0)

    let startedAt = now()
    var attempts = 0
    var lastFailureCode = "postcondition_not_observed"
    var lastObservation: Observation?

    while attempts < budget.maxAttempts {
        let elapsedBeforeObservation = max(0, now().timeIntervalSince(startedAt))
        let remainingBeforeObservation = budget.maxDuration - elapsedBeforeObservation
        // Do not schedule an effectively zero AX timeout due to floating-point
        // residue at the deadline. One millisecond is already below the useful
        // resolution for cross-process AX messaging.
        guard remainingBeforeObservation >= targetedAXMinimumUsefulCallDurationV1 else { break }
        attempts += 1
        let callTimeout = min(
            budget.maxSynchronousCallDuration, remainingBeforeObservation)
        switch observeWithTimeout(callTimeout) {
        case let .matched(observation):
            return .verified(observation, attempts: attempts)
        case let .terminal(failureCode, observation):
            return .inconclusive(
                failureCode: failureCode, observation: observation, attempts: attempts)
        case let .retryable(failureCode, observation):
            lastFailureCode = failureCode
            lastObservation = observation
        }

        guard attempts < budget.maxAttempts else { break }
        let elapsed = max(0, now().timeIntervalSince(startedAt))
        let remaining = budget.maxDuration - elapsed
        guard remaining > 0 else { break }
        sleep(min(budget.retryInterval, remaining))
    }

    return .inconclusive(
        failureCode: lastFailureCode, observation: lastObservation, attempts: attempts)
}

/// Compatibility overload for deterministic unit harnesses and non-AX
/// observations. Production AX postconditions use `observeWithTimeout` above.
func runTargetedAXPostconditionVerificationV1<Observation>(
    budget: TargetedAXPostconditionBudgetV1 = targetedAXPostconditionBudgetV1,
    now: () -> Date,
    sleep: (TimeInterval) -> Void,
    observe: () -> TargetedAXPostconditionSampleV1<Observation>
) -> TargetedAXPostconditionOutcomeV1<Observation> {
    runTargetedAXPostconditionVerificationV1(
        budget: budget,
        now: now,
        sleep: sleep,
        observeWithTimeout: { _ in observe() })
}
