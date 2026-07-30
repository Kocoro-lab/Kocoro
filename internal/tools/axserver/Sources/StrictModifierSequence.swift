import CoreGraphics
import Foundation

struct StrictPreparedModifierV1 {
    let press: () -> Bool
    let release: () -> Bool
}

enum StrictModifierSequenceResultV1<Value: Equatable>: Equatable {
    case completed(Value)
    case preparationFailed(cleanupComplete: Bool)
    case pressFailed(cleanupComplete: Bool)
    case cancelled(cleanupComplete: Bool)
    case releaseFailed(Value)
}

func runStrictModifierSequenceV1<Value: Equatable>(
    modifiers: [String],
    prepare: (String) -> StrictPreparedModifierV1?,
    isCancelled: () -> Bool,
    action: () -> Value
) -> StrictModifierSequenceResultV1<Value> {
    var held: [StrictPreparedModifierV1] = []
    let releaseHeld: () -> Bool = {
        var complete = true
        for modifier in held.reversed() where !modifier.release() {
            complete = false
        }
        held.removeAll()
        return complete
    }

    for modifier in modifiers {
        if isCancelled() {
            return .cancelled(cleanupComplete: releaseHeld())
        }
        guard let prepared = prepare(modifier) else {
            return .preparationFailed(cleanupComplete: releaseHeld())
        }
        guard prepared.press() else {
            return .pressFailed(cleanupComplete: releaseHeld())
        }
        held.append(prepared)
    }
    if isCancelled() {
        return .cancelled(cleanupComplete: releaseHeld())
    }

    let value = action()
    return releaseHeld() ? .completed(value) : .releaseFailed(value)
}

private let strictModifierVirtualKeysV1: [String: CGKeyCode] = [
    "command": 0x37,
    "shift": 0x38,
    "option": 0x3a,
    "control": 0x3b,
]

private let strictModifierFlagsV1: [String: CGEventFlags] = [
    "command": .maskCommand,
    "shift": .maskShift,
    "option": .maskAlternate,
    "control": .maskControl,
]

func strictInputModifierFlagsV1(_ modifiers: [String]) -> CGEventFlags? {
    guard strictInputModifiersV1(modifiers) else { return nil }
    return modifiers.reduce(into: CGEventFlags()) { flags, modifier in
        flags.formUnion(strictModifierFlagsV1[modifier]!)
    }
}

func productionStrictModifierPrepareV1() -> (String) -> StrictPreparedModifierV1? {
    var heldFlags = CGEventFlags()
    return { modifier in
        guard let keyCode = strictModifierVirtualKeysV1[modifier],
              let modifierFlag = strictModifierFlagsV1[modifier],
              let syntheticSource = physicalInputSyntheticEventSourceV1,
              let down = CGEvent(
                keyboardEventSource: syntheticSource,
                virtualKey: keyCode, keyDown: true) else { return nil }
        let flagsBeforePress = heldFlags
        heldFlags.formUnion(modifierFlag)
        down.flags = heldFlags

        var token: UUID?
        let releaseMetadata = InputReleaseMetadataV1.key(
            virtualKey: UInt16(keyCode), eventFlags: flagsBeforePress.rawValue)
        guard let release = productionInputRelease(
            metadata: releaseMetadata, eventSource: syntheticSource) else { return nil }
        return StrictPreparedModifierV1(
            press: {
                guard token == nil else { return false }
                token = processInputCommitGateV1.registerPress(
                    release: release,
                    commitDown: {
                        down.post(tap: .cghidEventTap)
                        return true
                    })
                return token != nil
            },
            release: {
                guard let activeToken = token else { return false }
                token = nil
                return processInputCommitGateV1.confirmRelease(token: activeToken)
            })
    }
}

private func strictModifierFailureCodeV1(
    cleanupComplete: Bool, cancelled: Bool = false
) -> String {
    if !cleanupComplete { return "modifier_release_unconfirmed" }
    return cancelled ? "cancelled_before_input" : "modifier_press_failed"
}

func runCoordinateMouseEventWithModifiersV1(
    request: CoordinateMouseEventRequestV1,
    dependencies: CoordinateMouseEventDependencies
) -> CoordinateMouseEventResultV1 {
    let result = runStrictModifierSequenceV1(
        modifiers: request.modifiers,
        prepare: productionStrictModifierPrepareV1(),
        isCancelled: { false },
        action: {
            runCoordinateMouseEvent(request: request, dependencies: dependencies)
        })
    switch result {
    case let .completed(value):
        return value
    case let .releaseFailed(value):
        guard value.primaryActionCommitted else { return value }
        return .init(
            status: "completed_unverified", action: value.action,
            primaryActionCommitted: true,
            pointerMotionCommitted: value.pointerMotionCommitted,
            phase: "post_verification",
            failureCode: "modifier_release_unconfirmed",
            pointerEndpoint: value.pointerEndpoint)
    case let .cancelled(cleanupComplete):
        return coordinateMouseModifierFailureV1(
            request, code: strictModifierFailureCodeV1(
                cleanupComplete: cleanupComplete, cancelled: true))
    case let .preparationFailed(cleanupComplete),
         let .pressFailed(cleanupComplete):
        return coordinateMouseModifierFailureV1(
            request, code: strictModifierFailureCodeV1(
                cleanupComplete: cleanupComplete))
    }
}

private func coordinateMouseModifierFailureV1(
    _ request: CoordinateMouseEventRequestV1, code: String
) -> CoordinateMouseEventResultV1 {
    .init(
        status: "failed", action: request.action,
        primaryActionCommitted: false, pointerMotionCommitted: false,
        phase: "preflight", failureCode: code, pointerEndpoint: nil)
}

func runCoordinateDragWithModifiersV1(
    request: CoordinateDragRequestV1,
    dependencies: CoordinateDragDependencies
) -> CoordinateDragResultV1 {
    let result = runStrictModifierSequenceV1(
        modifiers: request.modifiers,
        prepare: productionStrictModifierPrepareV1(),
        isCancelled: dependencies.isCancelled,
        action: {
            runCoordinateDrag(request: request, dependencies: dependencies)
        })
    switch result {
    case let .completed(value):
        return value
    case let .releaseFailed(value):
        guard value.mouseDownCommitted else { return value }
        return .init(
            status: "completed_unverified", dragCommitted: true,
            mouseDownCommitted: true,
            pointerMotionCommitted: value.pointerMotionCommitted,
            mouseUpCommitted: value.mouseUpCommitted,
            possibleDropSideEffect: true,
            phase: "post_verification",
            failureCode: "modifier_release_unconfirmed",
            postcondition: nil, pointerEndpoint: value.pointerEndpoint)
    case let .cancelled(cleanupComplete):
        return coordinateDragModifierFailureV1(code: strictModifierFailureCodeV1(
            cleanupComplete: cleanupComplete, cancelled: true))
    case let .preparationFailed(cleanupComplete),
         let .pressFailed(cleanupComplete):
        return coordinateDragModifierFailureV1(code: strictModifierFailureCodeV1(
            cleanupComplete: cleanupComplete))
    }
}

private func coordinateDragModifierFailureV1(code: String) -> CoordinateDragResultV1 {
    .init(
        status: "failed", dragCommitted: false,
        mouseDownCommitted: false, pointerMotionCommitted: false,
        mouseUpCommitted: false, possibleDropSideEffect: false,
        phase: "preflight", failureCode: code,
        postcondition: nil, pointerEndpoint: nil)
}

func runCoordinatePixelScrollWithModifiersV1(
    request: CoordinatePixelScrollRequestV1,
    dependencies: CoordinatePixelScrollDependenciesV1
) -> CoordinatePixelScrollResultV1 {
    let result = runStrictModifierSequenceV1(
        modifiers: request.modifiers,
        prepare: productionStrictModifierPrepareV1(),
        isCancelled: dependencies.isCancelled,
        action: {
            runCoordinatePixelScrollV1(request: request, dependencies: dependencies)
        })
    switch result {
    case let .completed(value):
        return value
    case let .releaseFailed(value):
        guard value.pointerMoveCommitState == "committed" else { return value }
        let scrollState: CoordinatePixelScrollCommitStateV1 =
            value.scrollCommitState == "committed" ? .committed : .notCommitted
        return .init(
            status: "committed_unverified",
            pointerMoveCommitState: .committed,
            scrollCommitState: scrollState,
            phase: value.scrollCommitState == "committed"
                ? "post_verification" : "between_commits",
            failureCode: "modifier_release_unconfirmed",
            requested: value.requested,
            pointerEndpoint: value.pointerEndpoint)
    case let .cancelled(cleanupComplete):
        return coordinatePixelScrollModifierFailureV1(
            request, code: strictModifierFailureCodeV1(
                cleanupComplete: cleanupComplete, cancelled: true))
    case let .preparationFailed(cleanupComplete),
         let .pressFailed(cleanupComplete):
        return coordinatePixelScrollModifierFailureV1(
            request, code: strictModifierFailureCodeV1(
                cleanupComplete: cleanupComplete))
    }
}

private func coordinatePixelScrollModifierFailureV1(
    _ request: CoordinatePixelScrollRequestV1, code: String
) -> CoordinatePixelScrollResultV1 {
    .init(
        status: "failed",
        pointerMoveCommitState: .notCommitted,
        scrollCommitState: .notCommitted,
        phase: "preflight", failureCode: code,
        requested: .init(request: request), pointerEndpoint: nil)
}
