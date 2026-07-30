import Foundation
import XCTest
@testable import ax_server

final class InputCrashRecoveryTests: XCTestCase {
    func testReleaseConfirmationWaitsForAsynchronousCombinedState() {
        var posts = 0
        var settles = 0
        var observations = [false, false, true]

        XCTAssertTrue(postAndConfirmInputReleaseV1(
            post: { posts += 1 },
            isReleased: { observations.removeFirst() },
            settle: { settles += 1 },
            maximumAttempts: 3
        ))
        XCTAssertEqual(posts, 1)
        XCTAssertEqual(settles, 3)
        XCTAssertTrue(observations.isEmpty)
    }

    func testBlockedGateSelfRecoversWhenCurrentProcessReleaseBecomesConfirmable() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        var releaseAttempts = 0
        let token = try XCTUnwrap(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                releaseAttempts += 1
                harness.posts.append("up-\(releaseAttempts)")
                return releaseAttempts >= 2
            },
            commitDown: {
                harness.posts.append("down")
                return true
            }))

        XCTAssertFalse(gate.confirmRelease(token: token))
        XCTAssertTrue(FileManager.default.fileExists(atPath: harness.journal.path))

        harness.now = harness.now.addingTimeInterval(1)
        XCTAssertTrue(gate.canAdmitInput())
        XCTAssertEqual(harness.posts, ["down", "up-1", "up-2"])
        XCTAssertFalse(FileManager.default.fileExists(atPath: harness.journal.path))
        XCTAssertTrue(gate.commitSample {
            harness.posts.append("next-input")
            return true
        })
    }

    func testEveryOfficialOpenAIClickButtonHasDurableReleaseMetadata() throws {
        for button in ["left", "right", "wheel", "back", "forward"] {
            let metadata = InputReleaseMetadataV1.mouse(button: button)
            XCTAssertTrue(metadata.isValid, button)
            let encoded = try JSONEncoder().encode(metadata)
            XCTAssertEqual(
                try JSONDecoder().decode(InputReleaseMetadataV1.self, from: encoded),
                metadata)
        }
    }

    func testTargetedKeyReleaseMetadataIsStrictAndContentFree() throws {
        let metadata = InputReleaseMetadataV1.targetedKey(
            virtualKey: 0x24,
            eventFlags: 0,
            pid: 42,
            bundleID: "com.apple.Notes",
            launchDate: "2026-07-28T06:00:00Z")
        XCTAssertTrue(metadata.isValid)
        let encoded = try JSONEncoder().encode(metadata)
        XCTAssertEqual(
            try JSONDecoder().decode(InputReleaseMetadataV1.self, from: encoded),
            metadata)
        let wire = String(decoding: encoded, as: UTF8.self)
        XCTAssertTrue(wire.contains("\"kind\":\"targeted_key\""))
        XCTAssertFalse(wire.contains("text"))
        XCTAssertFalse(wire.contains("clipboard"))
        XCTAssertFalse(wire.contains("AXValue"))

        var malformed = try XCTUnwrap(
            JSONSerialization.jsonObject(with: encoded) as? [String: Any])
        malformed.removeValue(forKey: "launch_date")
        XCTAssertThrowsError(try JSONDecoder().decode(
            InputReleaseMetadataV1.self,
            from: JSONSerialization.data(withJSONObject: malformed)))
    }

    func testTargetedKeyJournalRecoversExactlyOnceWithExactProcessIdentity() throws {
        let harness = try RecoveryHarness()
        let metadata = InputReleaseMetadataV1.targetedKey(
            virtualKey: 0x7b,
            eventFlags: 0,
            pid: 42,
            bundleID: "com.apple.Notes",
            launchDate: "2026-07-28T06:00:00Z")
        try harness.writeJournal(createdAt: harness.now, releases: [metadata])

        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .recovered(releaseCount: 1))
        XCTAssertEqual(harness.recovered, [metadata])
        XCTAssertFalse(FileManager.default.fileExists(atPath: harness.journal.path))
        XCTAssertTrue(gate.canAdmitInput())
        XCTAssertEqual(harness.recovered, [metadata])
    }

    func testTargetedKeyRecoveryForGoneProcessNeverPostsGlobally() throws {
        let metadata = InputReleaseMetadataV1.targetedKey(
            virtualKey: 0x24,
            eventFlags: 0,
            pid: Int(Int32.max),
            bundleID: "invalid.example.gone",
            launchDate: "2026-07-28T06:00:00Z")
        let release = try XCTUnwrap(productionInputRelease(metadata: metadata))

        XCTAssertTrue(
            release.postAndConfirm(),
            "a gone exact process instance should clear recovery without a post")
    }

    func testJournalIsDurableBeforeDownAndClearedOnlyAfterConfirmedRelease() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)

        let token = gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                harness.posts.append("up")
                return true
            },
            commitDown: {
                XCTAssertTrue(FileManager.default.fileExists(atPath: harness.journal.path))
                harness.posts.append("down")
                return true
            })

        XCTAssertNotNil(token)
        XCTAssertEqual(harness.posts, ["down"])
        XCTAssertTrue(FileManager.default.fileExists(atPath: harness.journal.path))
        XCTAssertTrue(gate.confirmRelease(token: try XCTUnwrap(token)))
        XCTAssertEqual(harness.posts, ["down", "up"])
        XCTAssertFalse(FileManager.default.fileExists(atPath: harness.journal.path))
    }

    func testSIGTERMDuringDragReleasesAndPreventsEveryLaterCommitOrSample() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        let token = try XCTUnwrap(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                harness.posts.append("up")
                return true
            },
            commitDown: { harness.posts.append("down"); return true }))
        XCTAssertTrue(gate.commitSample { harness.posts.append("drag"); return true })

        let shutdown = gate.shutdownForSignal(SIGTERM)

        XCTAssertEqual(shutdown.signal, SIGTERM)
        XCTAssertEqual(shutdown.confirmedReleaseCount, 1)
        XCTAssertEqual(harness.posts, ["down", "drag", "up"])
        XCTAssertFalse(gate.commitSample { harness.posts.append("late-drag"); return true })
        XCTAssertFalse(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) { true },
            commitDown: { harness.posts.append("late-down"); return true }) != nil)
        XCTAssertTrue(gate.confirmRelease(token: token), "shutdown already confirmed this release")
        XCTAssertEqual(harness.posts, ["down", "drag", "up"])
        XCTAssertFalse(FileManager.default.fileExists(atPath: harness.journal.path))
    }

    func testSocketSignalControllerRunsTERMReleaseOnNormalDispatchContext() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        XCTAssertNotNil(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                harness.posts.append("up")
                return true
            },
            commitDown: { harness.posts.append("down"); return true }))
        let controller = SocketServerShutdownController(
            socketPath: nil, inputGate: gate, terminate: { _ in XCTFail("unexpected exit") })

        controller.stop(signal: SIGTERM, terminate: false)

        XCTAssertEqual(harness.posts, ["down", "up"])
        XCTAssertFalse(gate.commitSample { harness.posts.append("late"); return true })
        XCTAssertEqual(harness.posts, ["down", "up"])
    }

    func testValidJournalIsRecoveredExactlyOnceBeforeNewMutation() throws {
        let harness = try RecoveryHarness()
        let first = harness.makeGate()
        XCTAssertEqual(first.recoverAtStartup(), .clean)
        XCTAssertNotNil(first.registerPress(
            release: .init(metadata: .key(virtualKey: 9, eventFlags: 1 << 20)) { true },
            commitDown: { true }))

        let restarted = harness.makeGate()
        XCTAssertEqual(restarted.recoverAtStartup(), .recovered(releaseCount: 1))
        XCTAssertEqual(harness.recovered, [.key(virtualKey: 9, eventFlags: 1 << 20)])
        XCTAssertFalse(FileManager.default.fileExists(atPath: harness.journal.path))
        XCTAssertEqual(restarted.recoverAtStartup(), .clean)
        XCTAssertEqual(harness.recovered.count, 1)
        XCTAssertNotNil(restarted.registerPress(
            release: .init(metadata: .mouse(button: "left")) { true },
            commitDown: { true }))
    }

    func testRedundantValidReleaseFromOldJournalIsHarmlessAndOneShot() throws {
        let harness = try RecoveryHarness()
        try harness.writeJournal(
            createdAt: harness.now,
            releases: [.mouse(button: "left")])
        let gate = harness.makeGate()

        XCTAssertEqual(gate.recoverAtStartup(), .recovered(releaseCount: 1))
        XCTAssertEqual(harness.recovered, [.mouse(button: "left")])
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        XCTAssertEqual(harness.recovered, [.mouse(button: "left")])
    }

    func testJournalClearFailureAfterReleasePermanentlyFailsGateClosed() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate(removeJournalSucceeds: false)
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        let token = try XCTUnwrap(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                harness.posts.append("up")
                return true
            },
            commitDown: { harness.posts.append("down"); return true }))

        XCTAssertFalse(gate.confirmRelease(token: token))
        XCTAssertEqual(harness.posts, ["down", "up"])
        XCTAssertTrue(FileManager.default.fileExists(atPath: harness.journal.path))
        XCTAssertFalse(gate.commitSample { harness.posts.append("late"); return true })
        XCTAssertNil(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) { true },
            commitDown: { harness.posts.append("late-down"); return true }))
        XCTAssertEqual(harness.posts, ["down", "up"])
    }

    func testUnconfirmedReleaseQuarantinesGateAndRejectsEveryLaterInput() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        let token = try XCTUnwrap(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                harness.posts.append("unconfirmed-up")
                return false
            },
            commitDown: {
                harness.posts.append("down")
                return true
            }))

        XCTAssertFalse(gate.confirmRelease(token: token))
        XCTAssertEqual(harness.posts, ["down", "unconfirmed-up"])
        XCTAssertTrue(
            FileManager.default.fileExists(atPath: harness.journal.path),
            "unconfirmed held-state recovery journal must remain durable")
        XCTAssertFalse(gate.canAdmitInput())
        XCTAssertFalse(gate.commitSample {
            harness.posts.append("late-sample")
            return true
        })
        XCTAssertNil(gate.registerPress(
            release: .init(metadata: .key(virtualKey: 9, eventFlags: 0)) {
                harness.posts.append("late-up")
                return true
            },
            commitDown: {
                harness.posts.append("late-down")
                return true
            }))
        XCTAssertEqual(harness.posts, ["down", "unconfirmed-up"])

        let shutdown = gate.shutdownForSignal(SIGTERM)
        XCTAssertEqual(shutdown.unresolvedReleaseCount, 1)
        XCTAssertFalse(gate.commitSample {
            harness.posts.append("post-shutdown-sample")
            return true
        })
        XCTAssertEqual(harness.posts, ["down", "unconfirmed-up", "unconfirmed-up"])
    }

    /// A quarantined gate must still release every other held input. Refusing
    /// to post the remaining releases is what strands a physically held
    /// Command/Shift on the user's machine: releaseHeld() keeps iterating, but
    /// each later confirmRelease would return false without posting.
    func testQuarantinedGateStillReleasesEveryOtherHeldInput() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)

        let command = try XCTUnwrap(gate.registerPress(
            release: .init(metadata: .key(virtualKey: 0x37, eventFlags: 0)) {
                harness.posts.append("command-up")
                return true
            },
            commitDown: {
                harness.posts.append("command-down")
                return true
            }))
        let shift = try XCTUnwrap(gate.registerPress(
            release: .init(metadata: .key(virtualKey: 0x38, eventFlags: 0)) {
                harness.posts.append("shift-up")
                return false
            },
            commitDown: {
                harness.posts.append("shift-down")
                return true
            }))

        // Shift releases first (reverse press order) and cannot be confirmed —
        // e.g. the user is physically resting on Shift, so the combined-session
        // keyState check still reads it as down. This quarantines the gate.
        XCTAssertFalse(gate.confirmRelease(token: shift))
        XCTAssertFalse(gate.canAdmitInput())

        // Regression: Command must still be released despite the quarantine.
        XCTAssertTrue(
            gate.confirmRelease(token: command),
            "quarantine must not suppress a confirmable release")
        XCTAssertEqual(
            harness.posts,
            ["command-down", "shift-down", "shift-up", "command-up"])

        // The quarantine itself must survive: no new input may be admitted.
        XCTAssertFalse(gate.canAdmitInput())
        XCTAssertFalse(gate.commitSample {
            harness.posts.append("late-sample")
            return true
        })
        XCTAssertEqual(
            harness.posts,
            ["command-down", "shift-down", "shift-up", "command-up"])
        XCTAssertTrue(
            FileManager.default.fileExists(atPath: harness.journal.path),
            "the unconfirmed shift release must remain durable for recovery")
    }

    func testJournalPersistFailurePreventsDownAndPermanentlyFailsClosed() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        try FileManager.default.removeItem(at: harness.directory)
        try Data("not-a-directory".utf8).write(to: harness.directory)

        let token = gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) {
                harness.posts.append("up")
                return true
            },
            commitDown: { harness.posts.append("down"); return true })

        XCTAssertNil(token)
        XCTAssertEqual(harness.posts, [])
        XCTAssertFalse(gate.commitSample { harness.posts.append("late"); return true })
        XCTAssertEqual(harness.posts, [])
    }

    func testMalformedJournalIsNeverExecutedAndFailsClosedWithoutEchoingBytes() throws {
        let harness = try RecoveryHarness()
        let secret = "clipboard=private text; AXValue=secret"
        try Data("{\"schema_version\":1,\"secret\":\"\(secret)\"}".utf8)
            .write(to: harness.journal, options: .atomic)

        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .blockedMalformed)
        XCTAssertEqual(harness.recovered, [])
        XCTAssertFalse(gate.commitSample { true })
        XCTAssertNil(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) { true },
            commitDown: { true }))
        XCTAssertTrue(FileManager.default.fileExists(atPath: harness.journal.path))
    }

    func testStaleJournalIsDiscardedWithoutPostingAndDoesNotBlockMutation() throws {
        let harness = try RecoveryHarness()
        try harness.writeJournal(
            createdAt: harness.now.addingTimeInterval(-InputCommitGateV1.maximumJournalAge - 1),
            releases: [.mouse(button: "left")])

        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .discardedStale)
        XCTAssertEqual(harness.recovered, [])
        XCTAssertFalse(FileManager.default.fileExists(atPath: harness.journal.path))
        XCTAssertNotNil(gate.registerPress(
            release: .init(metadata: .mouse(button: "left")) { true },
            commitDown: { true }))
    }

    func testJournalSchemaContainsOnlyRedactedReleaseMetadata() throws {
        let harness = try RecoveryHarness()
        let gate = harness.makeGate()
        XCTAssertEqual(gate.recoverAtStartup(), .clean)
        let sensitive = "typed-text clipboard-bytes AXValue"
        XCTAssertNotNil(gate.registerPress(
            release: .init(metadata: .key(virtualKey: 9, eventFlags: 1 << 20)) {
                _ = sensitive
                return true
            },
            commitDown: { true }))

        let bytes = try Data(contentsOf: harness.journal)
        let wire = String(decoding: bytes, as: UTF8.self)
        XCTAssertFalse(wire.contains(sensitive))
        XCTAssertFalse(wire.localizedCaseInsensitiveContains("clipboard"))
        XCTAssertFalse(wire.contains("AXValue"))
        let root = try XCTUnwrap(JSONSerialization.jsonObject(with: bytes) as? [String: Any])
        XCTAssertEqual(Set(root.keys), ["schema_version", "created_at", "releases"])
        let releases = try XCTUnwrap(root["releases"] as? [[String: Any]])
        XCTAssertEqual(Set(try XCTUnwrap(releases.first).keys), ["kind", "virtual_key", "event_flags"])
    }

    func testProductionSyntheticInputEntrypointsAreAllGateIntegrated() throws {
        let sources = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Sources", isDirectory: true)
        let read: (String) throws -> String = { name in
            try String(contentsOf: sources.appendingPathComponent(name), encoding: .utf8)
        }
        let drag = try read("CoordinateDrag.swift")
        let coordinate = try read("CoordinateMouseEvent.swift")
        let target = try read("TargetBoundInput.swift")
        let legacy = try read("InputDriver.swift")
        let socket = try read("SocketServer.swift")

        XCTAssertTrue(drag.contains("processInputCommitGateV1.registerPress"))
        XCTAssertTrue(drag.contains("processInputCommitGateV1.commitSample"))
        XCTAssertTrue(coordinate.contains("processInputCommitGateV1.registerPress"))
        XCTAssertTrue(coordinate.contains("processInputCommitGateV1.commitSample"))
        XCTAssertTrue(target.contains("processInputCommitGateV1.registerPress"))
        XCTAssertTrue(legacy.contains("postMousePair("))
        XCTAssertTrue(legacy.contains("postKeyPair("))
        XCTAssertTrue(legacy.contains("processInputCommitGateV1.commitSample"))

        for (name, source) in [
            ("CoordinateDrag.swift", drag),
            ("CoordinateMouseEvent.swift", coordinate),
            ("TargetBoundInput.swift", target),
            ("InputDriver.swift", legacy),
        ] {
            if source.contains("keyDown: true") || source.contains("MouseDown") {
                XCTAssertTrue(
                    source.contains("registerPress"),
                    "\(name) constructs a held-state down without the process gate")
            }
        }
        XCTAssertFalse(socket.contains("@convention(c)"))
        XCTAssertFalse(socket.contains("signalHandler"))
        XCTAssertTrue(socket.contains("DispatchSource.makeSignalSource"))
        XCTAssertTrue(socket.contains("recoverAtStartup()"))
        XCTAssertLessThan(
            socket.range(of: "recoverAtStartup()")!.lowerBound,
            socket.range(of: "print(\"ready\")")!.lowerBound)
    }
}

private final class RecoveryHarness {
    let directory: URL
    let journal: URL
    var now = Date(timeIntervalSince1970: 2_000_000_000)
    var posts: [String] = []
    var recovered: [InputReleaseMetadataV1] = []

    init() throws {
        directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        journal = directory.appendingPathComponent("active-input.json")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    }

    deinit { try? FileManager.default.removeItem(at: directory) }

    func makeGate(removeJournalSucceeds: Bool = true) -> InputCommitGateV1 {
        InputCommitGateV1(
            journalURL: journal,
            now: { self.now },
            makeRecoveryRelease: { metadata in
                PreparedInputReleaseV1(metadata: metadata) {
                    self.recovered.append(metadata)
                    return true
                }
            },
            removeJournalFile: { url in
                guard FileManager.default.fileExists(atPath: url.path) else { return true }
                guard removeJournalSucceeds else { return false }
                do { try FileManager.default.removeItem(at: url); return true }
                catch { return false }
            })
    }

    func writeJournal(createdAt: Date, releases: [InputReleaseMetadataV1]) throws {
        try InputCommitGateV1.writeJournalFixture(
            to: journal, createdAt: createdAt, releases: releases)
    }
}
