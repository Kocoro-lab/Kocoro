import Foundation

struct DisplayTopologyRectV1: Codable, Equatable {
    let x: Double
    let y: Double
    let width: Double
    let height: Double

    func validate(field: String) throws {
        guard x.isFinite, y.isFinite, width.isFinite, height.isFinite else {
            throw DisplayTopologyValidationError.invalid("\(field) must be finite")
        }
        guard width > 0, height > 0 else {
            throw DisplayTopologyValidationError.invalid("\(field) sizes must be positive")
        }
    }
}

struct DisplayTopologyDisplayV1: Encodable, Equatable {
    let displayID: UInt32
    let isMain: Bool
    let isBuiltin: Bool
    let isActive: Bool
    let isOnline: Bool
    let isAsleep: Bool
    let quartzBounds: DisplayTopologyRectV1
    let appKitFrame: DisplayTopologyRectV1
    let appKitVisibleFrame: DisplayTopologyRectV1
    let backingScaleFactor: Double
    let pixelWidth: Int
    let pixelHeight: Int
    let rotationDegrees: Double
    let mirrorMasterDisplayID: UInt32?

    enum CodingKeys: String, CodingKey {
        case displayID = "display_id"
        case isMain = "is_main"
        case isBuiltin = "is_builtin"
        case isActive = "is_active"
        case isOnline = "is_online"
        case isAsleep = "is_asleep"
        case quartzBounds = "quartz_bounds"
        case appKitFrame = "appkit_frame"
        case appKitVisibleFrame = "appkit_visible_frame"
        case backingScaleFactor = "backing_scale_factor"
        case pixelWidth = "pixel_width"
        case pixelHeight = "pixel_height"
        case rotationDegrees = "rotation_degrees"
        case mirrorMasterDisplayID = "mirror_master_display_id"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(displayID, forKey: .displayID)
        try container.encode(isMain, forKey: .isMain)
        try container.encode(isBuiltin, forKey: .isBuiltin)
        try container.encode(isActive, forKey: .isActive)
        try container.encode(isOnline, forKey: .isOnline)
        try container.encode(isAsleep, forKey: .isAsleep)
        try container.encode(quartzBounds, forKey: .quartzBounds)
        try container.encode(appKitFrame, forKey: .appKitFrame)
        try container.encode(appKitVisibleFrame, forKey: .appKitVisibleFrame)
        try container.encode(backingScaleFactor, forKey: .backingScaleFactor)
        try container.encode(pixelWidth, forKey: .pixelWidth)
        try container.encode(pixelHeight, forKey: .pixelHeight)
        try container.encode(rotationDegrees, forKey: .rotationDegrees)
        if let mirrorMasterDisplayID {
            try container.encode(mirrorMasterDisplayID, forKey: .mirrorMasterDisplayID)
        } else {
            try container.encodeNil(forKey: .mirrorMasterDisplayID)
        }
    }
}

struct DisplayTopologyV1: Encodable, Equatable {
    let schemaVersion: Int
    let topologyID: String
    let helperBootID: String
    let generation: UInt64
    let capturedAt: String
    let mainDisplayID: UInt32
    let displays: [DisplayTopologyDisplayV1]

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case topologyID = "topology_id"
        case helperBootID = "helper_boot_id"
        case generation
        case capturedAt = "captured_at"
        case mainDisplayID = "main_display_id"
        case displays
    }

    func validate() throws {
        guard schemaVersion == 1 else {
            throw DisplayTopologyValidationError.invalid("unsupported schema_version \(schemaVersion)")
        }
        guard !topologyID.isEmpty, !helperBootID.isEmpty, generation > 0, mainDisplayID > 0 else {
            throw DisplayTopologyValidationError.invalid("topology authority is required")
        }
        guard isDisplayTopologyTimestamp(capturedAt) else {
            throw DisplayTopologyValidationError.invalid("captured_at must be RFC3339")
        }
        guard !displays.isEmpty else {
            throw DisplayTopologyValidationError.invalid("at least one display is required")
        }

        var displayIDs = Set<UInt32>()
        var mainCount = 0
        for display in displays {
            guard display.displayID > 0 else {
                throw DisplayTopologyValidationError.invalid("display_id must not be zero")
            }
            guard displayIDs.insert(display.displayID).inserted else {
                throw DisplayTopologyValidationError.invalid("duplicate display_id \(display.displayID)")
            }
            if display.isMain { mainCount += 1 }
            try display.quartzBounds.validate(field: "quartz_bounds")
            try display.appKitFrame.validate(field: "appkit_frame")
            try display.appKitVisibleFrame.validate(field: "appkit_visible_frame")
            guard display.backingScaleFactor.isFinite, display.backingScaleFactor > 0 else {
                throw DisplayTopologyValidationError.invalid("backing_scale_factor must be positive and finite")
            }
            guard abs(display.quartzBounds.width - display.appKitFrame.width) <= 0.000_001,
                  abs(display.quartzBounds.height - display.appKitFrame.height) <= 0.000_001 else {
                throw DisplayTopologyValidationError.invalid("Quartz/AppKit logical sizes must match")
            }
            guard display.pixelWidth > 0, display.pixelHeight > 0 else {
                throw DisplayTopologyValidationError.invalid("pixel sizes must be positive")
            }
            guard display.rotationDegrees.isFinite,
                  display.rotationDegrees >= 0,
                  display.rotationDegrees < 360 else {
                throw DisplayTopologyValidationError.invalid("rotation_degrees must be finite and in [0, 360)")
            }
            if let mirrorMasterDisplayID = display.mirrorMasterDisplayID {
                guard mirrorMasterDisplayID > 0 else {
                    throw DisplayTopologyValidationError.invalid("mirror master must not be zero")
                }
                if mirrorMasterDisplayID == display.displayID {
                    throw DisplayTopologyValidationError.invalid("a display cannot mirror itself")
                }
            }
        }
        guard mainCount == 1,
              displays.contains(where: { $0.displayID == mainDisplayID && $0.isMain }) else {
            throw DisplayTopologyValidationError.invalid("main_display_id must identify the unique main display")
        }
        for display in displays {
            if let master = display.mirrorMasterDisplayID, !displayIDs.contains(master) {
                throw DisplayTopologyValidationError.invalid("mirror master \(master) is not in this topology")
            }
        }
    }
}

private func isDisplayTopologyTimestamp(_ value: String) -> Bool {
    let formatter = ISO8601DateFormatter()
    if formatter.date(from: value) != nil {
        return true
    }
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter.date(from: value) != nil
}

enum DisplayTopologyValidationError: Error, Equatable {
    case invalid(String)
}

func encodeDisplayTopologyV1(_ topology: DisplayTopologyV1) throws -> Data {
    try topology.validate()
    return try makeWireEncoder().encode(topology)
}
