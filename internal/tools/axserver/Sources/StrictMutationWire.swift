import Foundation

struct StrictMutationCodingKey: CodingKey, Hashable {
    let stringValue: String
    let intValue: Int? = nil

    init?(stringValue: String) { self.stringValue = stringValue }
    init?(intValue: Int) { return nil }
}

enum StrictMutationWireError: Error, Equatable {
    case invalid(String)
}

func strictMutationKey(_ value: String) -> StrictMutationCodingKey {
    StrictMutationCodingKey(stringValue: value)!
}

func requireStrictMutationKeys(
    _ container: KeyedDecodingContainer<StrictMutationCodingKey>,
    exactly expected: Set<String>,
    field: String
) throws {
    let actual = Set(container.allKeys.map(\.stringValue))
    guard actual == expected else {
        throw StrictMutationWireError.invalid(
            "\(field) keys differ: expected \(expected.sorted()), got \(actual.sorted())")
    }
}

func strictMutationIdentity(_ value: String) -> Bool {
    !value.isEmpty && value == value.trimmingCharacters(in: .whitespacesAndNewlines)
}

func strictMutationDate(_ value: String) -> Date? {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: value) { return date }
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.date(from: value)
}

private let strictInputModifierNamesV1: Set<String> = [
    "command", "control", "option", "shift",
]

func strictInputModifiersV1(_ modifiers: [String]) -> Bool {
    guard modifiers.count <= strictInputModifierNamesV1.count else { return false }
    var seen: Set<String> = []
    for modifier in modifiers {
        guard strictMutationIdentity(modifier),
              strictInputModifierNamesV1.contains(modifier),
              seen.insert(modifier).inserted else { return false }
    }
    return true
}

func strictInputKeyV1(_ key: String) -> Bool {
    let named: Set<String> = [
        "return", "escape", "tab", "delete", "backspace", "home", "end",
        "pageup", "pagedown", "up", "down", "left", "right", "space",
        "forwarddelete",
        "f1", "f2", "f3", "f4", "f5", "f6",
        "f7", "f8", "f9", "f10", "f11", "f12",
    ]
    if named.contains(key) { return true }
    guard strictMutationIdentity(key), key.utf8.count == 1,
          let byte = key.utf8.first else { return false }
    return (0x20...0x7e).contains(byte) &&
        !(UInt8(ascii: "A")...UInt8(ascii: "Z")).contains(byte)
}

func rejectStrictMutationDuplicateJSONMembers(_ payload: Data) throws {
    var scanner = StrictMutationJSONScanner(payload)
    try scanner.validate()
}

private struct StrictMutationJSONScanner {
    private let bytes: [UInt8]
    private var index = 0

    init(_ payload: Data) { bytes = Array(payload) }

    mutating func validate() throws {
        skipWhitespace()
        try scanValue()
        skipWhitespace()
        guard index == bytes.count else {
            throw StrictMutationWireError.invalid("trailing JSON data")
        }
    }

    private mutating func scanValue() throws {
        skipWhitespace()
        guard let byte = current else {
            throw StrictMutationWireError.invalid("missing JSON value")
        }
        switch byte {
        case UInt8(ascii: "{"): try scanObject()
        case UInt8(ascii: "["): try scanArray()
        case UInt8(ascii: "\""): _ = try scanString()
        default: try scanPrimitive()
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
                throw StrictMutationWireError.invalid("JSON object member must be a string")
            }
            let member = try scanString()
            guard members.insert(member).inserted else {
                throw StrictMutationWireError.invalid("duplicate JSON object member: \(member)")
            }
            skipWhitespace()
            guard consume(UInt8(ascii: ":")) else {
                throw StrictMutationWireError.invalid("JSON object member is missing ':'")
            }
            try scanValue()
            skipWhitespace()
            if consume(UInt8(ascii: "}")) { return }
            guard consume(UInt8(ascii: ",")) else {
                throw StrictMutationWireError.invalid("invalid JSON object separator")
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
                throw StrictMutationWireError.invalid("invalid JSON array separator")
            }
        }
    }

    private mutating func scanString() throws -> String {
        let start = index
        guard consume(UInt8(ascii: "\"")) else {
            throw StrictMutationWireError.invalid("expected JSON string")
        }
        var escaped = false
        while let byte = current {
            index += 1
            if escaped { escaped = false; continue }
            if byte == UInt8(ascii: "\\") { escaped = true; continue }
            if byte == UInt8(ascii: "\"") {
                do {
                    return try JSONDecoder().decode(String.self, from: Data(bytes[start..<index]))
                } catch {
                    throw StrictMutationWireError.invalid("invalid JSON string")
                }
            }
            if byte < 0x20 {
                throw StrictMutationWireError.invalid("unescaped control byte in JSON string")
            }
        }
        throw StrictMutationWireError.invalid("unterminated JSON string")
    }

    private mutating func scanPrimitive() throws {
        let start = index
        while let byte = current,
              !Self.isWhitespace(byte),
              byte != UInt8(ascii: ","),
              byte != UInt8(ascii: "]"),
              byte != UInt8(ascii: "}") { index += 1 }
        guard index > start else {
            throw StrictMutationWireError.invalid("invalid JSON primitive")
        }
    }

    private var current: UInt8? { index < bytes.count ? bytes[index] : nil }
    private mutating func consume(_ byte: UInt8) -> Bool {
        guard current == byte else { return false }
        index += 1
        return true
    }
    private mutating func skipWhitespace() {
        while let byte = current, Self.isWhitespace(byte) { index += 1 }
    }
    private static func isWhitespace(_ byte: UInt8) -> Bool {
        byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D
    }
}
