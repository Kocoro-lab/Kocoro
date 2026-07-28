import Foundation
import CryptoKit

struct Request: Decodable {
    let id: Int64
    let method: String
    let params: Params?
}

struct Params: Decodable {
    let schemaVersion: Int?
    let topologyRef: CaptureCoordinateWindowTopologyRefV1?
    let pid: Int?
    let maxDepth: Int?
    let semanticBudget: Int?
    let filter: String?
    let path: String?
    let expectedRole: String?
    let expectedFingerprint: String?
    let windowID: Int?
    let bundleID: String?
    let expectedQuartzBounds: DisplayTopologyRectV1?
    let fallbackPolicy: String?
    let value: String?
    let appName: String?
    let query: String?
    let role: String?
    let identifier: String?
    let type: String?
    let x: Double?
    let y: Double?
    let button: String?
    let clicks: Int?
    let key: String?
    let modifiers: [String]?
    let dx: Int?
    let dy: Int?
    let windowTitle: String?
    let verify: Bool?
    let condition: String?
    let timeout: Double?
    let interval: Double?
    let roles: [String]?
    let maxLabels: Int?
    let excludedPIDs: [Int]?

    enum CodingKeys: String, CodingKey {
        case pid, filter, path, value, query, role, identifier, type
        case x, y, button, clicks, key, modifiers, dx, dy
        case condition, timeout, interval, verify, roles
        case maxDepth = "max_depth"
        case schemaVersion = "schema_version"
        case topologyRef = "topology_ref"
        case semanticBudget = "semantic_budget"
        case expectedRole = "expected_role"
        case expectedFingerprint = "expected_fingerprint"
        case windowID = "window_id"
        case bundleID = "bundle_id"
        case expectedQuartzBounds = "expected_quartz_bounds"
        case fallbackPolicy = "fallback_policy"
        case appName = "app_name"
        case windowTitle = "window_title"
        case maxLabels = "max_labels"
        case excludedPIDs = "excluded_pids"
    }
}

struct Response: Encodable {
    let id: Int64
    var result: AnyCodable?
    var error: ErrorInfo?
}

struct ErrorInfo: Encodable {
    let code: Int
    let message: String
}

struct AXFrame: Codable, Equatable {
    let x: Double
    let y: Double
    let width: Double
    let height: Double
}

struct AXElementSnapshotAttributes {
    let role: String
    let subrole: String?
    let identifier: String?
    let title: String?
    let description: String?
    let value: String?
    let valueRedacted: Bool
    let protectedContent: Bool
    let enabled: Bool
    let focused: Bool
    let selected: Bool
    let actions: [String]
    let frame: AXFrame?

    init(
        role: String,
        subrole: String?,
        identifier: String?,
        title: String?,
        description: String?,
        value: String?,
        valueRedacted: Bool? = nil,
        protectedContent: Bool,
        enabled: Bool,
        focused: Bool,
        selected: Bool,
        actions: [String],
        frame: AXFrame?
    ) {
        self.role = role
        self.subrole = subrole
        self.identifier = identifier
        self.title = title
        self.description = description
        self.value = value
        self.valueRedacted = valueRedacted ?? (protectedContent ||
            role == "AXSecureTextField" || subrole == "AXSecureTextField")
        self.protectedContent = protectedContent
        self.enabled = enabled
        self.focused = focused
        self.selected = selected
        self.actions = actions
        self.frame = frame
    }
}

struct Element: Encodable {
    let ref: String
    let fingerprint: String
    let path: String
    let role: String
    let subrole: String?
    let identifier: String?
    let title: String?
    let description: String?
    let value: String?
    let valueRedacted: Bool
    let enabled: Bool
    let focused: Bool
    let selected: Bool
    let actions: [String]
    let frame: AXFrame?
    let children: [Element]

    enum CodingKeys: String, CodingKey {
        case ref, fingerprint, path, role, subrole, identifier, title, description
        case desc, value, enabled, focused, selected, actions, frame, children
        case valueRedacted = "value_redacted"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(ref, forKey: .ref)
        try container.encode(fingerprint, forKey: .fingerprint)
        try container.encode(path, forKey: .path)
        try container.encode(role, forKey: .role)
        try container.encodeIfPresent(subrole, forKey: .subrole)
        try container.encodeIfPresent(identifier, forKey: .identifier)
        try container.encodeIfPresent(title, forKey: .title)
        try container.encodeIfPresent(description, forKey: .description)
        // Keep the original key while legacy Go consumers migrate.
        try container.encodeIfPresent(description, forKey: .desc)
        try container.encodeIfPresent(value, forKey: .value)
        try container.encode(valueRedacted, forKey: .valueRedacted)
        try container.encode(enabled, forKey: .enabled)
        try container.encode(focused, forKey: .focused)
        try container.encode(selected, forKey: .selected)
        try container.encode(actions, forKey: .actions)
        try container.encodeIfPresent(frame, forKey: .frame)
        try container.encode(children, forKey: .children)
    }
}

struct ReadTreeResult: Encodable {
    let schemaVersion: Int
    let appName: String
    let bundleID: String?
    let pid: Int
    let windowTitle: String
    let windowID: Int?
    let windowFrame: AXFrame?
    let focusedRef: String?
    let elements: [Element]
    let refPaths: [String: RefEntry]

    enum CodingKeys: String, CodingKey {
        case app, pid, window, elements
        case schemaVersion = "schema_version"
        case appName = "app_name"
        case bundleID = "bundle_id"
        case windowTitle = "window_title"
        case windowID = "window_id"
        case windowFrame = "window_frame"
        case focusedRef = "focused_ref"
        case refPaths = "ref_paths"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(schemaVersion, forKey: .schemaVersion)
        try container.encode(appName, forKey: .appName)
        if let bundleID {
            try container.encode(bundleID, forKey: .bundleID)
        } else {
            try container.encodeNil(forKey: .bundleID)
        }
        try container.encode(appName, forKey: .app)
        try container.encode(pid, forKey: .pid)
        try container.encode(windowTitle, forKey: .windowTitle)
        try container.encode(windowTitle, forKey: .window)
        if let windowID {
            try container.encode(windowID, forKey: .windowID)
        } else {
            try container.encodeNil(forKey: .windowID)
        }
        if let windowFrame {
            try container.encode(windowFrame, forKey: .windowFrame)
        } else {
            try container.encodeNil(forKey: .windowFrame)
        }
        if let focusedRef {
            try container.encode(focusedRef, forKey: .focusedRef)
        } else {
            try container.encodeNil(forKey: .focusedRef)
        }
        try container.encode(elements, forKey: .elements)
        try container.encode(refPaths, forKey: .refPaths)
    }
}

struct RefEntry: Encodable {
    let path: String
    let role: String
    let fingerprint: String?

    init(path: String, role: String, fingerprint: String? = nil) {
        self.path = path
        self.role = role
        self.fingerprint = fingerprint
    }
}

func makeWireEncoder() -> JSONEncoder {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys]
    return encoder
}

private func normalizedFingerprintComponent(_ value: String?) -> String {
    value?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
}

/// An intrinsic identity hint. It deliberately excludes ref, path, frame,
/// value, and children so traversal numbering and child order cannot change it.
func stableElementFingerprint(_ attributes: AXElementSnapshotAttributes) -> String {
    let components = [
        attributes.role,
        normalizedFingerprintComponent(attributes.subrole),
        normalizedFingerprintComponent(attributes.identifier),
        normalizedFingerprintComponent(attributes.title),
        normalizedFingerprintComponent(attributes.description),
    ]
    var bytes = Data()
    for component in components {
        let encoded = Data(component.utf8)
        var length = UInt64(encoded.count).bigEndian
        withUnsafeBytes(of: &length) { bytes.append(contentsOf: $0) }
        bytes.append(encoded)
    }
    return "axf_" + SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined()
}

func makeElementSnapshot(
    attributes: AXElementSnapshotAttributes,
    ref: String,
    path: String,
    children: [Element]
) -> Element {
    return Element(
        ref: ref,
        fingerprint: stableElementFingerprint(attributes),
        path: path,
        role: attributes.role,
        subrole: attributes.subrole,
        identifier: attributes.identifier,
        title: attributes.title,
        description: attributes.description,
        value: attributes.valueRedacted ? nil : attributes.value,
        valueRedacted: attributes.valueRedacted,
        enabled: attributes.enabled,
        focused: attributes.focused,
        selected: attributes.selected,
        actions: attributes.actions.sorted(),
        frame: attributes.frame,
        children: children)
}

struct AnnotationEntry: Encodable {
    let label: Int
    let ref: String
    let role: String
    var title: String?
    let x: Double
    let y: Double
    let width: Double
    let height: Double
}

private struct AnnotationContentSignatureEntry: Encodable {
    let role: String
    let title: String?
    let x: Double
    let y: Double
    let width: Double
    let height: Double
}

private struct AnnotationContentSignaturePayload: Encodable {
    let windowTitle: String
    let windowID: Int?
    let windowFrame: AXFrame?
    let annotations: [AnnotationContentSignatureEntry]
}

func annotationContentSignature(
    windowTitle: String,
    windowID: Int?,
    windowFrame: AXFrame?,
    annotations: [AnnotationEntry]
) -> String {
    let payload = AnnotationContentSignaturePayload(
        windowTitle: windowTitle,
        windowID: windowID,
        windowFrame: windowFrame,
        annotations: annotations.map {
            AnnotationContentSignatureEntry(
                role: $0.role, title: $0.title,
                x: $0.x, y: $0.y, width: $0.width, height: $0.height)
        })
    guard let data = try? makeWireEncoder().encode(payload) else { return "" }
    return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
}

struct AnnotateResult: Encodable {
    let app: String
    let appName: String
    let bundleID: String?
    let pid: Int
    let window: String
    let windowID: Int?
    let windowFrame: AXFrame?
    let contentSignature: String
    let annotations: [AnnotationEntry]
    let refPaths: [String: RefEntry]

    init(
        app: String,
        appName: String,
        bundleID: String?,
        pid: Int,
        window: String,
        windowID: Int?,
        windowFrame: AXFrame?,
        annotations: [AnnotationEntry],
        refPaths: [String: RefEntry]
    ) {
        self.app = app
        self.appName = appName
        self.bundleID = bundleID
        self.pid = pid
        self.window = window
        self.windowID = windowID
        self.windowFrame = windowFrame
        self.annotations = annotations
        self.refPaths = refPaths
        self.contentSignature = annotationContentSignature(
            windowTitle: window,
            windowID: windowID,
            windowFrame: windowFrame,
            annotations: annotations)
    }

    enum CodingKeys: String, CodingKey {
        case app, pid, window, annotations
        case appName = "app_name"
        case bundleID = "bundle_id"
        case windowID = "window_id"
        case windowFrame = "window_frame"
        case contentSignature = "content_signature"
        case refPaths = "ref_paths"
    }
}

struct FindResult: Encodable {
    let path: String
    let role: String
    let title: String
    var desc: String?
    var value: String?
}

struct AppContext: Encodable {
    let app: String
    let window: String
    var url: String?
    var focusedElement: String?

    enum CodingKeys: String, CodingKey {
        case app, window, url
        case focusedElement = "focused_element"
    }
}

struct ActionResult: Encodable {
    let result: String
    var role: String?
    var context: AppContext?
}

/// Type-erased Encodable wrapper for JSON responses.
struct AnyCodable: Encodable {
    private let _encode: (Encoder) throws -> Void

    init<T: Encodable>(_ value: T) {
        _encode = { encoder in
            try value.encode(to: encoder)
        }
    }

    func encode(to encoder: Encoder) throws {
        try _encode(encoder)
    }
}
