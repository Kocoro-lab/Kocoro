import CoreGraphics
import Darwin
import Foundation

/// The only information that may survive an ax_server crash. It is sufficient
/// to synthesize a release, but deliberately cannot contain typed text,
/// clipboard bytes, AX values, application identity, or coordinates.
struct InputReleaseMetadataV1: Codable, Equatable, Hashable {
    let kind: String
    let button: String?
    let virtualKey: UInt16?
    let eventFlags: UInt64?

    static func mouse(button: String) -> Self {
        .init(kind: "mouse", button: button, virtualKey: nil, eventFlags: nil)
    }

    static func key(virtualKey: UInt16, eventFlags: UInt64) -> Self {
        .init(kind: "key", button: nil, virtualKey: virtualKey, eventFlags: eventFlags)
    }

    private enum CodingKeys: String, CodingKey {
        case kind
        case button
        case virtualKey = "virtual_key"
        case eventFlags = "event_flags"
    }

    init(kind: String, button: String?, virtualKey: UInt16?, eventFlags: UInt64?) {
        self.kind = kind
        self.button = button
        self.virtualKey = virtualKey
        self.eventFlags = eventFlags
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        kind = try container.decode(String.self, forKey: .kind)
        switch kind {
        case "mouse":
            guard Set(container.allKeys) == [.kind, .button] else {
                throw InputRecoveryJournalError.invalid
            }
            button = try container.decode(String.self, forKey: .button)
            virtualKey = nil
            eventFlags = nil
        case "key":
            guard Set(container.allKeys) == [.kind, .virtualKey, .eventFlags] else {
                throw InputRecoveryJournalError.invalid
            }
            button = nil
            virtualKey = try container.decode(UInt16.self, forKey: .virtualKey)
            eventFlags = try container.decode(UInt64.self, forKey: .eventFlags)
        default:
            throw InputRecoveryJournalError.invalid
        }
        guard isValid else { throw InputRecoveryJournalError.invalid }
    }

    func encode(to encoder: Encoder) throws {
        guard isValid else { throw InputRecoveryJournalError.invalid }
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(kind, forKey: .kind)
        if kind == "mouse" {
            try container.encode(button!, forKey: .button)
        } else {
            try container.encode(virtualKey!, forKey: .virtualKey)
            try container.encode(eventFlags!, forKey: .eventFlags)
        }
    }

    var isValid: Bool {
        switch kind {
        case "mouse":
            return ["left", "right", "center", "wheel", "back", "forward"].contains(button) &&
                virtualKey == nil && eventFlags == nil
        case "key":
            return button == nil && virtualKey != nil && eventFlags != nil
        default:
            return false
        }
    }
}

struct PreparedInputReleaseV1 {
    let metadata: InputReleaseMetadataV1
    let postAndConfirm: () -> Bool

    init(metadata: InputReleaseMetadataV1, postAndConfirm: @escaping () -> Bool) {
        self.metadata = metadata
        self.postAndConfirm = postAndConfirm
    }
}

enum InputRecoveryStartupResultV1: Equatable {
    case clean
    case recovered(releaseCount: Int)
    case discardedStale
    case blockedMalformed
    case blockedRecoveryFailed
}

struct InputShutdownResultV1: Equatable {
    let signal: Int32
    let confirmedReleaseCount: Int
    let unresolvedReleaseCount: Int
}

private enum InputRecoveryJournalError: Error {
    case invalid
}

private struct InputRecoveryJournalV1: Codable {
    let schemaVersion: Int
    let createdAt: String
    let releases: [InputReleaseMetadataV1]

    private enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case createdAt = "created_at"
        case releases
    }

    init(createdAt: String, releases: [InputReleaseMetadataV1]) {
        schemaVersion = 1
        self.createdAt = createdAt
        self.releases = releases
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        guard Set(container.allKeys) == [.schemaVersion, .createdAt, .releases] else {
            throw InputRecoveryJournalError.invalid
        }
        schemaVersion = try container.decode(Int.self, forKey: .schemaVersion)
        createdAt = try container.decode(String.self, forKey: .createdAt)
        releases = try container.decode([InputReleaseMetadataV1].self, forKey: .releases)
        guard schemaVersion == 1, (1...16).contains(releases.count),
              releases.allSatisfy(\.isValid), Set(releases).count == releases.count else {
            throw InputRecoveryJournalError.invalid
        }
    }
}

/// Serializes every synthetic input commit in this helper. A prepared release
/// is journaled before its matching down event may be posted. Shutdown flips
/// the gate first, waits for any in-flight sample, releases active inputs, and
/// permanently prevents later commits in this process.
final class InputCommitGateV1 {
    static let maximumJournalAge: TimeInterval = 5 * 60
    private static let maximumFutureSkew: TimeInterval = 5
    private static let maximumJournalBytes = 16 * 1024

    private enum State {
        case uninitialized
        case ready
        case blocked
        case shuttingDown
    }

    private let lock = NSLock()
    private let journalURL: URL
    private let now: () -> Date
    private let makeRecoveryRelease: (InputReleaseMetadataV1) -> PreparedInputReleaseV1?
    private let removeJournalFile: (URL) -> Bool
    private var state: State = .uninitialized
    private var active: [UUID: PreparedInputReleaseV1] = [:]
    private var releasesConfirmedDuringShutdown: Set<UUID> = []

    init(
        journalURL: URL,
        now: @escaping () -> Date,
        makeRecoveryRelease: @escaping (InputReleaseMetadataV1) -> PreparedInputReleaseV1?,
        removeJournalFile: @escaping (URL) -> Bool = InputCommitGateV1.removeJournal
    ) {
        self.journalURL = journalURL
        self.now = now
        self.makeRecoveryRelease = makeRecoveryRelease
        self.removeJournalFile = removeJournalFile
    }

    @discardableResult
    func recoverAtStartup() -> InputRecoveryStartupResultV1 {
        lock.lock()
        defer { lock.unlock() }
        switch state {
        case .ready: return .clean
        case .blocked: return .blockedRecoveryFailed
        case .shuttingDown: return .blockedRecoveryFailed
        case .uninitialized: break
        }
        guard FileManager.default.fileExists(atPath: journalURL.path) else {
            state = .ready
            return .clean
        }
        let bytes: Data
        do {
            bytes = try Data(contentsOf: journalURL, options: .mappedIfSafe)
            guard !bytes.isEmpty, bytes.count <= Self.maximumJournalBytes else {
                throw InputRecoveryJournalError.invalid
            }
            try rejectStrictMutationDuplicateJSONMembers(bytes)
        } catch {
            state = .blocked
            return .blockedMalformed
        }
        let journal: InputRecoveryJournalV1
        do {
            journal = try JSONDecoder().decode(InputRecoveryJournalV1.self, from: bytes)
        } catch {
            state = .blocked
            return .blockedMalformed
        }
        guard let createdAt = strictMutationDate(journal.createdAt) else {
            state = .blocked
            return .blockedMalformed
        }
        let age = now().timeIntervalSince(createdAt)
        if age > Self.maximumJournalAge || age < -Self.maximumFutureSkew {
            guard removeJournalLocked() else {
                state = .blocked
                return .blockedRecoveryFailed
            }
            state = .ready
            return .discardedStale
        }
        let releases = journal.releases.compactMap(makeRecoveryRelease)
        guard releases.count == journal.releases.count else {
            state = .blocked
            return .blockedRecoveryFailed
        }
        var confirmed = 0
        for release in releases where release.postAndConfirm() {
            confirmed += 1
        }
        guard confirmed == releases.count, removeJournalLocked() else {
            state = .blocked
            return .blockedRecoveryFailed
        }
        state = .ready
        return .recovered(releaseCount: confirmed)
    }

    /// Returns a token only after the redacted release journal is durable and
    /// the down event has been posted while holding the process-wide gate.
    func registerPress(
        release: PreparedInputReleaseV1,
        commitDown: () -> Bool
    ) -> UUID? {
        lock.lock()
        defer { lock.unlock() }
        guard state == .ready, release.metadata.isValid else { return nil }
        let token = UUID()
        active[token] = release
        guard persistActiveLocked() else {
            active.removeValue(forKey: token)
            state = .blocked
            return nil
        }
        guard commitDown() else {
            active.removeValue(forKey: token)
            if !persistActiveLocked() { state = .blocked }
            return nil
        }
        return token
    }

    /// Used by pointer movement/drag samples and other synthetic input without
    /// a held-state recovery record. No sample can begin after shutdown flips.
    func commitSample(_ commit: () -> Bool) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard state == .ready else { return false }
        return commit()
    }

    func canAdmitInput() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return state == .ready
    }

    /// Clears the journal entry only after the pre-created release has been
    /// posted and confirmed by its production checker.
    func confirmRelease(token: UUID) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if releasesConfirmedDuringShutdown.contains(token) { return true }
        // `.blocked` quarantines NEW input (registerPress/commitSample/
        // canAdmitInput all require `.ready`) but must never suppress a
        // release. Posting a keyUp/mouseUp is idempotent and can only reduce
        // held state, so refusing here is what strands the remaining modifiers
        // of a multi-modifier sequence: releaseHeld() keeps iterating, but each
        // later confirmRelease would return false without posting, leaving a
        // physically held Command/Shift on the user's machine until the helper
        // restarts. Shutdown owns the releases once `.shuttingDown` flips.
        guard state == .ready || state == .blocked,
              let release = active[token] else { return false }
        guard release.postAndConfirm() else {
            // The release journal is still durable and the held-state may
            // still be active. Quarantine new input immediately: accepting
            // another sample or down here could compound an unresolved
            // mouse/key state. Signal shutdown and the next helper startup
            // retain their normal retry/recovery paths through `active` plus
            // the journal.
            state = .blocked
            return false
        }
        active.removeValue(forKey: token)
        let persisted = persistActiveLocked()
        if !persisted { state = .blocked }
        return persisted
    }

    func shutdownForSignal(_ signal: Int32) -> InputShutdownResultV1 {
        lock.lock()
        defer { lock.unlock() }
        guard state != .shuttingDown else {
            return .init(
                signal: signal,
                confirmedReleaseCount: releasesConfirmedDuringShutdown.count,
                unresolvedReleaseCount: active.count)
        }
        state = .shuttingDown
        var confirmed = 0
        var confirmedTokens: [UUID] = []
        for (token, release) in Array(active) where release.postAndConfirm() {
            confirmedTokens.append(token)
        }
        for token in confirmedTokens {
            active.removeValue(forKey: token)
            releasesConfirmedDuringShutdown.insert(token)
            confirmed += 1
        }
        _ = persistActiveLocked()
        return .init(
            signal: signal,
            confirmedReleaseCount: confirmed,
            unresolvedReleaseCount: active.count)
    }

    static func writeJournalFixture(
        to url: URL,
        createdAt: Date,
        releases: [InputReleaseMetadataV1]
    ) throws {
        let journal = InputRecoveryJournalV1(
            createdAt: inputRecoveryTimestamp(createdAt), releases: releases)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        try encoder.encode(journal).write(to: url, options: [.atomic])
    }

    private func persistActiveLocked() -> Bool {
        if active.isEmpty { return removeJournalLocked() }
        let releases = Array(Set(active.values.map(\.metadata))).sorted {
            inputRecoveryMetadataSortKey($0) < inputRecoveryMetadataSortKey($1)
        }
        do {
            try FileManager.default.createDirectory(
                at: journalURL.deletingLastPathComponent(),
                withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700])
            let journal = InputRecoveryJournalV1(
                createdAt: inputRecoveryTimestamp(now()), releases: releases)
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.sortedKeys]
            let data = try encoder.encode(journal)
            guard data.count <= Self.maximumJournalBytes else { return false }
            try data.write(to: journalURL, options: [.atomic])
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o600], ofItemAtPath: journalURL.path)
            return true
        } catch {
            return false
        }
    }

    private func removeJournalLocked() -> Bool {
        removeJournalFile(journalURL)
    }

    private static func removeJournal(_ url: URL) -> Bool {
        do {
            if FileManager.default.fileExists(atPath: url.path) {
                try FileManager.default.removeItem(at: url)
            }
            return true
        } catch { return false }
    }
}

private func inputRecoveryTimestamp(_ date: Date) -> String {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter.string(from: date)
}

private func inputRecoveryMetadataSortKey(_ metadata: InputReleaseMetadataV1) -> String {
    if metadata.kind == "mouse" { return "mouse:\(metadata.button ?? "")" }
    return "key:\(metadata.virtualKey ?? 0):\(metadata.eventFlags ?? 0)"
}

func productionInputRelease(
    metadata: InputReleaseMetadataV1,
    eventSource: CGEventSource? = nil
) -> PreparedInputReleaseV1? {
    switch metadata.kind {
    case "mouse":
        let normalized = metadata.button == "center" ? "wheel" : metadata.button
        guard let name = normalized,
              let mapping = coordinateMouseButtonMappingV1(name) else { return nil }
        let button = mapping.button
        let type = mapping.upType
        return PreparedInputReleaseV1(metadata: metadata) {
            let location = CGEvent(source: nil)?.location ?? .zero
            guard let event = CGEvent(
                mouseEventSource: eventSource, mouseType: type,
                mouseCursorPosition: location, mouseButton: button) else { return false }
            event.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.005)
            return !CGEventSource.buttonState(.combinedSessionState, button: button)
        }
    case "key":
        guard let virtualKey = metadata.virtualKey,
              let flags = metadata.eventFlags,
              let event = CGEvent(
                keyboardEventSource: eventSource,
                virtualKey: CGKeyCode(virtualKey), keyDown: false) else {
            return nil
        }
        event.flags = CGEventFlags(rawValue: flags)
        return PreparedInputReleaseV1(metadata: metadata) {
            event.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.005)
            return !CGEventSource.keyState(
                .combinedSessionState, key: CGKeyCode(virtualKey))
        }
    default:
        return nil
    }
}

let processInputCommitGateV1 = InputCommitGateV1(
    journalURL: URL(fileURLWithPath: "/tmp", isDirectory: true).appendingPathComponent(
        "run.shannon.kocoro.ax-active-input-v1-\(getuid()).json"),
    now: Date.init,
    makeRecoveryRelease: { productionInputRelease(metadata: $0) })
