import AppKit
import CoreGraphics
import Foundation

struct DisplayTopologyQuartzDisplaySnapshot: Equatable {
    let displayID: UInt32
    let isMain: Bool
    let isBuiltin: Bool
    let isActive: Bool
    let isOnline: Bool
    let isAsleep: Bool
    let bounds: DisplayTopologyRectV1
    let rotationDegrees: Double
    let mirrorMasterDisplayID: UInt32?
}

struct DisplayTopologyAppKitScreenSnapshot: Equatable {
    let displayID: UInt32
    let frame: DisplayTopologyRectV1
    let visibleFrame: DisplayTopologyRectV1
    let backingScaleFactor: Double
    let pixelWidth: Int
    let pixelHeight: Int
}

struct DisplayTopologyRawSnapshot: Equatable {
    let mainDisplayID: UInt32
    var quartzDisplays: [DisplayTopologyQuartzDisplaySnapshot]
    var appKitScreens: [DisplayTopologyAppKitScreenSnapshot]
}

enum DisplayTopologyLiveError: Error, Equatable {
    case invalid(String)
    case coreGraphics(String)
}

private let displayTopologySetMismatchReason =
    "CoreGraphics and NSScreen display IDs do not match exactly"

private struct DisplayTopologyStructuralSignature: Encodable {
    let mainDisplayID: UInt32
    let displays: [DisplayTopologyDisplayV1]

    enum CodingKeys: String, CodingKey {
        case mainDisplayID = "main_display_id"
        case displays
    }
}

final class DisplayTopologyObservationBuilder {
    private let helperBootID: String
    private let topologyID: String
    private var generation: UInt64 = 0
    private var structuralSignature: Data?

    init(helperBootID: String, topologyID: String) {
        self.helperBootID = helperBootID
        self.topologyID = topologyID
    }

    func observe(snapshot: DisplayTopologyRawSnapshot, capturedAt: String) throws -> DisplayTopologyV1 {
        guard !helperBootID.isEmpty, !topologyID.isEmpty else {
            throw DisplayTopologyLiveError.invalid("helper boot and topology IDs are required")
        }
        guard !snapshot.quartzDisplays.isEmpty else {
            throw DisplayTopologyLiveError.invalid("CoreGraphics returned no online displays")
        }

        var quartzByID: [UInt32: DisplayTopologyQuartzDisplaySnapshot] = [:]
        for display in snapshot.quartzDisplays {
            guard quartzByID.updateValue(display, forKey: display.displayID) == nil else {
                throw DisplayTopologyLiveError.invalid("duplicate CoreGraphics display ID \(display.displayID)")
            }
        }
        var screensByID: [UInt32: DisplayTopologyAppKitScreenSnapshot] = [:]
        for screen in snapshot.appKitScreens {
            guard screensByID.updateValue(screen, forKey: screen.displayID) == nil else {
                throw DisplayTopologyLiveError.invalid("ambiguous NSScreen display ID \(screen.displayID)")
            }
        }
        guard Set(quartzByID.keys) == Set(screensByID.keys) else {
            throw DisplayTopologyLiveError.invalid(displayTopologySetMismatchReason)
        }
        let mainDisplays = snapshot.quartzDisplays.filter(\.isMain)
        guard mainDisplays.count == 1,
              mainDisplays[0].displayID == snapshot.mainDisplayID else {
            throw DisplayTopologyLiveError.invalid("CG main display APIs disagree")
        }

        let displays = try quartzByID.keys.sorted().map { displayID -> DisplayTopologyDisplayV1 in
            guard let quartz = quartzByID[displayID], let screen = screensByID[displayID] else {
                throw DisplayTopologyLiveError.invalid("missing display \(displayID)")
            }
            return DisplayTopologyDisplayV1(
                displayID: displayID,
                isMain: quartz.isMain,
                isBuiltin: quartz.isBuiltin,
                isActive: quartz.isActive,
                isOnline: quartz.isOnline,
                isAsleep: quartz.isAsleep,
                quartzBounds: quartz.bounds,
                appKitFrame: screen.frame,
                appKitVisibleFrame: screen.visibleFrame,
                backingScaleFactor: screen.backingScaleFactor,
                pixelWidth: screen.pixelWidth,
                pixelHeight: screen.pixelHeight,
                rotationDegrees: quartz.rotationDegrees,
                mirrorMasterDisplayID: quartz.mirrorMasterDisplayID)
        }
        let signature = try makeWireEncoder().encode(DisplayTopologyStructuralSignature(
            mainDisplayID: snapshot.mainDisplayID,
            displays: displays))
        let nextGeneration: UInt64
        if structuralSignature == nil {
            nextGeneration = 1
        } else if structuralSignature == signature {
            nextGeneration = generation
        } else {
            guard generation < UInt64.max else {
                throw DisplayTopologyLiveError.invalid("display topology generation overflow")
            }
            nextGeneration = generation + 1
        }
        let topology = DisplayTopologyV1(
            schemaVersion: 1,
            topologyID: topologyID,
            helperBootID: helperBootID,
            generation: nextGeneration,
            capturedAt: capturedAt,
            mainDisplayID: snapshot.mainDisplayID,
            displays: displays)
        try topology.validate()

        structuralSignature = signature
        generation = nextGeneration
        return topology
    }
}

func collectDisplayTopologyRawSnapshot(
    refresh: () -> Void,
    readQuartz: () throws -> (mainDisplayID: UInt32, displays: [DisplayTopologyQuartzDisplaySnapshot]),
    readAppKit: () throws -> [DisplayTopologyAppKitScreenSnapshot]
) throws -> DisplayTopologyRawSnapshot {
    refresh()
    let quartz = try readQuartz()
    let screens = try readAppKit()
    return DisplayTopologyRawSnapshot(
        mainDisplayID: quartz.mainDisplayID,
        quartzDisplays: quartz.displays,
        appKitScreens: screens)
}

private func readQuartzDisplayTopology() throws -> (
    mainDisplayID: UInt32,
    displays: [DisplayTopologyQuartzDisplaySnapshot]
) {
    var count: UInt32 = 0
    guard CGGetOnlineDisplayList(0, nil, &count) == .success, count > 0 else {
        throw DisplayTopologyLiveError.coreGraphics("cannot enumerate online displays")
    }
    var displayIDs = [CGDirectDisplayID](repeating: 0, count: Int(count))
    let listError = displayIDs.withUnsafeMutableBufferPointer { buffer in
        CGGetOnlineDisplayList(count, buffer.baseAddress, &count)
    }
    guard listError == .success else {
        throw DisplayTopologyLiveError.coreGraphics("cannot read online display IDs: \(listError.rawValue)")
    }
    displayIDs = Array(displayIDs.prefix(Int(count)))
    let mainDisplayID = CGMainDisplayID()
    let displays = displayIDs.map { displayID in
        let bounds = CGDisplayBounds(displayID)
        let mirrorMaster = CGDisplayMirrorsDisplay(displayID)
        return DisplayTopologyQuartzDisplaySnapshot(
            displayID: displayID,
            isMain: CGDisplayIsMain(displayID) != 0,
            isBuiltin: CGDisplayIsBuiltin(displayID) != 0,
            isActive: CGDisplayIsActive(displayID) != 0,
            isOnline: CGDisplayIsOnline(displayID) != 0,
            isAsleep: CGDisplayIsAsleep(displayID) != 0,
            bounds: DisplayTopologyRectV1(
                x: Double(bounds.origin.x),
                y: Double(bounds.origin.y),
                width: Double(bounds.width),
                height: Double(bounds.height)),
            rotationDegrees: CGDisplayRotation(displayID),
            mirrorMasterDisplayID: mirrorMaster == kCGNullDirectDisplay ? nil : mirrorMaster)
    }
    return (mainDisplayID: mainDisplayID, displays: displays)
}

private func readAppKitDisplayTopology() throws -> [DisplayTopologyAppKitScreenSnapshot] {
    try NSScreen.screens.map { screen in
        let screenNumberKey = NSDeviceDescriptionKey("NSScreenNumber")
        guard let number = screen.deviceDescription[screenNumberKey] as? NSNumber else {
            throw DisplayTopologyLiveError.invalid("NSScreen is missing NSScreenNumber")
        }
        let displayID = CGDirectDisplayID(number.uint32Value)
        guard let mode = CGDisplayCopyDisplayMode(displayID) else {
            throw DisplayTopologyLiveError.coreGraphics("display \(displayID) has no current pixel mode")
        }
        return DisplayTopologyAppKitScreenSnapshot(
            displayID: displayID,
            frame: DisplayTopologyRectV1(
                x: Double(screen.frame.origin.x),
                y: Double(screen.frame.origin.y),
                width: Double(screen.frame.width),
                height: Double(screen.frame.height)),
            visibleFrame: DisplayTopologyRectV1(
                x: Double(screen.visibleFrame.origin.x),
                y: Double(screen.visibleFrame.origin.y),
                width: Double(screen.visibleFrame.width),
                height: Double(screen.visibleFrame.height)),
            backingScaleFactor: Double(screen.backingScaleFactor),
            pixelWidth: mode.pixelWidth,
            pixelHeight: mode.pixelHeight)
    }
}

final class LiveDisplayTopologyService {
    // AppKit can trail CoreGraphics briefly during display reconfiguration.
    // One immediate read plus two 50 ms settle retries covers that transient
    // without relaxing exact-set validation for a persistent mismatch.
    private static let collectionAttempts = 3
    private static let displaySetMismatch = DisplayTopologyLiveError.invalid(
        displayTopologySetMismatchReason)

    private let builder: DisplayTopologyObservationBuilder
    private let now: () -> Date
    private let collect: () throws -> DisplayTopologyRawSnapshot
    private let settleBeforeRetry: () -> Void
    private let lock = NSLock()
    private var lastCapturedDate: Date?
    private var lastCapturedAt: String?

    init(
        helperBootID: String,
        topologyID: String,
        now: @escaping () -> Date = Date.init,
        collect: @escaping () throws -> DisplayTopologyRawSnapshot = {
            try collectDisplayTopologyRawSnapshot(
                refresh: { refreshAppKitState() },
                readQuartz: readQuartzDisplayTopology,
                readAppKit: readAppKitDisplayTopology)
        },
        settleBeforeRetry: @escaping () -> Void = {
            Thread.sleep(forTimeInterval: 0.05)
        }
    ) {
        builder = DisplayTopologyObservationBuilder(
            helperBootID: helperBootID,
            topologyID: topologyID)
        self.now = now
        self.collect = collect
        self.settleBeforeRetry = settleBeforeRetry
    }

    func observe() throws -> DisplayTopologyV1 {
        lock.lock()
        defer { lock.unlock() }

        for attempt in 0..<Self.collectionAttempts {
            do {
                let snapshot = try collect()
                var capturedDate = now()
                var capturedAt = formatDisplayTopologyTimestamp(capturedDate)
                if let lastCapturedDate, let lastCapturedAt,
                   capturedDate <= lastCapturedDate || capturedAt == lastCapturedAt {
                    capturedDate = lastCapturedDate.addingTimeInterval(0.001)
                    capturedAt = formatDisplayTopologyTimestamp(capturedDate)
                }
                let topology = try builder.observe(
                    snapshot: snapshot,
                    capturedAt: capturedAt)
                lastCapturedDate = capturedDate
                lastCapturedAt = capturedAt
                return topology
            } catch let error as DisplayTopologyLiveError {
                guard error == Self.displaySetMismatch,
                      attempt + 1 < Self.collectionAttempts else {
                    throw error
                }
                settleBeforeRetry()
            }
        }
        preconditionFailure("display topology collection attempts exhausted")
    }
}

private func formatDisplayTopologyTimestamp(_ date: Date) -> String {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    formatter.timeZone = TimeZone(secondsFromGMT: 0)
    return formatter.string(from: date)
}

private let liveDisplayTopologyIdentity = UUID().uuidString.lowercased()
let liveDisplayTopologyService = LiveDisplayTopologyService(
    helperBootID: "helper_\(liveDisplayTopologyIdentity)",
    topologyID: "topo_\(liveDisplayTopologyIdentity)")
