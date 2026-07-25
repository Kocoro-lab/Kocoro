import CoreGraphics
import AppKit

struct InputDriver {
    static func mouseEvent(type: String, x: Double, y: Double,
                           button: String = "left", clicks: Int = 1) -> (ActionResult?, ErrorInfo?) {
        let point = CGPoint(x: x, y: y)

        switch type {
        case "click":
            let (btn, down, up) = mouseConstants(button)
            if let error = movePointer(to: point) {
                return (nil, error)
            }
            for i in 0..<clicks {
                guard let downEvent = CGEvent(mouseEventSource: nil, mouseType: down,
                                              mouseCursorPosition: point, mouseButton: btn),
                      let upEvent = CGEvent(mouseEventSource: nil, mouseType: up,
                                            mouseCursorPosition: point, mouseButton: btn) else {
                    return (nil, ErrorInfo(code: -1, message: "failed to create mouse click event"))
                }
                if clicks > 1 {
                    downEvent.setIntegerValueField(.mouseEventClickState, value: Int64(i + 1))
                    upEvent.setIntegerValueField(.mouseEventClickState, value: Int64(i + 1))
                }
                guard postMousePair(
                    down: downEvent, up: upEvent, buttonName: button, button: btn) else {
                    return (nil, ErrorInfo(
                        code: -1,
                        message: "input commit blocked or mouse release unconfirmed"))
                }
            }
            return (ActionResult(result: "clicked \(button) at (\(Int(x)), \(Int(y))) \(clicks)x"), nil)

        case "move":
            if let error = movePointer(to: point) {
                return (nil, error)
            }
            return (ActionResult(result: "moved to (\(Int(x)), \(Int(y)))"), nil)

        default:
            return (nil, ErrorInfo(code: -1, message: "unknown mouse event type: \(type)"))
        }
    }

    /// Moves the user's real pointer before a coordinate or semantic click so
    /// GUI automation stays visible instead of teleporting a hidden event.
    static func movePointer(to point: CGPoint) -> ErrorInfo? {
        let bounds = activeDisplayBounds()
        guard !bounds.isEmpty, isPointOnScreen(point, displayBounds: bounds) else {
            return ErrorInfo(code: -1, message: "mouse target is outside all active displays: (\(Int(point.x)), \(Int(point.y)))")
        }

        guard let move = CGEvent(mouseEventSource: nil, mouseType: .mouseMoved,
                                 mouseCursorPosition: point, mouseButton: .left) else {
            return ErrorInfo(code: -1, message: "failed to create mouse move event")
        }
        var observed: CGPoint?
        var warpError: CGError?
        guard processInputCommitGateV1.commitSample({
            let result = CGWarpMouseCursorPosition(point)
            guard result == .success else { warpError = result; return false }
            _ = CGAssociateMouseAndMouseCursorPosition(boolean_t(1))
            move.post(tap: .cghidEventTap)
            observed = CGEvent(source: nil)?.location
            return true
        }) else {
            if let warpError {
                return ErrorInfo(
                    code: -1,
                    message: "failed to move mouse cursor (CGError \(warpError.rawValue))")
            }
            return ErrorInfo(code: -1, message: "input commit blocked during pointer move")
        }

        guard let observed, pointerReached(point, observed: observed) else {
            let suffix = observed.map { " observed (\(Int($0.x)), \(Int($0.y)))" } ?? ""
            return ErrorInfo(code: -1, message: "mouse cursor did not reach (\(Int(point.x)), \(Int(point.y)))\(suffix)")
        }
        return nil
    }

    static func isPointOnScreen(_ point: CGPoint, displayBounds: [CGRect]) -> Bool {
        displayBounds.contains { $0.contains(point) }
    }

    static func pointerReached(_ requested: CGPoint, observed: CGPoint, tolerance: CGFloat = 2) -> Bool {
        abs(requested.x - observed.x) <= tolerance && abs(requested.y - observed.y) <= tolerance
    }

    private static func activeDisplayBounds() -> [CGRect] {
        var count: UInt32 = 0
        guard CGGetActiveDisplayList(0, nil, &count) == .success, count > 0 else {
            return []
        }
        var displays = Array(repeating: CGDirectDisplayID(), count: Int(count))
        guard CGGetActiveDisplayList(count, &displays, &count) == .success else {
            return []
        }
        return displays.prefix(Int(count)).map(CGDisplayBounds)
    }

    static func keyEvent(key: String, modifiers: [String]) -> (ActionResult?, ErrorInfo?) {
        let validMods: Set<String> = ["command", "cmd", "shift", "option", "alt", "control", "ctrl"]
        var flags: CGEventFlags = []
        for mod in modifiers {
            let m = mod.lowercased()
            guard validMods.contains(m) else {
                return (nil, ErrorInfo(code: -1, message: "unknown modifier: \(mod) (valid: command, cmd, shift, option, alt, control, ctrl)"))
            }
            switch m {
            case "command", "cmd": flags.insert(.maskCommand)
            case "shift": flags.insert(.maskShift)
            case "option", "alt": flags.insert(.maskAlternate)
            case "control", "ctrl": flags.insert(.maskControl)
            default: break
            }
        }

        guard let keyCode = keyCodeMap[key.lowercased()] else {
            return (nil, ErrorInfo(code: -1, message: "unknown key: \(key)"))
        }

        guard postKeyPair(keyCode: keyCode, flags: flags) else {
            return (nil, ErrorInfo(
                code: -1, message: "input commit blocked or key release unconfirmed"))
        }

        let modStr = modifiers.isEmpty ? "" : modifiers.joined(separator: "+") + "+"
        return (ActionResult(result: "pressed \(modStr)\(key)"), nil)
    }

    /// Type text. Non-ASCII (CJK, emoji) routes through clipboard paste
    /// because CGEvent synthetic keystrokes produce wrong output when an IME is active
    /// (macOS reads virtualKey=0 → 'a' instead of the Unicode string).
    static func typeText(_ text: String) -> (ActionResult?, ErrorInfo?) {
        guard processInputCommitGateV1.canAdmitInput() else {
            return (nil, ErrorInfo(code: -1, message: "input recovery is blocked"))
        }
        let hasNonASCII = text.unicodeScalars.contains { $0.value > 0x7F }

        if hasNonASCII || text.count > 20 {
            // Compatibility-only clipboard path. Clipboard bytes are never
            // journaled, so a fatal crash cannot promise restoration; the Go
            // caller receives commit-unknown when no acknowledgement arrives.
            // While alive, reuse the target-bound transaction's change-count
            // ownership guard so cleanup never overwrites newer user content.
            let pasteboard = NSPasteboard.general
            guard let transaction = makeTargetBoundClipboardTransaction(
                text, pasteboard: pasteboard,
                waitBeforeRestore: { Thread.sleep(forTimeInterval: 0.1) }) else {
                return (nil, ErrorInfo(code: -1, message: "Failed to prepare pasteboard"))
            }
            switch transaction.post() {
            case .ownershipLost:
                return (nil, ErrorInfo(
                    code: -1,
                    message: "clipboard ownership lost before paste; newer content preserved"))
            case .failed:
                let restored = transaction.restore()
                return (nil, ErrorInfo(
                    code: -1,
                    message: restored
                        ? "paste input commit blocked or key release unconfirmed"
                        : "paste failed and clipboard restore is unresolved"))
            case .committed:
                guard transaction.restore() else {
                    return (nil, ErrorInfo(
                        code: -1,
                        message: "paste committed but clipboard restore is unresolved"))
                }
            }
            return (ActionResult(result: "typed text (content redacted)"), nil)
        }

        // Short ASCII text — direct keystroke synthesis
        for char in text {
            let key = String(char).lowercased()
            let needsShift = char.isUppercase || shiftChars.contains(char)

            // For shifted symbols (!@#...), look up the base key
            let baseKey = shiftedCharMap[char] ?? key
            guard let keyCode = keyCodeMap[baseKey] ?? keyCodeMap[String(char)] else {
                // Unknown char — skip
                continue
            }

            var flags: CGEventFlags = []
            if needsShift { flags.insert(.maskShift) }

            guard postKeyPair(keyCode: keyCode, flags: flags) else {
                return (nil, ErrorInfo(
                    code: -1, message: "text input commit blocked or key release unconfirmed"))
            }
            Thread.sleep(forTimeInterval: 0.01)
        }
        return (ActionResult(result: "typed text (content redacted)"), nil)
    }

    static func scroll(dx: Int, dy: Int) {
        guard let event = CGEvent(scrollWheelEvent2Source: nil, units: .pixel,
                                  wheelCount: 2, wheel1: Int32(dy), wheel2: Int32(dx), wheel3: 0) else {
            return
        }
        _ = processInputCommitGateV1.commitSample {
            event.post(tap: .cghidEventTap)
            return true
        }
    }

    private static func mouseConstants(_ button: String) -> (CGMouseButton, CGEventType, CGEventType) {
        switch button.lowercased() {
        case "right":
            return (.right, .rightMouseDown, .rightMouseUp)
        default:
            return (.left, .leftMouseDown, .leftMouseUp)
        }
    }

    private static func postMousePair(
        down: CGEvent,
        up: CGEvent,
        buttonName: String,
        button: CGMouseButton
    ) -> Bool {
        let normalized = buttonName.lowercased() == "right" ? "right" : "left"
        let release = PreparedInputReleaseV1(metadata: .mouse(button: normalized)) {
            up.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.005)
            return !CGEventSource.buttonState(.combinedSessionState, button: button)
        }
        guard let token = processInputCommitGateV1.registerPress(
            release: release,
            commitDown: { down.post(tap: .cghidEventTap); return true }) else { return false }
        return processInputCommitGateV1.confirmRelease(token: token)
    }

    private static func postKeyPair(keyCode: CGKeyCode, flags: CGEventFlags) -> Bool {
        guard let down = CGEvent(
            keyboardEventSource: nil, virtualKey: keyCode, keyDown: true),
              let up = CGEvent(
                keyboardEventSource: nil, virtualKey: keyCode, keyDown: false) else { return false }
        down.flags = flags
        up.flags = flags
        let release = PreparedInputReleaseV1(metadata: .key(
            virtualKey: UInt16(keyCode), eventFlags: flags.rawValue)) {
            up.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.005)
            return !CGEventSource.keyState(.combinedSessionState, key: keyCode)
        }
        guard let token = processInputCommitGateV1.registerPress(
            release: release,
            commitDown: { down.post(tap: .cghidEventTap); return true }) else { return false }
        return processInputCommitGateV1.confirmRelease(token: token)
    }
}

/// Characters that require shift to produce on a US keyboard.
private let shiftChars: Set<Character> = Set("~!@#$%^&*()_+{}|:\"<>?ABCDEFGHIJKLMNOPQRSTUVWXYZ")

/// Maps shifted symbols to their base key for keycode lookup (US keyboard layout).
private let shiftedCharMap: [Character: String] = [
    "~": "`", "!": "1", "@": "2", "#": "3", "$": "4",
    "%": "5", "^": "6", "&": "7", "*": "8", "(": "9",
    ")": "0", "_": "-", "+": "=", "{": "[", "}": "]",
    "|": "\\", ":": ";", "\"": "'", "<": ",", ">": ".",
    "?": "/",
]

let keyCodeMap: [String: CGKeyCode] = [
    // Letters
    "a": 0x00, "b": 0x0B, "c": 0x08, "d": 0x02, "e": 0x0E,
    "f": 0x03, "g": 0x05, "h": 0x04, "i": 0x22, "j": 0x26,
    "k": 0x28, "l": 0x25, "m": 0x2E, "n": 0x2D, "o": 0x1F,
    "p": 0x23, "q": 0x0C, "r": 0x0F, "s": 0x01, "t": 0x11,
    "u": 0x20, "v": 0x09, "w": 0x0D, "x": 0x07, "y": 0x10,
    "z": 0x06,

    // Numbers
    "0": 0x1D, "1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15,
    "5": 0x17, "6": 0x16, "7": 0x1A, "8": 0x1C, "9": 0x19,

    // Special keys
    "return": 0x24, "enter": 0x24, "tab": 0x30, "space": 0x31,
    "delete": 0x33, "backspace": 0x33, "escape": 0x35, "esc": 0x35,

    // Arrow keys
    "left": 0x7B, "right": 0x7C, "down": 0x7D, "up": 0x7E,

    // Function keys
    "f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76,
    "f5": 0x60, "f6": 0x61, "f7": 0x62, "f8": 0x64,
    "f9": 0x65, "f10": 0x6D, "f11": 0x67, "f12": 0x6F,

    // Modifiers (for standalone use)
    "command": 0x37, "shift": 0x38, "option": 0x3A, "control": 0x3B,

    // Punctuation
    "-": 0x1B, "=": 0x18, "[": 0x21, "]": 0x1E,
    "\\": 0x2A, ";": 0x29, "'": 0x27, ",": 0x2B,
    ".": 0x2F, "/": 0x2C, "`": 0x32,

    // Navigation
    "home": 0x73, "end": 0x77, "pageup": 0x74, "pagedown": 0x79,
    "forwarddelete": 0x75,
]
