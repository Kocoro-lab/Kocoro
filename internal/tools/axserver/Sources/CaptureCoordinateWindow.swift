import AppKit
import CoreGraphics
import CryptoKit
import Foundation
import ImageIO

struct CaptureCoordinateWindowTopologyRefV1: Codable, Equatable {
    let topologyID: String
    let generation: UInt64

    enum CodingKeys: String, CodingKey {
        case topologyID = "topology_id"
        case generation
    }
}

struct CaptureCoordinateWindowRequestV1: Equatable {
    let schemaVersion: Int
    let topologyRef: CaptureCoordinateWindowTopologyRefV1
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let expectedQuartzBounds: DisplayTopologyRectV1

    init(
        schemaVersion: Int,
        topologyRef: CaptureCoordinateWindowTopologyRefV1,
        pid: Int,
        bundleID: String,
        windowID: UInt32,
        expectedQuartzBounds: DisplayTopologyRectV1
    ) {
        self.schemaVersion = schemaVersion
        self.topologyRef = topologyRef
        self.pid = pid
        self.bundleID = bundleID
        self.windowID = windowID
        self.expectedQuartzBounds = expectedQuartzBounds
    }

    init(params: Params) throws {
        guard let schemaVersion = params.schemaVersion,
              let topologyRef = params.topologyRef,
              let pid = params.pid,
              let bundleID = params.bundleID,
              let rawWindowID = params.windowID,
              rawWindowID > 0,
              let windowID = UInt32(exactly: rawWindowID),
              let expectedQuartzBounds = params.expectedQuartzBounds else {
            throw CaptureCoordinateWindowLiveError.invalidRequest
        }
        self.init(
            schemaVersion: schemaVersion,
            topologyRef: topologyRef,
            pid: pid,
            bundleID: bundleID,
            windowID: windowID,
            expectedQuartzBounds: expectedQuartzBounds)
        try validate()
    }

    func validate() throws {
        guard schemaVersion == 1,
              !topologyRef.topologyID.isEmpty,
              topologyRef.generation > 0,
              pid > 0,
              !bundleID.isEmpty,
              windowID > 0 else {
            throw CaptureCoordinateWindowLiveError.invalidRequest
        }
        do {
            try expectedQuartzBounds.validate(field: "expected_quartz_bounds")
        } catch {
            throw CaptureCoordinateWindowLiveError.invalidRequest
        }
    }
}

struct CaptureCoordinateWindowWindowSnapshot: Equatable {
    let windowID: UInt32
    let ownerPID: Int
    let layer: Int
    let isOnScreen: Bool
    let bounds: DisplayTopologyRectV1
}

struct CaptureCoordinateWindowFailureDiagnosticsV1: Encodable, Equatable {
    let stage: String
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let preWindowQuartzBounds: DisplayTopologyRectV1
    let postWindowQuartzBounds: DisplayTopologyRectV1
    let displayID: UInt32
    let backingScaleFactor: Double
    let expectedWidthPX: Double
    let expectedHeightPX: Double
    let metadataWidthPX: Int?
    let metadataHeightPX: Int?
    let decodedWidthPX: Int?
    let decodedHeightPX: Int?

    enum CodingKeys: String, CodingKey {
        case stage, pid
        case bundleID = "bundle_id"
        case windowID = "window_id"
        case preWindowQuartzBounds = "pre_window_quartz_bounds"
        case postWindowQuartzBounds = "post_window_quartz_bounds"
        case displayID = "display_id"
        case backingScaleFactor = "backing_scale_factor"
        case expectedWidthPX = "expected_width_px"
        case expectedHeightPX = "expected_height_px"
        case metadataWidthPX = "metadata_width_px"
        case metadataHeightPX = "metadata_height_px"
        case decodedWidthPX = "decoded_width_px"
        case decodedHeightPX = "decoded_height_px"
    }
}

enum CaptureCoordinateWindowLiveError: Error, Equatable {
    case invalidRequest
    case timeout
    case tooLarge
    case failed
}

struct CaptureCoordinateWindowDependencies {
    let observeTopology: () throws -> DisplayTopologyV1
    let bundleIDForPID: (Int) -> String?
    let exactWindow: (UInt32) throws -> CaptureCoordinateWindowWindowSnapshot?
    let capturePNG: (UInt32, TimeInterval, Int) throws -> Data
    let rawByteCap: Int
    let ndjsonByteCap: Int
    let captureTimeout: TimeInterval
}

struct CaptureCoordinateWindowResultV1: Encodable {
    let schemaVersion: Int
    let status: String
    let failureCode: String?
    let retrySafe: Bool
    let failureDiagnostics: CaptureCoordinateWindowFailureDiagnosticsV1?
    let topologyRef: CaptureCoordinateWindowTopologyRefV1?
    let helperBootID: String?
    let pid: Int?
    let bundleID: String?
    let windowID: UInt32?
    let windowQuartzBounds: DisplayTopologyRectV1?
    let displayID: UInt32?
    let backingScaleFactor: Double?
    let mediaType: String?
    let widthPX: Int?
    let heightPX: Int?
    let byteLength: Int?
    let sha256: String?
    let imageBase64: String?
    let capturedAt: String?

    enum CodingKeys: String, CodingKey {
        case status, pid
        case schemaVersion = "schema_version"
        case failureCode = "failure_code"
        case retrySafe = "retry_safe"
        case failureDiagnostics = "failure_diagnostics"
        case topologyRef = "topology_ref"
        case helperBootID = "helper_boot_id"
        case bundleID = "bundle_id"
        case windowID = "window_id"
        case windowQuartzBounds = "window_quartz_bounds"
        case displayID = "display_id"
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
        pid: Int,
        bundleID: String,
        windowID: UInt32,
        windowQuartzBounds: DisplayTopologyRectV1,
        displayID: UInt32,
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
            failureDiagnostics: nil,
            topologyRef: topologyRef,
            helperBootID: helperBootID,
            pid: pid,
            bundleID: bundleID,
            windowID: windowID,
            windowQuartzBounds: windowQuartzBounds,
            displayID: displayID,
            backingScaleFactor: backingScaleFactor,
            mediaType: "image/png",
            widthPX: widthPX,
            heightPX: heightPX,
            byteLength: png.count,
            sha256: SHA256.hash(data: png).map { String(format: "%02x", $0) }.joined(),
            imageBase64: png.base64EncodedString(),
            capturedAt: capturedAt)
    }

    static func failed(
        code: String,
        retrySafe: Bool,
        diagnostics: CaptureCoordinateWindowFailureDiagnosticsV1? = nil
    ) -> Self {
        Self(
            schemaVersion: 1,
            status: "failed",
            failureCode: code,
            retrySafe: retrySafe,
            failureDiagnostics: diagnostics,
            topologyRef: nil,
            helperBootID: nil,
            pid: nil,
            bundleID: nil,
            windowID: nil,
            windowQuartzBounds: nil,
            displayID: nil,
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
        try encodeNullable(failureDiagnostics, into: &container, key: .failureDiagnostics)
        try encodeNullable(topologyRef, into: &container, key: .topologyRef)
        try encodeNullable(helperBootID, into: &container, key: .helperBootID)
        try encodeNullable(pid, into: &container, key: .pid)
        try encodeNullable(bundleID, into: &container, key: .bundleID)
        try encodeNullable(windowID, into: &container, key: .windowID)
        try encodeNullable(windowQuartzBounds, into: &container, key: .windowQuartzBounds)
        try encodeNullable(displayID, into: &container, key: .displayID)
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

private struct CaptureCoordinateWindowWireEnvelope: Encodable {
    let id: Int64
    let result: CaptureCoordinateWindowResultV1
}

func captureCoordinateWindow(
    request: CaptureCoordinateWindowRequestV1,
    dependencies: CaptureCoordinateWindowDependencies
) -> CaptureCoordinateWindowResultV1 {
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
    let requestedTopology = CaptureCoordinateWindowTopologyRefV1(
        topologyID: preTopology.topologyID,
        generation: preTopology.generation)
    guard requestedTopology == request.topologyRef else {
        return .failed(code: "stale_topology", retrySafe: true)
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return .failed(code: "process_identity_mismatch", retrySafe: false)
    }

    let preWindow: CaptureCoordinateWindowWindowSnapshot
    do {
        guard let window = try dependencies.exactWindow(request.windowID) else {
            return .failed(code: "window_not_found", retrySafe: true)
        }
        preWindow = window
    } catch {
        return .failed(code: "window_not_found", retrySafe: true)
    }
    guard preWindow.windowID == request.windowID, preWindow.ownerPID == request.pid else {
        return .failed(code: "window_identity_mismatch", retrySafe: false)
    }
    guard preWindow.layer == 0, preWindow.isOnScreen else {
        return .failed(code: "window_not_actionable", retrySafe: true)
    }
    guard coordinateWindowRectsCorrelate(preWindow.bounds, request.expectedQuartzBounds) else {
        return .failed(code: "window_bounds_mismatch", retrySafe: true)
    }
    let containingDisplays = preTopology.displays.filter { display in
        display.isActive && display.isOnline && !display.isAsleep &&
            display.mirrorMasterDisplayID == nil && display.rotationDegrees == 0 &&
            coordinateWindowRect(display.quartzBounds, fullyContains: preWindow.bounds)
    }
    guard containingDisplays.count == 1 else {
        return .failed(code: "display_not_actionable", retrySafe: true)
    }
    let display = containingDisplays[0]

    let png: Data
    do {
        png = try dependencies.capturePNG(
            request.windowID,
            dependencies.captureTimeout,
            dependencies.rawByteCap)
    } catch CaptureCoordinateWindowLiveError.timeout {
        return .failed(code: "capture_timeout", retrySafe: true)
    } catch CaptureCoordinateWindowLiveError.tooLarge {
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
    let postTopologyRef = CaptureCoordinateWindowTopologyRefV1(
        topologyID: postTopology.topologyID,
        generation: postTopology.generation)
    guard postTopologyRef == request.topologyRef,
          postTopology.helperBootID == preTopology.helperBootID,
          postTopology.mainDisplayID == preTopology.mainDisplayID,
          postTopology.displays == preTopology.displays else {
        return .failed(code: "topology_changed", retrySafe: true)
    }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return .failed(code: "process_identity_mismatch", retrySafe: false)
    }
    let postWindow: CaptureCoordinateWindowWindowSnapshot
    do {
        guard let window = try dependencies.exactWindow(request.windowID) else {
            return .failed(code: "window_changed", retrySafe: true)
        }
        postWindow = window
    } catch {
        return .failed(code: "window_changed", retrySafe: true)
    }
    guard postWindow.windowID == preWindow.windowID,
          postWindow.ownerPID == preWindow.ownerPID,
          postWindow.layer == preWindow.layer,
          postWindow.isOnScreen == preWindow.isOnScreen,
          coordinateWindowRectsEqual(postWindow.bounds, preWindow.bounds) else {
        return .failed(code: "window_changed", retrySafe: true)
    }

    let expectedWidth = preWindow.bounds.width * display.backingScaleFactor
    let expectedHeight = preWindow.bounds.height * display.backingScaleFactor
    guard expectedWidth.isFinite, expectedHeight.isFinite,
          abs(expectedWidth.rounded() - expectedWidth) <= 0.000_001,
          abs(expectedHeight.rounded() - expectedHeight) <= 0.000_001 else {
        return .failed(
            code: "image_dimensions_mismatch",
            retrySafe: true,
            diagnostics: CaptureCoordinateWindowFailureDiagnosticsV1(
                stage: "non_integral_expected_dimensions",
                pid: request.pid,
                bundleID: request.bundleID,
                windowID: request.windowID,
                preWindowQuartzBounds: preWindow.bounds,
                postWindowQuartzBounds: postWindow.bounds,
                displayID: display.displayID,
                backingScaleFactor: display.backingScaleFactor,
                expectedWidthPX: expectedWidth,
                expectedHeightPX: expectedHeight,
                metadataWidthPX: nil,
                metadataHeightPX: nil,
                decodedWidthPX: nil,
                decodedHeightPX: nil))
    }
    let dimensions: (width: Int, height: Int)
    switch decodeCoordinateWindowPNG(
        png,
        expectedWidth: Int(expectedWidth.rounded()),
        expectedHeight: Int(expectedHeight.rounded())) {
    case let .valid(width, height):
        dimensions = (width, height)
    case let .dimensionMismatch(metadataWidth, metadataHeight, decodedWidth, decodedHeight):
        return .failed(
            code: "image_dimensions_mismatch",
            retrySafe: true,
            diagnostics: CaptureCoordinateWindowFailureDiagnosticsV1(
                stage: "decoded_dimensions",
                pid: request.pid,
                bundleID: request.bundleID,
                windowID: request.windowID,
                preWindowQuartzBounds: preWindow.bounds,
                postWindowQuartzBounds: postWindow.bounds,
                displayID: display.displayID,
                backingScaleFactor: display.backingScaleFactor,
                expectedWidthPX: expectedWidth,
                expectedHeightPX: expectedHeight,
                metadataWidthPX: metadataWidth,
                metadataHeightPX: metadataHeight,
                decodedWidthPX: decodedWidth,
                decodedHeightPX: decodedHeight))
    case .invalid:
        return .failed(code: "invalid_png", retrySafe: false)
    }
    let result = CaptureCoordinateWindowResultV1.captured(
        topologyRef: postTopologyRef,
        helperBootID: postTopology.helperBootID,
        pid: request.pid,
        bundleID: request.bundleID,
        windowID: request.windowID,
        windowQuartzBounds: postWindow.bounds,
        displayID: display.displayID,
        backingScaleFactor: display.backingScaleFactor,
        png: png,
        widthPX: dimensions.width,
        heightPX: dimensions.height,
        capturedAt: postTopology.capturedAt)
    guard let encoded = try? makeWireEncoder().encode(CaptureCoordinateWindowWireEnvelope(
        id: Int64.max,
        result: result)),
        encoded.count + 1 <= dependencies.ndjsonByteCap else {
        return .failed(code: "response_too_large", retrySafe: false)
    }
    return result
}

private func coordinateWindowRectsEqual(
    _ left: DisplayTopologyRectV1,
    _ right: DisplayTopologyRectV1
) -> Bool {
    abs(left.x - right.x) <= 0.000_001 &&
        abs(left.y - right.y) <= 0.000_001 &&
        abs(left.width - right.width) <= 0.000_001 &&
        abs(left.height - right.height) <= 0.000_001
}

private func coordinateWindowRectsCorrelate(
    _ live: DisplayTopologyRectV1,
    _ expected: DisplayTopologyRectV1
) -> Bool {
    abs(live.x - expected.x) <= 2 &&
        abs(live.y - expected.y) <= 2 &&
        abs(live.width - expected.width) <= 2 &&
        abs(live.height - expected.height) <= 2
}

private func coordinateWindowRect(
    _ outer: DisplayTopologyRectV1,
    fullyContains inner: DisplayTopologyRectV1
) -> Bool {
    inner.x >= outer.x && inner.y >= outer.y &&
        inner.x + inner.width <= outer.x + outer.width &&
        inner.y + inner.height <= outer.y + outer.height
}

enum CoordinateWindowPNGDecodeResult {
    case valid(width: Int, height: Int)
    case dimensionMismatch(
        metadataWidth: Int,
        metadataHeight: Int,
        decodedWidth: Int,
        decodedHeight: Int
    )
    case invalid
}

func decodeCoordinateWindowPNG(
    _ data: Data,
    expectedWidth: Int,
    expectedHeight: Int
) -> CoordinateWindowPNGDecodeResult {
    let signature: [UInt8] = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]
    guard data.count >= 24, Array(data.prefix(8)) == signature,
          validateCoordinateWindowPNGChunks(data),
          let source = CGImageSourceCreateWithData(data as CFData, nil),
          CGImageSourceGetCount(source) == 1,
          let image = CGImageSourceCreateImageAtIndex(source, 0, nil),
          let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any],
          let width = properties[kCGImagePropertyPixelWidth] as? NSNumber,
          let height = properties[kCGImagePropertyPixelHeight] as? NSNumber else {
        return .invalid
    }
    guard width.intValue == expectedWidth,
          height.intValue == expectedHeight,
          image.width == expectedWidth,
          image.height == expectedHeight else {
        return .dimensionMismatch(
            metadataWidth: width.intValue,
            metadataHeight: height.intValue,
            decodedWidth: image.width,
            decodedHeight: image.height)
    }
    var decodedPixel = [UInt8](repeating: 0, count: 4)
    guard let colorSpace = CGColorSpace(name: CGColorSpace.sRGB),
          let context = CGContext(
            data: &decodedPixel,
            width: 1,
            height: 1,
            bitsPerComponent: 8,
            bytesPerRow: 4,
            space: colorSpace,
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
        return .invalid
    }
    context.interpolationQuality = .none
    context.draw(image, in: CGRect(x: 0, y: 0, width: 1, height: 1))
    guard CGImageSourceGetStatusAtIndex(source, 0) == .statusComplete else { return .invalid }
    return .valid(width: image.width, height: image.height)
}

private func validateCoordinateWindowPNGChunks(_ data: Data) -> Bool {
    let bytes = [UInt8](data)
    var offset = 8
    var index = 0
    var sawIDAT = false
    var sawIEND = false
    while offset < bytes.count {
        guard bytes.count - offset >= 12 else { return false }
        let length = Int(readCoordinateWindowBigEndianUInt32(bytes, offset: offset))
        guard length >= 0, length <= bytes.count - offset - 12 else { return false }
        let typeStart = offset + 4
        let dataEnd = typeStart + 4 + length
        let chunkType = String(bytes: bytes[typeStart..<(typeStart + 4)], encoding: .ascii)
        let expectedCRC = readCoordinateWindowBigEndianUInt32(bytes, offset: dataEnd)
        let actualCRC = coordinateWindowCRC32(bytes[typeStart..<dataEnd])
        guard expectedCRC == actualCRC else { return false }
        if index == 0 && (chunkType != "IHDR" || length != 13) { return false }
        if chunkType == "IDAT" { sawIDAT = true }
        if chunkType == "IEND" {
            guard length == 0, dataEnd + 4 == bytes.count else { return false }
            sawIEND = true
            break
        }
        offset = dataEnd + 4
        index += 1
    }
    return sawIDAT && sawIEND
}

private func readCoordinateWindowBigEndianUInt32(_ bytes: [UInt8], offset: Int) -> UInt32 {
    UInt32(bytes[offset]) << 24 |
        UInt32(bytes[offset + 1]) << 16 |
        UInt32(bytes[offset + 2]) << 8 |
        UInt32(bytes[offset + 3])
}

private func coordinateWindowCRC32(_ bytes: ArraySlice<UInt8>) -> UInt32 {
    var crc: UInt32 = 0xffff_ffff
    for byte in bytes {
        crc ^= UInt32(byte)
        for _ in 0..<8 {
            crc = (crc & 1) == 1 ? (crc >> 1) ^ 0xedb8_8320 : crc >> 1
        }
    }
    return crc ^ 0xffff_ffff
}

private func productionBundleIDForPID(_ pid: Int) -> String? {
    guard let processID = pid_t(exactly: pid) else { return nil }
    return NSRunningApplication(processIdentifier: processID)?.bundleIdentifier
}

private func productionExactCoordinateWindow(
    _ windowID: UInt32
) throws -> CaptureCoordinateWindowWindowSnapshot? {
    guard let raw = CGWindowListCopyWindowInfo(
        [.optionIncludingWindow],
        CGWindowID(windowID)) as? [[String: Any]] else {
        return nil
    }
    let exact = raw.filter { info in
        (info[kCGWindowNumber as String] as? NSNumber)?.uint32Value == windowID
    }
    guard exact.count == 1, let info = exact.first,
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

func runCoordinateWindowScreencapture(
    windowID: UInt32,
    timeout: TimeInterval,
    rawByteCap: Int
) throws -> Data {
    refreshAppKitState()
    let exactWindow = try productionExactCoordinateWindow(windowID)
    let foregroundCompositeBounds: DisplayTopologyRectV1?
    if let exactWindow,
       NSWorkspace.shared.frontmostApplication?.processIdentifier ==
       pid_t(exactWindow.ownerPID) {
        foregroundCompositeBounds = exactWindow.bounds
    } else {
        foregroundCompositeBounds = nil
    }
    return try withCoordinateWindowTemporaryFile { outputURL in
        try runCoordinateWindowCaptureProcess(
            executableURL: URL(fileURLWithPath: "/usr/sbin/screencapture"),
            arguments: coordinateWindowScreencaptureArguments(
                windowID: windowID,
                foregroundCompositeBounds: foregroundCompositeBounds,
                outputURL: outputURL),
            timeout: timeout)
        if foregroundCompositeBounds != nil, let exactWindow {
            refreshAppKitState()
            guard NSWorkspace.shared.frontmostApplication?.processIdentifier ==
                    pid_t(exactWindow.ownerPID) else {
                throw CaptureCoordinateWindowLiveError.failed
            }
        }
        return try readCoordinateWindowCaptureFile(outputURL, rawByteCap: rawByteCap)
    }
}

/// Background capture isolates the selected CGWindow so overlapping apps never
/// enter its coordinate frame. For the frontmost process, an exact region
/// capture preserves the same frame while also showing sheets and popovers
/// composited by WindowServer. Non-integral bounds remain fail-closed on the
/// isolated path rather than silently rounding the coordinate anchor.
func coordinateWindowScreencaptureArguments(
    windowID: UInt32,
    foregroundCompositeBounds: DisplayTopologyRectV1?,
    outputURL: URL
) -> [String] {
    if let bounds = foregroundCompositeBounds,
       bounds.x.isFinite, bounds.y.isFinite,
       bounds.width.isFinite, bounds.height.isFinite,
       bounds.x.rounded() == bounds.x,
       bounds.y.rounded() == bounds.y,
       bounds.width.rounded() == bounds.width,
       bounds.height.rounded() == bounds.height,
       bounds.width > 0, bounds.height > 0,
       let x = Int(exactly: bounds.x),
       let y = Int(exactly: bounds.y),
       let width = Int(exactly: bounds.width),
       let height = Int(exactly: bounds.height) {
        return ["-x", "-R\(x),\(y),\(width),\(height)", outputURL.path]
    }
    return ["-x", "-o", "-a", "-l\(windowID)", outputURL.path]
}

func withCoordinateWindowTemporaryFile<T>(
    _ body: (URL) throws -> T
) rethrows -> T {
    let outputURL = FileManager.default.temporaryDirectory
        .appendingPathComponent("kocoro-coordinate-window-\(UUID().uuidString).png")
    defer { try? FileManager.default.removeItem(at: outputURL) }
    return try body(outputURL)
}

func runCoordinateWindowCaptureProcess(
    executableURL: URL,
    arguments: [String],
    timeout: TimeInterval
) throws {
    guard timeout > 0, timeout <= 8 else {
        throw CaptureCoordinateWindowLiveError.failed
    }
    let process = FoundationCoordinateWindowProcess(
        executableURL: executableURL,
        arguments: arguments)
    try runCoordinateWindowCaptureProcess(process: process, timeout: timeout)
}

protocol CoordinateWindowProcessControlling: AnyObject {
    var isRunning: Bool { get }
    var terminationStatus: Int32 { get }
    func start(onTermination: @escaping () -> Void) throws
    func terminate()
    func kill()
    func waitUntilExit()
}

private final class FoundationCoordinateWindowProcess: CoordinateWindowProcessControlling {
    private let process = Process()

    init(executableURL: URL, arguments: [String]) {
        process.executableURL = executableURL
        process.arguments = arguments
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
    }

    var isRunning: Bool { process.isRunning }
    var terminationStatus: Int32 { process.terminationStatus }

    func start(onTermination: @escaping () -> Void) throws {
        process.terminationHandler = { _ in onTermination() }
        try process.run()
    }

    func terminate() { process.terminate() }
    func kill() { Darwin.kill(process.processIdentifier, SIGKILL) }
    func waitUntilExit() { process.waitUntilExit() }
}

func runCoordinateWindowCaptureProcess(
    process: CoordinateWindowProcessControlling,
    timeout: TimeInterval,
    terminationGrace: TimeInterval = 0.5,
    killGrace: TimeInterval = 1
) throws {
    guard timeout > 0, timeout <= 8,
          terminationGrace >= 0, killGrace >= 0 else {
        throw CaptureCoordinateWindowLiveError.failed
    }
    let terminated = DispatchSemaphore(value: 0)
    do {
        try process.start { terminated.signal() }
    } catch {
        throw CaptureCoordinateWindowLiveError.failed
    }
    if terminated.wait(timeout: .now() + timeout) == .timedOut {
        process.terminate()
        if terminated.wait(timeout: .now() + terminationGrace) == .timedOut {
            process.kill()
            _ = terminated.wait(timeout: .now() + killGrace)
        }
        if process.isRunning {
            process.kill()
        }
        process.waitUntilExit()
        throw CaptureCoordinateWindowLiveError.timeout
    }
    process.waitUntilExit()
    guard process.terminationStatus == 0 else {
        throw CaptureCoordinateWindowLiveError.failed
    }
}

func readCoordinateWindowCaptureFile(
    _ outputURL: URL,
    rawByteCap: Int
) throws -> Data {
    guard rawByteCap > 0,
          let attributes = try? FileManager.default.attributesOfItem(atPath: outputURL.path),
          let fileSize = attributes[.size] as? NSNumber else {
        throw CaptureCoordinateWindowLiveError.failed
    }
    guard fileSize.uint64Value <= UInt64(rawByteCap) else {
        throw CaptureCoordinateWindowLiveError.tooLarge
    }
    guard let data = try? Data(contentsOf: outputURL, options: [.mappedIfSafe]) else {
        throw CaptureCoordinateWindowLiveError.failed
    }
    guard data.count <= rawByteCap else { throw CaptureCoordinateWindowLiveError.tooLarge }
    guard UInt64(data.count) == fileSize.uint64Value else {
        throw CaptureCoordinateWindowLiveError.failed
    }
    return data
}

let productionCaptureCoordinateWindowDependencies = CaptureCoordinateWindowDependencies(
    observeTopology: { try liveDisplayTopologyService.observe() },
    bundleIDForPID: productionBundleIDForPID,
    exactWindow: productionExactCoordinateWindow,
    capturePNG: runCoordinateWindowScreencapture,
    rawByteCap: 32 * 1024 * 1024,
    ndjsonByteCap: 64 * 1024 * 1024,
    captureTimeout: 8)
