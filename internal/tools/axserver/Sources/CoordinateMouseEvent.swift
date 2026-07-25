import AppKit
import CoreGraphics
import Foundation

private struct CoordinateMouseCodingKey: CodingKey, Hashable {
    let stringValue: String
    let intValue: Int? = nil

    init?(stringValue: String) {
        self.stringValue = stringValue
    }

    init?(intValue: Int) {
        return nil
    }
}

enum CoordinateMouseEventWireError: Error, Equatable {
    case invalid(String)
}

struct CoordinateMouseButtonMappingV1: Equatable {
    let button: CGMouseButton
    let downType: CGEventType
    let upType: CGEventType
}

func coordinateMouseButtonMappingV1(_ name: String) -> CoordinateMouseButtonMappingV1? {
    switch name {
    case "left":
        return .init(button: .left, downType: .leftMouseDown, upType: .leftMouseUp)
    case "right":
        return .init(button: .right, downType: .rightMouseDown, upType: .rightMouseUp)
    case "wheel":
        return .init(button: .center, downType: .otherMouseDown, upType: .otherMouseUp)
    case "back":
        guard let button = CGMouseButton(rawValue: 3) else { return nil }
        return .init(button: button, downType: .otherMouseDown, upType: .otherMouseUp)
    case "forward":
        guard let button = CGMouseButton(rawValue: 4) else { return nil }
        return .init(button: button, downType: .otherMouseDown, upType: .otherMouseUp)
    default:
        return nil
    }
}

private struct CoordinateMouseJSONDuplicateMemberScanner {
    private let bytes: [UInt8]
    private var index = 0

    init(_ payload: Data) {
        bytes = Array(payload)
    }

    mutating func validate() throws {
        skipWhitespace()
        try scanValue()
        skipWhitespace()
        guard index == bytes.count else {
            throw CoordinateMouseEventWireError.invalid("trailing JSON data")
        }
    }

    private mutating func scanValue() throws {
        skipWhitespace()
        guard let byte = current else {
            throw CoordinateMouseEventWireError.invalid("missing JSON value")
        }
        switch byte {
        case UInt8(ascii: "{"):
            try scanObject()
        case UInt8(ascii: "["):
            try scanArray()
        case UInt8(ascii: "\""):
            _ = try scanString()
        default:
            try scanPrimitive()
        }
    }

    private mutating func scanObject() throws {
        index += 1
        skipWhitespace()
        if consume(UInt8(ascii: "}")) { return }

        var members: Set<String> = []
        while true {
            skipWhitespace()
            guard current == UInt8(ascii: "\"") else {
                throw CoordinateMouseEventWireError.invalid("JSON object member must be a string")
            }
            let member = try scanString()
            guard members.insert(member).inserted else {
                throw CoordinateMouseEventWireError.invalid(
                    "duplicate JSON object member: \(member)")
            }
            skipWhitespace()
            guard consume(UInt8(ascii: ":")) else {
                throw CoordinateMouseEventWireError.invalid("JSON object member is missing ':'")
            }
            try scanValue()
            skipWhitespace()
            if consume(UInt8(ascii: "}")) { return }
            guard consume(UInt8(ascii: ",")) else {
                throw CoordinateMouseEventWireError.invalid("invalid JSON object separator")
            }
        }
    }

    private mutating func scanArray() throws {
        index += 1
        skipWhitespace()
        if consume(UInt8(ascii: "]")) { return }

        while true {
            try scanValue()
            skipWhitespace()
            if consume(UInt8(ascii: "]")) { return }
            guard consume(UInt8(ascii: ",")) else {
                throw CoordinateMouseEventWireError.invalid("invalid JSON array separator")
            }
        }
    }

    private mutating func scanString() throws -> String {
        let start = index
        guard consume(UInt8(ascii: "\"")) else {
            throw CoordinateMouseEventWireError.invalid("expected JSON string")
        }
        var escaped = false
        while let byte = current {
            index += 1
            if escaped {
                escaped = false
                continue
            }
            if byte == UInt8(ascii: "\\") {
                escaped = true
                continue
            }
            if byte == UInt8(ascii: "\"") {
                let encoded = Data(bytes[start..<index])
                do {
                    return try JSONDecoder().decode(String.self, from: encoded)
                } catch {
                    throw CoordinateMouseEventWireError.invalid("invalid JSON string")
                }
            }
            if byte < 0x20 {
                throw CoordinateMouseEventWireError.invalid("unescaped control byte in JSON string")
            }
        }
        throw CoordinateMouseEventWireError.invalid("unterminated JSON string")
    }

    private mutating func scanPrimitive() throws {
        let start = index
        while let byte = current,
              !Self.isWhitespace(byte),
              byte != UInt8(ascii: ","),
              byte != UInt8(ascii: "]"),
              byte != UInt8(ascii: "}") {
            index += 1
        }
        guard index > start else {
            throw CoordinateMouseEventWireError.invalid("invalid JSON primitive")
        }
    }

    private var current: UInt8? {
        index < bytes.count ? bytes[index] : nil
    }

    private mutating func consume(_ byte: UInt8) -> Bool {
        guard current == byte else { return false }
        index += 1
        return true
    }

    private mutating func skipWhitespace() {
        while let byte = current, Self.isWhitespace(byte) {
            index += 1
        }
    }

    private static func isWhitespace(_ byte: UInt8) -> Bool {
        byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D
    }
}

private func rejectCoordinateMouseDuplicateJSONMembers(_ payload: Data) throws {
    var scanner = CoordinateMouseJSONDuplicateMemberScanner(payload)
    try scanner.validate()
}

private func requireCoordinateMouseKeys(
    _ container: KeyedDecodingContainer<CoordinateMouseCodingKey>,
    exactly expected: Set<String>,
    field: String
) throws {
    let actual = Set(container.allKeys.map(\.stringValue))
    guard actual == expected else {
        throw CoordinateMouseEventWireError.invalid(
            "\(field) keys differ: expected \(expected.sorted()), got \(actual.sorted())")
    }
}

private func coordinateMouseKey(_ value: String) -> CoordinateMouseCodingKey {
    CoordinateMouseCodingKey(stringValue: value)!
}

private func coordinateMouseExactIdentity(_ value: String) -> Bool {
    !value.isEmpty &&
        value == value.trimmingCharacters(in: .whitespacesAndNewlines)
}

struct CoordinateMouseEventPointV1: Codable, Equatable {
    let x: Double
    let y: Double

    init(x: Double, y: Double) {
        self.x = x
        self.y = y
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(container, exactly: ["x", "y"], field: "quartz_point")
        x = try container.decode(Double.self, forKey: coordinateMouseKey("x"))
        y = try container.decode(Double.self, forKey: coordinateMouseKey("y"))
        guard x.isFinite, y.isFinite else {
            throw CoordinateMouseEventWireError.invalid("quartz_point must be finite")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try container.encode(x, forKey: coordinateMouseKey("x"))
        try container.encode(y, forKey: coordinateMouseKey("y"))
    }
}

struct CoordinateMouseEventTopologyRefV1: Codable, Equatable {
    let topologyID: String
    let generation: UInt64

    init(topologyID: String, generation: UInt64) {
        self.topologyID = topologyID
        self.generation = generation
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(
            container,
            exactly: ["topology_id", "generation"],
            field: "topology_ref")
        topologyID = try container.decode(String.self, forKey: coordinateMouseKey("topology_id"))
        generation = try container.decode(UInt64.self, forKey: coordinateMouseKey("generation"))
        guard coordinateMouseExactIdentity(topologyID), generation > 0 else {
            throw CoordinateMouseEventWireError.invalid("topology_ref authority is required")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try container.encode(topologyID, forKey: coordinateMouseKey("topology_id"))
        try container.encode(generation, forKey: coordinateMouseKey("generation"))
    }
}

private func coordinateMouseRiskOpaqueID(_ value: String) -> Bool {
    guard coordinateMouseExactIdentity(value), value.utf8.count <= 128 else { return false }
    return value.unicodeScalars.allSatisfy { scalar in
        scalar.isASCII && (CharacterSet.alphanumerics.contains(scalar) ||
            [".", "_", ":", "-"].contains(Character(scalar)))
    }
}

private func coordinateMouseRiskPath(_ value: String) -> Bool {
    guard value.utf8.count <= 512, value.hasPrefix("window[0]/") else { return false }
    let components = value.split(separator: "/")
    guard components.first == "window[0]", components.count > 1 else { return false }
    return components.dropFirst().allSatisfy { component in
        guard component.hasPrefix("AX"), component.hasSuffix("]"),
              let bracket = component.lastIndex(of: "[") else { return false }
        let role = component[..<bracket]
        let index = component[component.index(after: bracket)..<component.index(before: component.endIndex)]
        return role.count > 2 && role.dropFirst(2).allSatisfy { $0.isASCII && $0.isLetter || $0.isNumber } &&
            !index.isEmpty && index.allSatisfy(\.isNumber)
    }
}

private func coordinateMouseRiskSHA256(_ value: String) -> Bool {
    value.utf8.count == 64 && value.unicodeScalars.allSatisfy {
        (UInt8(ascii: "0")...UInt8(ascii: "9")).contains(UInt8($0.value)) ||
            (UInt8(ascii: "a")...UInt8(ascii: "f")).contains(UInt8($0.value))
    }
}

private func coordinateMouseRiskLabel(_ value: String) -> Bool {
    guard strictMutationIdentity(value), value.unicodeScalars.count <= 128 else { return false }
    return value.unicodeScalars.allSatisfy { scalar in
        !CharacterSet.controlCharacters.contains(scalar) &&
            scalar.properties.generalCategory != .format
    }
}

struct CoordinateMouseRiskPixelPointV1: Decodable, Equatable {
    let x: Int
    let y: Int

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(container, exactly: ["x", "y"], field: "source_pixel")
        x = try container.decode(Int.self, forKey: coordinateMouseKey("x"))
        y = try container.decode(Int.self, forKey: coordinateMouseKey("y"))
        guard x >= 0, y >= 0, x <= Int(Int32.max), y <= Int(Int32.max) else {
            throw CoordinateMouseEventWireError.invalid("invalid coordinate risk source_pixel")
        }
    }
}

struct CoordinateMouseRiskAuthorityV1: Decodable, Equatable {
    let elementPath: String
    let frameID: String
    let frameExpiresAt: String
    let finalImageSHA256: String
    let topologyRef: CoordinateMouseEventTopologyRefV1
    let helperBootID: String
    let displayID: UInt32
    let sourcePixel: CoordinateMouseRiskPixelPointV1
    let quartzPoint: CoordinateMouseEventPointV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(container, exactly: [
            "element_path", "frame_id", "frame_expires_at", "final_image_sha256",
            "topology_ref", "helper_boot_id", "display_id", "source_pixel", "quartz_point",
        ], field: "coordinate risk authority")
        elementPath = try container.decode(String.self, forKey: coordinateMouseKey("element_path"))
        frameID = try container.decode(String.self, forKey: coordinateMouseKey("frame_id"))
        frameExpiresAt = try container.decode(String.self, forKey: coordinateMouseKey("frame_expires_at"))
        finalImageSHA256 = try container.decode(String.self, forKey: coordinateMouseKey("final_image_sha256"))
        topologyRef = try container.decode(
            CoordinateMouseEventTopologyRefV1.self, forKey: coordinateMouseKey("topology_ref"))
        helperBootID = try container.decode(String.self, forKey: coordinateMouseKey("helper_boot_id"))
        let rawDisplayID = try container.decode(Int.self, forKey: coordinateMouseKey("display_id"))
        sourcePixel = try container.decode(
            CoordinateMouseRiskPixelPointV1.self, forKey: coordinateMouseKey("source_pixel"))
        quartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self, forKey: coordinateMouseKey("quartz_point"))
        guard coordinateMouseRiskPath(elementPath),
              (frameID.hasPrefix("frame_") || frameID.hasPrefix("frame-")),
              coordinateMouseRiskOpaqueID(frameID),
              strictMutationDate(frameExpiresAt) != nil,
              coordinateMouseRiskSHA256(finalImageSHA256),
              coordinateMouseRiskOpaqueID(helperBootID),
              let exactDisplayID = UInt32(exactly: rawDisplayID), exactDisplayID > 0 else {
            throw CoordinateMouseEventWireError.invalid("invalid coordinate risk authority")
        }
        displayID = exactDisplayID
    }
}

struct CoordinateMouseRiskAssertionV1: Decodable, Equatable {
    let kind: String
    let riskKind: String
    let elementRef: String
    let expectedRole: String
    let expectedFingerprint: String
    let coordinateAuthority: CoordinateMouseRiskAuthorityV1
    let destinationAssertion: SemanticPressRiskDestinationAssertionV2

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(container, exactly: [
            "kind", "risk_kind", "element_ref", "expected_role", "expected_fingerprint",
            "coordinate_authority", "destination_assertion",
        ], field: "coordinate risk assertion")
        kind = try container.decode(String.self, forKey: coordinateMouseKey("kind"))
        riskKind = try container.decode(String.self, forKey: coordinateMouseKey("risk_kind"))
        elementRef = try container.decode(String.self, forKey: coordinateMouseKey("element_ref"))
        expectedRole = try container.decode(String.self, forKey: coordinateMouseKey("expected_role"))
        expectedFingerprint = try container.decode(
            String.self, forKey: coordinateMouseKey("expected_fingerprint"))
        coordinateAuthority = try container.decode(
            CoordinateMouseRiskAuthorityV1.self, forKey: coordinateMouseKey("coordinate_authority"))
        destinationAssertion = try container.decode(
            SemanticPressRiskDestinationAssertionV2.self,
            forKey: coordinateMouseKey("destination_assertion"))
        guard kind == "consequential_click_v1",
              ["send", "delete", "purchase"].contains(riskKind),
              elementRef.count > 1, elementRef.first == "e",
              elementRef.dropFirst().allSatisfy(\.isNumber),
              expectedRole.hasPrefix("AX"), coordinateMouseRiskOpaqueID(expectedRole),
              expectedFingerprint.hasPrefix("axf_"),
              expectedFingerprint.utf8.count == 68,
              coordinateMouseRiskSHA256(String(expectedFingerprint.dropFirst(4))),
              coordinateMouseRiskLabel(destinationAssertion.expectedWindowTitle) else {
            throw CoordinateMouseEventWireError.invalid("invalid coordinate risk assertion")
        }
    }
}

private struct CoordinateMouseEventRectPayloadV1: Decodable {
    let value: DisplayTopologyRectV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(
            container,
            exactly: ["x", "y", "width", "height"],
            field: "expected_window_quartz_bounds")
        value = DisplayTopologyRectV1(
            x: try container.decode(Double.self, forKey: coordinateMouseKey("x")),
            y: try container.decode(Double.self, forKey: coordinateMouseKey("y")),
            width: try container.decode(Double.self, forKey: coordinateMouseKey("width")),
            height: try container.decode(Double.self, forKey: coordinateMouseKey("height")))
        do {
            try value.validate(field: "expected_window_quartz_bounds")
        } catch {
            throw CoordinateMouseEventWireError.invalid("invalid expected_window_quartz_bounds")
        }
    }
}

struct CoordinateMouseEventRequestV1: Equatable, Decodable {
    let schemaVersion: Int
    let topologyRef: CoordinateMouseEventTopologyRefV1
    let helperBootID: String
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let expectedWindowQuartzBounds: DisplayTopologyRectV1
    let displayID: UInt32
    let quartzPoint: CoordinateMouseEventPointV1
    let action: String
    let button: String?
    let clickCount: Int?
    let modifiers: [String]
    let commitDeadlineAt: String
    let riskAssertion: CoordinateMouseRiskAssertionV1?

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(
            container,
            exactly: [
                "schema_version", "topology_ref", "helper_boot_id", "pid", "bundle_id",
                "window_id", "expected_window_quartz_bounds", "display_id", "quartz_point",
                "action", "button", "click_count", "modifiers", "commit_deadline_at",
                "risk_assertion",
            ],
            field: "coordinate_mouse_event params")

        schemaVersion = try container.decode(Int.self, forKey: coordinateMouseKey("schema_version"))
        topologyRef = try container.decode(
            CoordinateMouseEventTopologyRefV1.self,
            forKey: coordinateMouseKey("topology_ref"))
        helperBootID = try container.decode(String.self, forKey: coordinateMouseKey("helper_boot_id"))
        pid = try container.decode(Int.self, forKey: coordinateMouseKey("pid"))
        bundleID = try container.decode(String.self, forKey: coordinateMouseKey("bundle_id"))
        let rawWindowID = try container.decode(Int.self, forKey: coordinateMouseKey("window_id"))
        let bounds = try container.decode(
            CoordinateMouseEventRectPayloadV1.self,
            forKey: coordinateMouseKey("expected_window_quartz_bounds"))
        expectedWindowQuartzBounds = bounds.value
        let rawDisplayID = try container.decode(Int.self, forKey: coordinateMouseKey("display_id"))
        quartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self,
            forKey: coordinateMouseKey("quartz_point"))
        action = try container.decode(String.self, forKey: coordinateMouseKey("action"))
        button = try container.decodeIfPresent(String.self, forKey: coordinateMouseKey("button"))
        clickCount = try container.decodeIfPresent(Int.self, forKey: coordinateMouseKey("click_count"))
        modifiers = try container.decode(
            [String].self, forKey: coordinateMouseKey("modifiers"))
        commitDeadlineAt = try container.decode(
            String.self,
            forKey: coordinateMouseKey("commit_deadline_at"))
        riskAssertion = try container.decodeIfPresent(
            CoordinateMouseRiskAssertionV1.self,
            forKey: coordinateMouseKey("risk_assertion"))

        guard schemaVersion == 1,
              coordinateMouseExactIdentity(helperBootID),
              pid > 0,
              coordinateMouseExactIdentity(bundleID),
              let exactWindowID = UInt32(exactly: rawWindowID), exactWindowID > 0,
              let exactDisplayID = UInt32(exactly: rawDisplayID), exactDisplayID > 0,
              strictInputModifiersV1(modifiers),
              coordinateMouseExactIdentity(commitDeadlineAt),
              coordinateMouseEventDate(commitDeadlineAt) != nil else {
            throw CoordinateMouseEventWireError.invalid("invalid coordinate_mouse_event authority")
        }
        windowID = exactWindowID
        displayID = exactDisplayID

        switch action {
        case "move":
            guard button == nil, clickCount == nil, riskAssertion == nil else {
                throw CoordinateMouseEventWireError.invalid(
                    "move requires explicit null button, click_count, and risk_assertion")
            }
        case "click":
            guard let button, coordinateMouseButtonMappingV1(button) != nil,
                  let clickCount,
                  (1...3).contains(clickCount) else {
                throw CoordinateMouseEventWireError.invalid(
                    "click requires an admitted button and click_count in 1...3")
            }
        default:
            throw CoordinateMouseEventWireError.invalid("unsupported coordinate mouse action")
        }
        if let riskAssertion {
            guard button == "left", clickCount == 1,
                  riskAssertion.coordinateAuthority.topologyRef == topologyRef,
                  riskAssertion.coordinateAuthority.helperBootID == helperBootID,
                  riskAssertion.coordinateAuthority.displayID == displayID,
                  riskAssertion.coordinateAuthority.quartzPoint == quartzPoint,
                  let commitDeadline = coordinateMouseEventDate(commitDeadlineAt),
                  let frameExpiry = strictMutationDate(
                    riskAssertion.coordinateAuthority.frameExpiresAt),
                  commitDeadline <= frameExpiry else {
                throw CoordinateMouseEventWireError.invalid(
                    "coordinate risk assertion does not bind exact request authority")
            }
        }
    }
}

struct CoordinateMouseEventRPCRequestV1: Decodable, Equatable {
    let id: Int64
    let method: String
    let params: CoordinateMouseEventRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CoordinateMouseCodingKey.self)
        try requireCoordinateMouseKeys(
            container,
            exactly: ["id", "method", "params"],
            field: "coordinate_mouse_event envelope")
        id = try container.decode(Int64.self, forKey: coordinateMouseKey("id"))
        method = try container.decode(String.self, forKey: coordinateMouseKey("method"))
        params = try container.decode(
            CoordinateMouseEventRequestV1.self,
            forKey: coordinateMouseKey("params"))
        guard id > 0, method == "coordinate_mouse_event" else {
            throw CoordinateMouseEventWireError.invalid("invalid coordinate_mouse_event envelope")
        }
    }
}

func decodeCoordinateMouseEventRPCRequestV1(_ payload: Data) throws -> CoordinateMouseEventRPCRequestV1 {
    try rejectCoordinateMouseDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(CoordinateMouseEventRPCRequestV1.self, from: payload)
}

struct CoordinateMouseEventPointerEndpointV1: Codable, Equatable {
    let requested: CoordinateMouseEventPointV1
    let observed: CoordinateMouseEventPointV1?
    let tolerance: Double
    let verified: Bool

    enum CodingKeys: String, CodingKey, CaseIterable {
        case requested, observed, tolerance, verified
    }

    init(
        requested: CoordinateMouseEventPointV1,
        observed: CoordinateMouseEventPointV1?,
        tolerance: Double,
        verified: Bool
    ) {
        self.requested = requested
        self.observed = observed
        self.tolerance = tolerance
        self.verified = verified
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        guard Set(container.allKeys) == Set(CodingKeys.allCases) else {
            throw CoordinateMouseEventWireError.invalid("invalid pointer_endpoint keys")
        }
        requested = try container.decode(CoordinateMouseEventPointV1.self, forKey: .requested)
        observed = try container.decodeIfPresent(CoordinateMouseEventPointV1.self, forKey: .observed)
        tolerance = try container.decode(Double.self, forKey: .tolerance)
        verified = try container.decode(Bool.self, forKey: .verified)
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(requested, forKey: .requested)
        if let observed {
            try container.encode(observed, forKey: .observed)
        } else {
            try container.encodeNil(forKey: .observed)
        }
        try container.encode(tolerance, forKey: .tolerance)
        try container.encode(verified, forKey: .verified)
    }
}

struct CoordinateMouseEventResultV1: Encodable, Equatable {
    let schemaVersion: Int
    let status: String
    let action: String
    let primaryActionCommitted: Bool
    let pointerMotionCommitted: Bool
    let phase: String
    let failureCode: String?
    let retrySafe: Bool
    let pointerEndpoint: CoordinateMouseEventPointerEndpointV1?

    enum CodingKeys: String, CodingKey {
        case status, action, phase
        case schemaVersion = "schema_version"
        case primaryActionCommitted = "primary_action_committed"
        case pointerMotionCommitted = "pointer_motion_committed"
        case failureCode = "failure_code"
        case retrySafe = "retry_safe"
        case pointerEndpoint = "pointer_endpoint"
    }

    init(
        status: String,
        action: String,
        primaryActionCommitted: Bool,
        pointerMotionCommitted: Bool,
        phase: String,
        failureCode: String?,
        pointerEndpoint: CoordinateMouseEventPointerEndpointV1?
    ) {
        schemaVersion = 1
        self.status = status
        self.action = action
        self.primaryActionCommitted = primaryActionCommitted
        self.pointerMotionCommitted = pointerMotionCommitted
        self.phase = phase
        self.failureCode = failureCode
        retrySafe = false
        self.pointerEndpoint = pointerEndpoint
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(schemaVersion, forKey: .schemaVersion)
        try container.encode(status, forKey: .status)
        try container.encode(action, forKey: .action)
        try container.encode(primaryActionCommitted, forKey: .primaryActionCommitted)
        try container.encode(pointerMotionCommitted, forKey: .pointerMotionCommitted)
        try container.encode(phase, forKey: .phase)
        if let failureCode {
            try container.encode(failureCode, forKey: .failureCode)
        } else {
            try container.encodeNil(forKey: .failureCode)
        }
        try container.encode(retrySafe, forKey: .retrySafe)
        if let pointerEndpoint {
            try container.encode(pointerEndpoint, forKey: .pointerEndpoint)
        } else {
            try container.encodeNil(forKey: .pointerEndpoint)
        }
    }
}

struct CoordinateMousePreparedClick {
    enum PostOutcome {
        case notCommitted
        case committed
        case partiallyCommitted
    }

    let post: () -> PostOutcome

    init(_ post: @escaping () -> PostOutcome) {
        self.post = post
    }
}

enum CoordinateMousePointerMoveOutcome {
    case notCommitted
    case committed(
        observed: CoordinateMouseEventPointV1?,
        expectedSyntheticEventCount: UInt32
    )
}

enum CoordinateMouseRiskVerificationOutcomeV1: Equatable {
    case matched
    case drift(String)
    case unavailable(String)
}

struct CoordinateMouseEventDependencies {
    let observeTopology: () throws -> DisplayTopologyV1
    let isPIDLive: (Int) -> Bool
    let bundleIDForPID: (Int) -> String?
    let exactWindow: (UInt32) throws -> CaptureCoordinateWindowWindowSnapshot?
    let frontmostWindowAtPoint: (CoordinateMouseEventPointV1) -> UInt32?
    let prepareClick: (CoordinateMouseEventPointV1, String, Int) -> CoordinateMousePreparedClick?
    let movePointer: (CoordinateMouseEventPointV1) -> CoordinateMousePointerMoveOutcome
    let observePointer: () -> CoordinateMouseEventPointV1?
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let verifyRiskTarget: (
        CoordinateMouseEventRequestV1, CoordinateMouseRiskAssertionV1, TimeInterval
    ) -> CoordinateMouseRiskVerificationOutcomeV1
    let sleep: (TimeInterval) -> Void
    let now: () -> Date
}

private let coordinateMouseEndpointToleranceV1 = 2.0
private let coordinateMouseMaximumDeadlineHorizonV1 = 2.0
private let coordinateMouseInterferenceSettleDelayV1 = 0.012
private let coordinateMouseCounterSettleDelayV1 = 0.005
private let coordinateMouseMaximumCounterSettleAttemptsV1 = 10

func coordinateMouseAssessPhysicalInputV1(
    baseline: PhysicalInputInterferenceSnapshotV1?,
    expectedPointer: CoordinateMouseEventPointV1?,
    expectedSyntheticEvents: [(CGEventType, UInt32)],
    expectedSyntheticHeldModifierFlags: UInt64,
    observe: () -> PhysicalInputInterferenceSnapshotV1?,
    settle: () -> Void,
    maximumSettleAttempts: Int =
        coordinateMouseMaximumCounterSettleAttemptsV1
) -> (
    assessment: PhysicalInputInterferenceAssessmentV1,
    snapshot: PhysicalInputInterferenceSnapshotV1?
) {
    var settleAttempt = 0
    while true {
        let current = observe()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: baseline,
            current: current,
            expectedPointer: expectedPointer,
            expectedSyntheticEvents: expectedSyntheticEvents,
            expectedSyntheticHeldModifierFlags:
                expectedSyntheticHeldModifierFlags
        )
        if assessment != .unavailable ||
            expectedSyntheticEvents.isEmpty ||
            settleAttempt >= max(0, maximumSettleAttempts) {
            return (assessment, current)
        }
        settleAttempt += 1
        settle()
    }
}

private func coordinateMouseFailure(
    request: CoordinateMouseEventRequestV1,
    code: String,
    phase: String = "preflight",
    primaryCommitted: Bool = false,
    pointerCommitted: Bool = false,
    endpoint: CoordinateMouseEventPointerEndpointV1? = nil,
    status: String = "failed"
) -> CoordinateMouseEventResultV1 {
    CoordinateMouseEventResultV1(
        status: status,
        action: request.action,
        primaryActionCommitted: primaryCommitted,
        pointerMotionCommitted: pointerCommitted,
        phase: phase,
        failureCode: code,
        pointerEndpoint: endpoint)
}

private func coordinateMouseEndpoint(
    requested: CoordinateMouseEventPointV1,
    observed: CoordinateMouseEventPointV1?
) -> CoordinateMouseEventPointerEndpointV1 {
    let verified = observed.map {
        abs(requested.x - $0.x) <= coordinateMouseEndpointToleranceV1 &&
            abs(requested.y - $0.y) <= coordinateMouseEndpointToleranceV1
    } ?? false
    return CoordinateMouseEventPointerEndpointV1(
        requested: requested,
        observed: observed,
        tolerance: coordinateMouseEndpointToleranceV1,
        verified: verified)
}

private func coordinateMouseRectsEqual(
    _ left: DisplayTopologyRectV1,
    _ right: DisplayTopologyRectV1
) -> Bool {
    abs(left.x - right.x) <= 0.000_001 &&
        abs(left.y - right.y) <= 0.000_001 &&
        abs(left.width - right.width) <= 0.000_001 &&
        abs(left.height - right.height) <= 0.000_001
}

private func coordinateMouseRect(
    _ outer: DisplayTopologyRectV1,
    fullyContains inner: DisplayTopologyRectV1
) -> Bool {
    inner.x >= outer.x && inner.y >= outer.y &&
        inner.x + inner.width <= outer.x + outer.width &&
        inner.y + inner.height <= outer.y + outer.height
}

private func coordinateMouseRect(
    _ rect: DisplayTopologyRectV1,
    contains point: CoordinateMouseEventPointV1
) -> Bool {
    point.x >= rect.x && point.y >= rect.y &&
        point.x < rect.x + rect.width && point.y < rect.y + rect.height
}

private func coordinateMouseAuthorityFailure(
    request: CoordinateMouseEventRequestV1,
    dependencies: CoordinateMouseEventDependencies
) -> String? {
    let topology: DisplayTopologyV1
    do {
        topology = try dependencies.observeTopology()
        try topology.validate()
    } catch {
        return "topology_unavailable"
    }
    guard topology.topologyID == request.topologyRef.topologyID,
          topology.generation == request.topologyRef.generation else {
        return "stale_topology"
    }
    guard topology.helperBootID == request.helperBootID else {
        return "helper_boot_mismatch"
    }
    guard dependencies.isPIDLive(request.pid) else {
        return "process_not_live"
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return "process_identity_mismatch"
    }

    let window: CaptureCoordinateWindowWindowSnapshot
    do {
        guard let observed = try dependencies.exactWindow(request.windowID) else {
            return "window_not_found"
        }
        window = observed
    } catch {
        return "window_not_found"
    }
    guard window.windowID == request.windowID, window.ownerPID == request.pid else {
        return "window_identity_mismatch"
    }
    guard window.layer == 0, window.isOnScreen else {
        return "window_not_actionable"
    }
    guard coordinateMouseRectsEqual(window.bounds, request.expectedWindowQuartzBounds) else {
        return "window_bounds_mismatch"
    }

    guard let requestedDisplay = topology.displays.first(where: {
        $0.displayID == request.displayID
    }) else {
        return "display_not_found"
    }
    guard requestedDisplay.isActive,
          requestedDisplay.isOnline,
          !requestedDisplay.isAsleep,
          requestedDisplay.mirrorMasterDisplayID == nil,
          requestedDisplay.rotationDegrees == 0 else {
        return "display_not_actionable"
    }
    let containingDisplays = topology.displays.filter {
        $0.isActive && $0.isOnline && !$0.isAsleep &&
            $0.mirrorMasterDisplayID == nil && $0.rotationDegrees == 0 &&
            coordinateMouseRect($0.quartzBounds, fullyContains: window.bounds)
    }
    guard containingDisplays.count == 1,
          containingDisplays[0].displayID == request.displayID else {
        return "display_not_actionable"
    }
    guard coordinateMouseRect(window.bounds, contains: request.quartzPoint) else {
        return "point_outside_window"
    }
    guard coordinateMouseRect(requestedDisplay.quartzBounds, contains: request.quartzPoint) else {
        return "point_outside_display"
    }
    guard dependencies.frontmostWindowAtPoint(request.quartzPoint) == request.windowID else {
        return "point_occluded"
    }
    return nil
}

func runCoordinateMouseEvent(
    request: CoordinateMouseEventRequestV1,
    dependencies: CoordinateMouseEventDependencies
) -> CoordinateMouseEventResultV1 {
    guard let deadline = coordinateMouseEventDate(request.commitDeadlineAt) else {
        return coordinateMouseFailure(request: request, code: "invalid_request")
    }
    let startedAt = dependencies.now()
    let horizon = deadline.timeIntervalSince(startedAt)
    guard horizon > 0 else {
        return coordinateMouseFailure(request: request, code: "request_expired")
    }
    guard horizon <= coordinateMouseMaximumDeadlineHorizonV1 else {
        return coordinateMouseFailure(request: request, code: "invalid_request")
    }
    if let failure = coordinateMouseAuthorityFailure(request: request, dependencies: dependencies) {
        return coordinateMouseFailure(request: request, code: failure)
    }

    var preparedClick: CoordinateMousePreparedClick?
    if request.action == "click" {
        guard let button = request.button, let clickCount = request.clickCount,
              let prepared = dependencies.prepareClick(request.quartzPoint, button, clickCount) else {
            return coordinateMouseFailure(
                request: request,
                code: "event_preparation_failed",
                phase: "preparation")
        }
        preparedClick = prepared
    }

    guard dependencies.now() < deadline else {
        return coordinateMouseFailure(request: request, code: "request_expired")
    }
    // Click preparation is deliberately side-effect free, but it may take long
    // enough for another application window or a same-app sheet to cover the
    // target. Revalidate immediately before the first pointer side effect.
    if request.action == "click",
       let failure = coordinateMouseAuthorityFailure(request: request, dependencies: dependencies) {
        return coordinateMouseFailure(request: request, code: failure)
    }

    guard let physicalInputBeforeMove = dependencies.observePhysicalInput() else {
        return coordinateMouseFailure(
            request: request,
            code: "interference_detection_unavailable")
    }
    if assessPhysicalInputInterferenceV1(
        baseline: physicalInputBeforeMove,
        current: physicalInputBeforeMove,
        expectedPointer: nil,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue
    ) == .interference {
        return coordinateMouseFailure(
            request: request,
            code: "physical_input_interference",
            phase: "user_interference",
            status: "user_interference")
    }

    let pointerOutcome = dependencies.movePointer(request.quartzPoint)
    let expectedPointerMoveEvents: [(CGEventType, UInt32)]
    switch pointerOutcome {
    case .notCommitted:
        return coordinateMouseFailure(
            request: request,
            code: "pointer_move_failed",
            phase: "pointer_move")
    case let .committed(_, expectedSyntheticEventCount):
        expectedPointerMoveEvents = expectedSyntheticEventCount == 0
            ? []
            : [(.mouseMoved, expectedSyntheticEventCount)]
    }
    let afterMoveAssessment = coordinateMouseAssessPhysicalInputV1(
        baseline: physicalInputBeforeMove,
        expectedPointer: request.quartzPoint,
        expectedSyntheticEvents: expectedPointerMoveEvents,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue,
        observe: dependencies.observePhysicalInput,
        settle: {
            dependencies.sleep(coordinateMouseCounterSettleDelayV1)
        }
    )
    let physicalInputAfterMove = afterMoveAssessment.snapshot
    switch afterMoveAssessment.assessment {
    case .interference:
        return coordinateMouseFailure(
            request: request,
            code: "physical_input_interference",
            phase: "user_interference",
            primaryCommitted: request.action == "move",
            pointerCommitted: true,
            endpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: physicalInputAfterMove?.pointer),
            status: "user_interference")
    case .unavailable:
        return coordinateMouseFailure(
            request: request,
            code: "interference_detection_unavailable",
            phase: "post_verification",
            primaryCommitted: request.action == "move",
            pointerCommitted: true,
            endpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: physicalInputAfterMove?.pointer),
            status: "completed_unverified")
    case .unchanged:
        break
    }
    // Cursor delivery is asynchronous. The move helper's immediate sample can
    // precede the HID counter that the monitor above waited for, so bind the
    // action gate to a fresh cursor observation after that settle instead.
    let preActionEndpoint = coordinateMouseEndpoint(
        requested: request.quartzPoint,
        observed: dependencies.observePointer())
    guard preActionEndpoint.verified else {
        if request.action == "move" {
            return coordinateMouseFailure(
                request: request,
                code: "pointer_endpoint_not_verified",
                phase: "post_verification",
                primaryCommitted: true,
                pointerCommitted: true,
                endpoint: preActionEndpoint,
                status: "completed_unverified")
        }
        return coordinateMouseFailure(
            request: request,
            code: "pointer_endpoint_not_verified",
            phase: "pointer_move",
            pointerCommitted: true,
            endpoint: preActionEndpoint)
    }

    dependencies.sleep(coordinateMouseInterferenceSettleDelayV1)
    let physicalInputAfterSettle = dependencies.observePhysicalInput()
    switch assessPhysicalInputInterferenceV1(
        baseline: physicalInputAfterMove,
        current: physicalInputAfterSettle,
        expectedPointer: request.quartzPoint,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue
    ) {
    case .interference:
        return coordinateMouseFailure(
            request: request,
            code: "physical_input_interference",
            phase: "user_interference",
            primaryCommitted: request.action == "move",
            pointerCommitted: true,
            endpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: physicalInputAfterSettle?.pointer),
            status: "user_interference")
    case .unavailable:
        return coordinateMouseFailure(
            request: request,
            code: "interference_detection_unavailable",
            phase: "post_verification",
            primaryCommitted: request.action == "move",
            pointerCommitted: true,
            endpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: physicalInputAfterSettle?.pointer),
            status: "completed_unverified")
    case .unchanged:
        break
    }

    if request.action == "move" {
        return CoordinateMouseEventResultV1(
            status: "completed",
            action: "move",
            primaryActionCommitted: true,
            pointerMotionCommitted: true,
            phase: "post_verification",
            failureCode: nil,
            pointerEndpoint: preActionEndpoint)
    }

    if coordinateMouseAuthorityFailure(request: request, dependencies: dependencies) != nil {
        return coordinateMouseFailure(
            request: request,
            code: "target_changed_before_click",
            phase: "action",
            pointerCommitted: true,
            endpoint: preActionEndpoint)
    }
    guard dependencies.now() < deadline else {
        return coordinateMouseFailure(
            request: request,
            code: "request_expired_before_click",
            phase: "action",
            pointerCommitted: true,
            endpoint: preActionEndpoint)
    }

    var physicalInputBeforeClick = dependencies.observePhysicalInput()
    switch assessPhysicalInputInterferenceV1(
        baseline: physicalInputAfterSettle,
        current: physicalInputBeforeClick,
        expectedPointer: request.quartzPoint,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue
    ) {
    case .interference:
        return coordinateMouseFailure(
            request: request,
            code: "physical_input_interference",
            phase: "user_interference",
            pointerCommitted: true,
            endpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: physicalInputBeforeClick?.pointer),
            status: "user_interference")
    case .unavailable:
        return coordinateMouseFailure(
            request: request,
            code: "interference_detection_unavailable",
            phase: "action",
            pointerCommitted: true,
            endpoint: preActionEndpoint)
    case .unchanged:
        break
    }

    if let assertion = request.riskAssertion {
        let verificationTimeout = min(
            targetedAXPostconditionBudgetV1.maxSynchronousCallDuration,
            max(0, deadline.timeIntervalSince(dependencies.now())))
        guard verificationTimeout > 0 else {
            return coordinateMouseFailure(
                request: request,
                code: "request_expired_before_click",
                phase: "action",
                pointerCommitted: true,
                endpoint: preActionEndpoint)
        }
        switch dependencies.verifyRiskTarget(request, assertion, verificationTimeout) {
        case .matched:
            break
        case let .drift(code):
            return coordinateMouseFailure(
                request: request,
                code: code,
                phase: "action",
                pointerCommitted: true,
                endpoint: preActionEndpoint)
        case let .unavailable(code):
            return coordinateMouseFailure(
                request: request,
                code: code,
                phase: "action",
                pointerCommitted: true,
                endpoint: preActionEndpoint)
        }
        guard dependencies.now() < deadline else {
            return coordinateMouseFailure(
                request: request,
                code: "request_expired_before_click",
                phase: "action",
                pointerCommitted: true,
                endpoint: preActionEndpoint)
        }
        let physicalInputAfterRiskVerification = dependencies.observePhysicalInput()
        switch assessPhysicalInputInterferenceV1(
            baseline: physicalInputBeforeClick,
            current: physicalInputAfterRiskVerification,
            expectedPointer: request.quartzPoint,
            expectedSyntheticHeldModifierFlags:
                strictInputModifierFlagsV1(request.modifiers)!.rawValue
        ) {
        case .interference:
            return coordinateMouseFailure(
                request: request,
                code: "physical_input_interference",
                phase: "user_interference",
                pointerCommitted: true,
                endpoint: coordinateMouseEndpoint(
                    requested: request.quartzPoint,
                    observed: physicalInputAfterRiskVerification?.pointer),
                status: "user_interference")
        case .unavailable:
            return coordinateMouseFailure(
                request: request,
                code: "interference_detection_unavailable",
                phase: "action",
                pointerCommitted: true,
                endpoint: preActionEndpoint)
        case .unchanged:
            physicalInputBeforeClick = physicalInputAfterRiskVerification
        }
    }

    let postOutcome = preparedClick!.post()
    let inputCommitted: Bool
    let expectedClickEvents: [(CGEventType, UInt32)]?
    switch postOutcome {
    case .notCommitted:
        inputCommitted = false
        expectedClickEvents = []
    case .committed, .partiallyCommitted:
        inputCommitted = true
        if postOutcome == .committed,
           let mapping = coordinateMouseButtonMappingV1(request.button!) {
            expectedClickEvents = [
                (mapping.downType, UInt32(request.clickCount!)),
                (mapping.upType, UInt32(request.clickCount!)),
            ]
        } else {
            expectedClickEvents = nil
        }
    }
    if postOutcome == .partiallyCommitted {
        return CoordinateMouseEventResultV1(
            status: "completed_unverified",
            action: "click",
            primaryActionCommitted: true,
            pointerMotionCommitted: true,
            phase: "post_verification",
            failureCode: "input_sequence_interrupted_after_commit",
            pointerEndpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: dependencies.observePointer()))
    }
    let afterClickAssessment = coordinateMouseAssessPhysicalInputV1(
        baseline: physicalInputBeforeClick,
        expectedPointer: request.quartzPoint,
        expectedSyntheticEvents: expectedClickEvents ?? [],
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue,
        observe: dependencies.observePhysicalInput,
        settle: {
            dependencies.sleep(coordinateMouseCounterSettleDelayV1)
        },
        maximumSettleAttempts: expectedClickEvents == nil ? 0 : 10
    )
    let physicalInputAfterClick = afterClickAssessment.snapshot
    let clickInterference = expectedClickEvents == nil
        ? PhysicalInputInterferenceAssessmentV1.unavailable
        : afterClickAssessment.assessment
    if clickInterference == .interference {
        return coordinateMouseFailure(
            request: request,
            code: "physical_input_interference",
            phase: "user_interference",
            primaryCommitted: inputCommitted,
            pointerCommitted: true,
            endpoint: coordinateMouseEndpoint(
                requested: request.quartzPoint,
                observed: physicalInputAfterClick?.pointer),
            status: "user_interference")
    }
    if clickInterference == .unavailable, inputCommitted {
        return coordinateMouseFailure(
            request: request,
            code: "interference_detection_unavailable",
            phase: "post_verification",
            primaryCommitted: true,
            pointerCommitted: true,
            endpoint: preActionEndpoint,
            status: "completed_unverified")
    }

    switch postOutcome {
    case .notCommitted:
        return coordinateMouseFailure(
            request: request,
            code: "input_commit_blocked",
            phase: "action",
            pointerCommitted: true,
            endpoint: preActionEndpoint)
    case .partiallyCommitted:
        return CoordinateMouseEventResultV1(
            status: "completed_unverified",
            action: "click",
            primaryActionCommitted: true,
            pointerMotionCommitted: true,
            phase: "post_verification",
            failureCode: "input_sequence_interrupted_after_commit",
            pointerEndpoint: preActionEndpoint)
    case .committed:
        break
    }
    let finalEndpoint = coordinateMouseEndpoint(
        requested: request.quartzPoint,
        observed: dependencies.observePointer())
    return CoordinateMouseEventResultV1(
        status: "completed_unverified",
        action: "click",
        primaryActionCommitted: true,
        pointerMotionCommitted: true,
        phase: "post_verification",
        failureCode: finalEndpoint.verified
            ? "click_postcondition_not_declared"
            : "pointer_endpoint_not_verified_after_commit",
        pointerEndpoint: finalEndpoint)
}

private func coordinateMouseEventDate(_ value: String) -> Date? {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: value) {
        return date
    }
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.date(from: value)
}

private func productionCoordinateMouseWindow(
    _ windowID: UInt32
) throws -> CaptureCoordinateWindowWindowSnapshot? {
    guard let raw = CGWindowListCopyWindowInfo(
        [.optionIncludingWindow],
        CGWindowID(windowID)) as? [[String: Any]] else {
        return nil
    }
    let exact = raw.filter {
        ($0[kCGWindowNumber as String] as? NSNumber)?.uint32Value == windowID
    }
    guard exact.count == 1,
          let info = exact.first,
          let ownerPID = info[kCGWindowOwnerPID as String] as? NSNumber,
          let layer = info[kCGWindowLayer as String] as? NSNumber,
          let onScreen = info[kCGWindowIsOnscreen as String] as? NSNumber,
          let rawBounds = info[kCGWindowBounds as String],
          CFGetTypeID(rawBounds as CFTypeRef) == CFDictionaryGetTypeID(),
          let bounds = CGRect(dictionaryRepresentation: rawBounds as! CFDictionary) else {
        return nil
    }
    return CaptureCoordinateWindowWindowSnapshot(
        windowID: windowID,
        ownerPID: ownerPID.intValue,
        layer: layer.intValue,
        isOnScreen: onScreen.boolValue,
        bounds: DisplayTopologyRectV1(
            x: Double(bounds.origin.x),
            y: Double(bounds.origin.y),
            width: Double(bounds.width),
            height: Double(bounds.height)))
}

private func productionCoordinateMousePreparedClick(
    point: CoordinateMouseEventPointV1,
    button: String,
    clicks: Int
) -> CoordinateMousePreparedClick? {
    guard let syntheticSource = physicalInputSyntheticEventSourceV1,
          let mapping = coordinateMouseButtonMappingV1(button) else { return nil }
    let cgButton = mapping.button
    let downType = mapping.downType
    let upType = mapping.upType
    let location = CGPoint(x: point.x, y: point.y)
    var events: [(down: CGEvent, release: PreparedInputReleaseV1)] = []
    events.reserveCapacity(clicks)
    for index in 0..<clicks {
        guard let down = CGEvent(
            mouseEventSource: syntheticSource,
            mouseType: downType,
            mouseCursorPosition: location,
            mouseButton: cgButton),
              let up = CGEvent(
                mouseEventSource: syntheticSource,
                mouseType: upType,
                mouseCursorPosition: location,
                mouseButton: cgButton) else {
            return nil
        }
        if clicks > 1 {
            let state = Int64(index + 1)
            down.setIntegerValueField(.mouseEventClickState, value: state)
            up.setIntegerValueField(.mouseEventClickState, value: state)
        }
        let release = PreparedInputReleaseV1(metadata: .mouse(button: button)) {
            up.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.005)
            return !CGEventSource.buttonState(.combinedSessionState, button: cgButton)
        }
        events.append((down, release))
    }
    return CoordinateMousePreparedClick {
        var completed = 0
        for pair in events {
            guard let token = processInputCommitGateV1.registerPress(
                release: pair.release,
                commitDown: {
                    pair.down.post(tap: .cghidEventTap)
                    return true
                }) else {
                return completed == 0 ? .notCommitted : .partiallyCommitted
            }
            guard processInputCommitGateV1.confirmRelease(token: token) else {
                return .partiallyCommitted
            }
            completed += 1
        }
        return .committed
    }
}

private func productionCoordinateMouseFrontmostWindowAtPoint(
    _ point: CoordinateMouseEventPointV1
) -> UInt32? {
    coordinateFrontmostNormalWindowID(
        at: CGPoint(x: point.x, y: point.y))
}

private func productionCoordinateMouseMove(
    _ point: CoordinateMouseEventPointV1
) -> CoordinateMousePointerMoveOutcome {
    guard let pointerEvent = CGEvent(source: nil) else {
        return .notCommitted
    }
    let location = CGPoint(x: point.x, y: point.y)
    let current = pointerEvent.location
    if current == location {
        return .committed(
            observed: CoordinateMouseEventPointV1(
                x: Double(current.x),
                y: Double(current.y)
            ),
            expectedSyntheticEventCount: 0
        )
    }
    guard let syntheticSource = physicalInputSyntheticEventSourceV1 else {
        return .notCommitted
    }
    guard let move = CGEvent(
        mouseEventSource: syntheticSource,
        mouseType: .mouseMoved,
        mouseCursorPosition: location,
        mouseButton: .left) else {
        return .notCommitted
    }
    var outcome: CoordinateMousePointerMoveOutcome = .notCommitted
    guard processInputCommitGateV1.commitSample({
        // Use one attributable private-source event. CGWarp has no source
        // identity, so combining it with this post would make an aggregate HID
        // counter delta indistinguishable from competing physical input.
        move.post(tap: .cghidEventTap)
        let observed = CGEvent(source: nil)?.location
        outcome = .committed(
            observed: observed.map {
                CoordinateMouseEventPointV1(
                    x: Double($0.x),
                    y: Double($0.y)
                )
            },
            expectedSyntheticEventCount: 1
        )
        return true
    }) else { return .notCommitted }
    return outcome
}

private enum CoordinateMouseLiveRiskTargetV1 {
    case matched(AXUIElement)
    case drift(String)
    case unavailable
}

private enum CoordinateMouseRiskAXReadV1<Value> {
    case value(Value)
    case unavailable
}

private func coordinateMouseRiskAXReadV1<Value>(
    element: AXUIElement,
    remaining: () -> TimeInterval,
    operation: () -> Value
) -> CoordinateMouseRiskAXReadV1<Value> {
    let timeout = remaining()
    guard timeout >= 0.001 else { return .unavailable }
    switch withTargetedAXMessagingTimeoutV1(
        element: element,
        timeout: timeout,
        operation: operation
    ) {
    case let .completed(value): return .value(value)
    case .unavailable: return .unavailable
    }
}

private func coordinateMouseRiskResolvePathV1(
    window: AXUIElement,
    path: String,
    remaining: () -> TimeInterval
) -> CoordinateMouseLiveRiskTargetV1 {
    let parts = path.split(separator: "/")
    guard parts.first == "window[0]" else {
        return .drift("risk_hit_target_drift")
    }
    var current = window
    for part in parts.dropFirst() {
        guard let bracketStart = part.firstIndex(of: "["),
              let bracketEnd = part.firstIndex(of: "]"),
              bracketEnd == part.index(before: part.endIndex),
              let index = Int(part[part.index(after: bracketStart)..<bracketEnd]),
              index >= 0 else {
            return .drift("risk_hit_target_drift")
        }
        let expectedRole = String(part[part.startIndex..<bracketStart])
        let children: [AXUIElement]
        switch coordinateMouseRiskAXReadV1(
            element: current,
            remaining: remaining,
            operation: { axChildren(current) ?? [] }
        ) {
        case let .value(value): children = value
        case .unavailable: return .unavailable
        }
        var matching: [AXUIElement] = []
        for child in children {
            let role: String?
            switch coordinateMouseRiskAXReadV1(
                element: child,
                remaining: remaining,
                operation: { axString(child, "AXRole") }
            ) {
            case let .value(value): role = value
            case .unavailable: return .unavailable
            }
            if role == expectedRole { matching.append(child) }
        }
        guard index < matching.count else {
            return .drift("risk_hit_target_drift")
        }
        current = matching[index]
    }
    return .matched(current)
}

private func productionCoordinateMouseRiskTarget(
    request: CoordinateMouseEventRequestV1,
    assertion: CoordinateMouseRiskAssertionV1,
    timeout: TimeInterval
) -> CoordinateMouseRiskVerificationOutcomeV1 {
    guard timeout > 0, timeout.isFinite else {
        return .unavailable("risk_hit_target_unavailable")
    }
    let startedAt = Date()
    let remaining: () -> TimeInterval = {
        max(0, timeout - Date().timeIntervalSince(startedAt))
    }
    guard assertion.coordinateAuthority.quartzPoint == request.quartzPoint,
          assertion.coordinateAuthority.topologyRef == request.topologyRef,
          assertion.coordinateAuthority.helperBootID == request.helperBootID,
          assertion.coordinateAuthority.displayID == request.displayID,
          let frameExpiry = strictMutationDate(assertion.coordinateAuthority.frameExpiresAt),
          Date() < frameExpiry else {
        return .drift("risk_hit_target_drift")
    }

    let candidates = currentCGWindowIdentityCandidates()
    guard candidates.contains(where: {
        $0.windowID == Int(request.windowID) && $0.ownerPID == request.pid && $0.layer == 0
    }) else { return .drift("risk_destination_drift") }
    let app = AXUIElementCreateApplication(Int32(request.pid))
    let windows: [AXUIElement]
    switch coordinateMouseRiskAXReadV1(
        element: app,
        remaining: remaining,
        operation: { axWindows(app) }
    ) {
    case let .value(value): windows = value
    case .unavailable: return .unavailable("risk_hit_target_unavailable")
    }
    var exactWindows: [(element: AXUIElement, title: String)] = []
    var ambiguous = false
    for window in windows {
        let attributes: ((x: Double, y: Double, width: Double, height: Double)?, String?)
        switch coordinateMouseRiskAXReadV1(
            element: window,
            remaining: remaining,
            operation: { (elementFrame(window), axString(window, "AXTitle")) }
        ) {
        case let .value(value): attributes = value
        case .unavailable: return .unavailable("risk_hit_target_unavailable")
        }
        guard let frame = attributes.0 else { continue }
        let title = attributes.1 ?? ""
        let matches = matchingWindowCandidates(
            .init(
                pid: request.pid, title: title,
                frame: .init(
                    x: frame.x, y: frame.y,
                    width: frame.width, height: frame.height)),
            candidates: candidates)
        if matches.count > 1 && matches.contains(where: {
            $0.windowID == Int(request.windowID)
        }) {
            ambiguous = true
        } else if matches.count == 1, matches[0].windowID == Int(request.windowID) {
            exactWindows.append((window, title))
        }
    }
    guard !ambiguous, exactWindows.count == 1, let exactWindow = exactWindows.first,
          exactWindow.title == assertion.destinationAssertion.expectedWindowTitle else {
        return .drift("risk_destination_drift")
    }
    let liveTarget = coordinateMouseRiskResolvePathV1(
        window: exactWindow.element,
        path: assertion.coordinateAuthority.elementPath,
        remaining: remaining)
    let target: AXUIElement
    switch liveTarget {
    case let .matched(value): target = value
    case let .drift(code): return .drift(code)
    case .unavailable: return .unavailable("risk_hit_target_unavailable")
    }
    let targetMatches: Bool
    switch coordinateMouseRiskAXReadV1(
        element: target,
        remaining: remaining,
        operation: { () -> Bool in
            guard let role = axString(target, "AXRole"),
                  role == assertion.expectedRole,
                  semanticPressFingerprintV2(target) == assertion.expectedFingerprint,
                  axBool(target, "AXEnabled") == true,
                  axActions(target).contains("AXPress"),
                  let frame = elementFrame(target),
                  request.quartzPoint.x >= frame.x,
                  request.quartzPoint.y >= frame.y,
                  request.quartzPoint.x < frame.x + frame.width,
                  request.quartzPoint.y < frame.y + frame.height,
                  !isSensitiveAXValue(axValueSensitivityMetadata(target, role: role)) else {
                return false
            }
            return true
        }
    ) {
    case let .value(value): targetMatches = value
    case .unavailable: return .unavailable("risk_hit_target_unavailable")
    }
    guard targetMatches else { return .drift("risk_hit_target_drift") }
    if remaining() <= 0 {
        return .unavailable("risk_hit_target_unavailable")
    }

    let systemWide = AXUIElementCreateSystemWide()
    let hit: AXUIElement?
    switch coordinateMouseRiskAXReadV1(
        element: systemWide,
        remaining: remaining,
        operation: { () -> AXUIElement? in
            var value: AXUIElement?
            guard AXUIElementCopyElementAtPosition(
                systemWide,
                Float(request.quartzPoint.x),
                Float(request.quartzPoint.y),
                &value) == .success else { return nil }
            return value
        }
    ) {
    case let .value(value): hit = value
    case .unavailable: return .unavailable("risk_hit_target_unavailable")
    }
    guard var current = hit else { return .drift("risk_hit_target_drift") }
    for _ in 0..<32 {
        if axElementsEqual(current, target) { return .matched }
        let parent: AXUIElement?
        switch coordinateMouseRiskAXReadV1(
            element: current,
            remaining: remaining,
            operation: { () -> AXUIElement? in
                guard let value = axValue(current, "AXParent"),
                      CFGetTypeID(value) == AXUIElementGetTypeID() else { return nil }
                return (value as! AXUIElement)
            }
        ) {
        case let .value(value): parent = value
        case .unavailable: return .unavailable("risk_hit_target_unavailable")
        }
        guard let parent else { return .drift("risk_hit_target_drift") }
        current = parent
    }
    return .drift("risk_hit_target_drift")
}

let productionCoordinateMouseEventDependencies = CoordinateMouseEventDependencies(
    observeTopology: { try liveDisplayTopologyService.observe() },
    isPIDLive: { pid in
        refreshAppKitState()
        guard let processID = pid_t(exactly: pid),
              let app = NSRunningApplication(processIdentifier: processID) else {
            return false
        }
        return !app.isTerminated
    },
    bundleIDForPID: { pid in
        guard let processID = pid_t(exactly: pid) else { return nil }
        return NSRunningApplication(processIdentifier: processID)?.bundleIdentifier
    },
    exactWindow: productionCoordinateMouseWindow,
    frontmostWindowAtPoint: productionCoordinateMouseFrontmostWindowAtPoint,
    prepareClick: { point, button, clicks in
        productionCoordinateMousePreparedClick(point: point, button: button, clicks: clicks)
    },
    movePointer: productionCoordinateMouseMove,
    observePointer: {
        guard let event = CGEvent(source: nil) else { return nil }
        let location = event.location
        return CoordinateMouseEventPointV1(
            x: Double(location.x),
            y: Double(location.y))
    },
    observePhysicalInput: observePhysicalInputInterferenceV1,
    verifyRiskTarget: productionCoordinateMouseRiskTarget,
    sleep: Thread.sleep(forTimeInterval:),
    now: Date.init)
