import Foundation
import XCTest
@testable import ax_server

final class StrictModifierSequenceTests: XCTestCase {
    func testModifiersWrapActionAndReleaseInReverseOrder() {
        var events: [String] = []
        let result = runStrictModifierSequenceV1(
            modifiers: ["command", "shift"],
            prepare: { modifier in
                StrictPreparedModifierV1(
                    press: { events.append("down:\(modifier)"); return true },
                    release: { events.append("up:\(modifier)"); return true })
            },
            isCancelled: { false },
            action: { events.append("action"); return 42 })

        XCTAssertEqual(result, .completed(42))
        XCTAssertEqual(
            events, ["down:command", "down:shift", "action", "up:shift", "up:command"])
    }

    func testCancellationAndPressFailureReleaseEveryHeldModifier() {
        var events: [String] = []
        var cancellationChecks = 0
        let cancelled = runStrictModifierSequenceV1(
            modifiers: ["command", "shift"],
            prepare: { modifier in
                StrictPreparedModifierV1(
                    press: { events.append("down:\(modifier)"); return true },
                    release: { events.append("up:\(modifier)"); return true })
            },
            isCancelled: {
                cancellationChecks += 1
                return cancellationChecks == 2
            },
            action: { events.append("action"); return true })
        XCTAssertEqual(cancelled, .cancelled(cleanupComplete: true))
        XCTAssertEqual(events, ["down:command", "up:command"])

        events.removeAll()
        let failed = runStrictModifierSequenceV1(
            modifiers: ["command", "shift"],
            prepare: { modifier in
                StrictPreparedModifierV1(
                    press: {
                        events.append("down:\(modifier)")
                        return modifier != "shift"
                    },
                    release: { events.append("up:\(modifier)"); return true })
            },
            isCancelled: { false },
            action: { events.append("action"); return true })
        XCTAssertEqual(failed, .pressFailed(cleanupComplete: true))
        XCTAssertEqual(events, ["down:command", "down:shift", "up:command"])
    }

    func testReleaseFailureIsNeverReportedAsCompleted() {
        let result = runStrictModifierSequenceV1(
            modifiers: ["command"],
            prepare: { _ in
                StrictPreparedModifierV1(press: { true }, release: { false })
            },
            isCancelled: { false },
            action: { "committed" })
        XCTAssertEqual(result, .releaseFailed("committed"))
    }

    func testOfficialKeypressPostsEveryOrdinaryKeyBetweenModifierLease() {
        var events: [String] = []
        let prepared = makeTargetBoundPreparedKeySequenceV1(
            keys: ["p", "a", "down"],
            modifiers: ["command", "shift"],
            prepareModifier: { modifier in
                StrictPreparedModifierV1(
                    press: { events.append("down:\(modifier)"); return true },
                    release: { events.append("up:\(modifier)"); return true })
            },
            prepareKey: { key, modifiers in
                XCTAssertEqual(modifiers, ["command", "shift"])
                return TargetBoundPreparedInput {
                    events.append("key:\(key)")
                    return true
                }
            },
            isCancelled: { false })

        XCTAssertEqual(prepared?.post(), .completed(keyPairsCommitted: 3))
        XCTAssertEqual(events, [
            "down:command", "down:shift",
            "key:p", "key:a", "key:down",
            "up:shift", "up:command",
        ])
    }
}
