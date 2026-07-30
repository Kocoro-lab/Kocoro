import CryptoKit
import Foundation

struct CaptureCoordinateDisplayRequestV1: Equatable {
    let schemaVersion: Int
    let topologyRef: CaptureCoordinateWindowTopologyRefV1
    let displayID: UInt32

    init(
        schemaVersion: Int,
        topologyRef: CaptureCoordinateWindowTopologyRefV1,
        displayID: UInt32
    ) {
        self.schemaVersion = schemaVersion
        self.topologyRef = topologyRef
        self.displayID = displayID
    }

    func validate() throws {
        guard schemaVersion == 1,
              strictMutationIdentity(topologyRef.topologyID),
              topologyRef.generation > 0,
              displayID > 0 else {
            throw CaptureCoordinateDisplayLiveError.invalidRequest
        }
    }
}

private struct CaptureCoordinateDisplayStrictParamsV1: Decodable {
    let request: CaptureCoordinateDisplayRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container,
            exactly: ["schema_version", "topology_ref", "display_id"],
            field: "capture_coordinate_display params")
        let schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        let displayID = try container.decode(UInt32.self, forKey: strictMutationKey("display_id"))
        let refContainer = try container.nestedContainer(
            keyedBy: StrictMutationCodingKey.self,
            forKey: strictMutationKey("topology_ref"))
        try requireStrictMutationKeys(
            refContainer,
            exactly: ["topology_id", "generation"],
            field: "capture_coordinate_display topology_ref")
        let topologyRef = CaptureCoordinateWindowTopologyRefV1(
            topologyID: try refContainer.decode(
                String.self, forKey: strictMutationKey("topology_id")),
            generation: try refContainer.decode(
                UInt64.self, forKey: strictMutationKey("generation")))
        request = CaptureCoordinateDisplayRequestV1(
            schemaVersion: schemaVersion,
            topologyRef: topologyRef,
            displayID: displayID)
        try request.validate()
    }
}

struct CaptureCoordinateDisplayRPCRequestV1: Equatable {
    let id: Int64
    let method: String
    let params: CaptureCoordinateDisplayRequestV1
}

private struct CaptureCoordinateDisplayStrictEnvelopeV1: Decodable {
    let value: CaptureCoordinateDisplayRPCRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container,
            exactly: ["id", "method", "params"],
            field: "capture_coordinate_display request")
        let id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        let method = try container.decode(String.self, forKey: strictMutationKey("method"))
        let params = try container.decode(
            CaptureCoordinateDisplayStrictParamsV1.self,
            forKey: strictMutationKey("params"))
        guard id > 0, method == "capture_coordinate_display" else {
            throw CaptureCoordinateDisplayLiveError.invalidRequest
        }
        value = CaptureCoordinateDisplayRPCRequestV1(
            id: id, method: method, params: params.request)
    }
}

func decodeCaptureCoordinateDisplayRPCRequestV1(
    _ payload: Data
) throws -> CaptureCoordinateDisplayRPCRequestV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(
        CaptureCoordinateDisplayStrictEnvelopeV1.self,
        from: payload).value
}

enum CaptureCoordinateDisplayLiveError: Error, Equatable {
    case invalidRequest
    case timeout
    case tooLarge
    case failed
}

struct CaptureCoordinateDisplayDependencies {
    let observeTopology: () throws -> DisplayTopologyV1
    let capturePNG: (DisplayTopologyDisplayV1, TimeInterval, Int) throws -> Data
    let rawByteCap: Int
    let ndjsonByteCap: Int
    let captureTimeout: TimeInterval
}

struct CaptureCoordinateDisplayResultV1: Encodable {
    let schemaVersion: Int
    let status: String
    let failureCode: String?
    let retrySafe: Bool
    let topologyRef: CaptureCoordinateWindowTopologyRefV1?
    let helperBootID: String?
    let displayID: UInt32?
    let displayQuartzBounds: DisplayTopologyRectV1?
    let backingScaleFactor: Double?
    let mediaType: String?
    let widthPX: Int?
    let heightPX: Int?
    let byteLength: Int?
    let sha256: String?
    let imageBase64: String?
    let capturedAt: String?

    enum CodingKeys: String, CodingKey {
        case status
        case schemaVersion = "schema_version"
        case failureCode = "failure_code"
        case retrySafe = "retry_safe"
        case topologyRef = "topology_ref"
        case helperBootID = "helper_boot_id"
        case displayID = "display_id"
        case displayQuartzBounds = "display_quartz_bounds"
        case backingScaleFactor = "backing_scale_factor"
        case mediaType = "media_type"
        case widthPX = "width_px"
        case heightPX = "height_px"
        case byteLength = "byte_length"
        case sha256
        case imageBase64 = "image_base64"
        case capturedAt = "captured_at"
    }

    static func captured(
        topologyRef: CaptureCoordinateWindowTopologyRefV1,
        helperBootID: String,
        displayID: UInt32,
        displayQuartzBounds: DisplayTopologyRectV1,
        backingScaleFactor: Double,
        png: Data,
        widthPX: Int,
        heightPX: Int,
        capturedAt: String
    ) -> Self {
        Self(
            schemaVersion: 1,
            status: "captured",
            failureCode: nil,
            retrySafe: false,
            topologyRef: topologyRef,
            helperBootID: helperBootID,
            displayID: displayID,
            displayQuartzBounds: displayQuartzBounds,
            backingScaleFactor: backingScaleFactor,
            mediaType: "image/png",
            widthPX: widthPX,
            heightPX: heightPX,
            byteLength: png.count,
            sha256: SHA256.hash(data: png).map { String(format: "%02x", $0) }.joined(),
            imageBase64: png.base64EncodedString(),
            capturedAt: capturedAt)
    }

    static func failed(code: String, retrySafe: Bool) -> Self {
        Self(
            schemaVersion: 1,
            status: "failed",
            failureCode: code,
            retrySafe: retrySafe,
            topologyRef: nil,
            helperBootID: nil,
            displayID: nil,
            displayQuartzBounds: nil,
            backingScaleFactor: nil,
            mediaType: nil,
            widthPX: nil,
            heightPX: nil,
            byteLength: nil,
            sha256: nil,
            imageBase64: nil,
            capturedAt: nil)
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(schemaVersion, forKey: .schemaVersion)
        try container.encode(status, forKey: .status)
        try encodeNullable(failureCode, into: &container, key: .failureCode)
        try container.encode(retrySafe, forKey: .retrySafe)
        try encodeNullable(topologyRef, into: &container, key: .topologyRef)
        try encodeNullable(helperBootID, into: &container, key: .helperBootID)
        try encodeNullable(displayID, into: &container, key: .displayID)
        try encodeNullable(displayQuartzBounds, into: &container, key: .displayQuartzBounds)
        try encodeNullable(backingScaleFactor, into: &container, key: .backingScaleFactor)
        try encodeNullable(mediaType, into: &container, key: .mediaType)
        try encodeNullable(widthPX, into: &container, key: .widthPX)
        try encodeNullable(heightPX, into: &container, key: .heightPX)
        try encodeNullable(byteLength, into: &container, key: .byteLength)
        try encodeNullable(sha256, into: &container, key: .sha256)
        try encodeNullable(imageBase64, into: &container, key: .imageBase64)
        try encodeNullable(capturedAt, into: &container, key: .capturedAt)
    }

    private func encodeNullable<T: Encodable>(
        _ value: T?,
        into container: inout KeyedEncodingContainer<CodingKeys>,
        key: CodingKeys
    ) throws {
        if let value {
            try container.encode(value, forKey: key)
        } else {
            try container.encodeNil(forKey: key)
        }
    }
}

private struct CaptureCoordinateDisplayWireEnvelope: Encodable {
    let id: Int64
    let result: CaptureCoordinateDisplayResultV1
}

func captureCoordinateDisplay(
    request: CaptureCoordinateDisplayRequestV1,
    dependencies: CaptureCoordinateDisplayDependencies
) -> CaptureCoordinateDisplayResultV1 {
    do {
        try request.validate()
    } catch {
        return .failed(code: "invalid_request", retrySafe: false)
    }
    guard dependencies.rawByteCap > 0,
          dependencies.ndjsonByteCap > 0,
          dependencies.captureTimeout > 0,
          dependencies.captureTimeout <= 8 else {
        return .failed(code: "invalid_request", retrySafe: false)
    }

    let preTopology: DisplayTopologyV1
    do {
        preTopology = try dependencies.observeTopology()
        try preTopology.validate()
    } catch {
        return .failed(code: "topology_unavailable", retrySafe: true)
    }
    let preRef = CaptureCoordinateWindowTopologyRefV1(
        topologyID: preTopology.topologyID,
        generation: preTopology.generation)
    guard preRef == request.topologyRef else {
        return .failed(code: "stale_topology", retrySafe: true)
    }
    guard let display = preTopology.displays.first(where: { $0.displayID == request.displayID }) else {
        return .failed(code: "display_not_found", retrySafe: true)
    }
    guard display.isActive,
          display.isOnline,
          !display.isAsleep,
          display.mirrorMasterDisplayID == nil,
          display.rotationDegrees == 0 else {
        return .failed(code: "display_not_actionable", retrySafe: true)
    }

    let png: Data
    do {
        png = try dependencies.capturePNG(
            display, dependencies.captureTimeout, dependencies.rawByteCap)
    } catch CaptureCoordinateDisplayLiveError.timeout {
        return .failed(code: "capture_timeout", retrySafe: true)
    } catch CaptureCoordinateDisplayLiveError.tooLarge {
        return .failed(code: "image_too_large", retrySafe: false)
    } catch {
        return .failed(code: "capture_failed", retrySafe: true)
    }
    guard png.count <= dependencies.rawByteCap else {
        return .failed(code: "image_too_large", retrySafe: false)
    }

    let postTopology: DisplayTopologyV1
    do {
        postTopology = try dependencies.observeTopology()
        try postTopology.validate()
    } catch {
        return .failed(code: "topology_unavailable", retrySafe: true)
    }
    let postRef = CaptureCoordinateWindowTopologyRefV1(
        topologyID: postTopology.topologyID,
        generation: postTopology.generation)
    guard postRef == request.topologyRef,
          postTopology.helperBootID == preTopology.helperBootID,
          postTopology.mainDisplayID == preTopology.mainDisplayID,
          postTopology.displays == preTopology.displays else {
        return .failed(code: "topology_changed", retrySafe: true)
    }

    let dimensions: (width: Int, height: Int)
    switch decodeCoordinateWindowPNG(
        png,
        expectedWidth: display.pixelWidth,
        expectedHeight: display.pixelHeight) {
    case let .valid(width, height):
        dimensions = (width, height)
    case .dimensionMismatch:
        return .failed(code: "image_dimensions_mismatch", retrySafe: false)
    case .invalid:
        return .failed(code: "invalid_png", retrySafe: false)
    }
    let result = CaptureCoordinateDisplayResultV1.captured(
        topologyRef: postRef,
        helperBootID: postTopology.helperBootID,
        displayID: display.displayID,
        displayQuartzBounds: display.quartzBounds,
        backingScaleFactor: display.backingScaleFactor,
        png: png,
        widthPX: dimensions.width,
        heightPX: dimensions.height,
        capturedAt: postTopology.capturedAt)
    guard let encoded = try? makeWireEncoder().encode(CaptureCoordinateDisplayWireEnvelope(
        id: Int64.max,
        result: result)),
        encoded.count + 1 <= dependencies.ndjsonByteCap else {
        return .failed(code: "response_too_large", retrySafe: false)
    }
    return result
}

/// `Kocoro AX.app` is an LSBackgroundOnly helper. Direct ScreenCaptureKit calls
/// abort in this process (`CGS_REQUIRE_INIT`), so display capture uses the
/// system capture broker. The broker receives the exact Quartz display bounds;
/// the caller validates the stable display ID/topology and physical pixel
/// dimensions before and after capture. No mixed-desktop mosaic is produced.
func runCoordinateDisplayScreencapture(
    display: DisplayTopologyDisplayV1,
    timeout: TimeInterval,
    rawByteCap: Int
) throws -> Data {
    return try withCoordinateWindowTemporaryFile { outputURL in
        do {
            try runCoordinateWindowCaptureProcess(
                executableURL: URL(fileURLWithPath: "/usr/sbin/screencapture"),
                arguments: try coordinateDisplayScreencaptureArguments(
                    display: display,
                    outputURL: outputURL),
                timeout: timeout)
            return try readCoordinateWindowCaptureFile(outputURL, rawByteCap: rawByteCap)
        } catch CaptureCoordinateWindowLiveError.timeout {
            throw CaptureCoordinateDisplayLiveError.timeout
        } catch CaptureCoordinateWindowLiveError.tooLarge {
            throw CaptureCoordinateDisplayLiveError.tooLarge
        } catch {
            throw CaptureCoordinateDisplayLiveError.failed
        }
    }
}

func coordinateDisplayScreencaptureArguments(
    display: DisplayTopologyDisplayV1,
    outputURL: URL
) throws -> [String] {
    let rect = display.quartzBounds
    let values = [rect.x, rect.y, rect.width, rect.height]
    guard values.allSatisfy({
        $0.isFinite && $0.rounded() == $0 &&
            $0 >= Double(Int.min) && $0 <= Double(Int.max)
    }) else {
        throw CaptureCoordinateDisplayLiveError.failed
    }
    return [
        "-x",
        "-R\(Int(rect.x)),\(Int(rect.y)),\(Int(rect.width)),\(Int(rect.height))",
        outputURL.path,
    ]
}

let productionCaptureCoordinateDisplayDependencies = CaptureCoordinateDisplayDependencies(
    observeTopology: { try liveDisplayTopologyService.observe() },
    capturePNG: runCoordinateDisplayScreencapture,
    rawByteCap: 32 * 1024 * 1024,
    ndjsonByteCap: 64 * 1024 * 1024,
    captureTimeout: 8)
