import AppKit

struct FocusManager {
    /// Launches an installed app by display name and activates it once the
    /// process becomes visible to NSWorkspace.
    static func launchApp(appName: String) -> (ActionResult?, ErrorInfo?) {
        if let running = resolveRunningApplication(appName: appName) {
            guard activateAndExposeWindow(running, timeout: 2) else {
                return (nil, ErrorInfo(code: -1, message: "App '\(appName)' is running but did not expose a window after a reopen request"))
            }
            let pid = Int(running.processIdentifier)
            return (ActionResult(result: "focused already-running \(appName) (pid \(pid))"), nil)
        }

        // This name-based API is deprecated, but it remains the only general
        // launch-by-display-name API. Once the process exists, all activation
        // and reopen work below uses the modern URL/configuration path.
        guard NSWorkspace.shared.launchApplication(appName) else {
            return (nil, ErrorInfo(code: -1, message: "App '\(appName)' is not installed or could not be launched"))
        }

        // Normal GUI apps register with NSWorkspace within a fraction of a
        // second, while large creative apps can take several seconds. Bound
        // the serial ax_server request at 10s so a stuck launch cannot wedge
        // every later GUI call. Callers can recover by using wait/focus or by
        // retrying launch_app after the app finishes its own startup work.
        let deadline = Date().addingTimeInterval(10)
        while Date() < deadline {
            if let launched = resolveRunningApplication(appName: appName) {
                guard activateAndExposeWindow(launched, timeout: max(0.1, deadline.timeIntervalSinceNow)) else {
                    return (nil, ErrorInfo(code: -1, message: "App '\(appName)' launched but did not expose a window"))
                }
                let pid = Int(launched.processIdentifier)
                return (ActionResult(result: "launched \(appName) (pid \(pid))"), nil)
            }
            Thread.sleep(forTimeInterval: 0.1)
        }
        return (nil, ErrorInfo(code: -1, message: "App '\(appName)' did not register within 10 seconds"))
    }

    /// Activates an app by name, optionally verifying focus.
    static func focusApp(appName: String, windowTitle: String?, verify: Bool) -> (ActionResult?, ErrorInfo?) {
        guard let app = resolveRunningApplication(appName: appName) else {
            return (nil, ErrorInfo(code: -1, message: "App '\(appName)' not found or not running"))
        }

        guard activateAndExposeWindow(app, timeout: 2) else {
            return (nil, ErrorInfo(code: -1, message: "App '\(appName)' is running but did not expose a window after a reopen request"))
        }

        if let requestedTitle = windowTitle, !requestedTitle.isEmpty {
            let appRef = AXUIElementCreateApplication(app.processIdentifier)
            let windows = axWindows(appRef)
            guard let requestedWindow = windows.first(where: {
                (axString($0, "AXTitle") ?? "").localizedCaseInsensitiveContains(requestedTitle)
            }) else {
                return (nil, ErrorInfo(code: -1, message: "No window containing '\(requestedTitle)' found in '\(appName)'"))
            }
            AXUIElementSetAttributeValue(requestedWindow, "AXMinimized" as CFString, false as CFTypeRef)
            AXUIElementPerformAction(requestedWindow, "AXRaise" as CFString)
        }

        if verify {
            // Brief wait for activation
            Thread.sleep(forTimeInterval: 0.3)
            guard let frontmost = NSWorkspace.shared.frontmostApplication,
                  frontmost.processIdentifier == app.processIdentifier else {
                return (nil, ErrorInfo(code: -1, message: "Failed to bring '\(appName)' to front"))
            }
        }

        let pid = Int(app.processIdentifier)
        return (ActionResult(result: "focused \(appName) (pid \(pid))"), nil)
    }

    /// Returns the frontmost app's PID and window title.
    static func frontmost() -> (ActionResult?, ErrorInfo?) {
        guard let app = NSWorkspace.shared.frontmostApplication else {
            return (nil, ErrorInfo(code: -1, message: "Cannot determine frontmost application"))
        }
        let name = app.localizedName ?? "Unknown"
        let pid = Int(app.processIdentifier)

        // Get window title via AX
        let appRef = AXUIElementCreateApplication(Int32(pid))
        var windowTitle = ""
        if let win = axWindows(appRef).first {
            windowTitle = axString(win, "AXTitle") ?? ""
        }

        struct FrontmostResult: Encodable {
            let app: String
            let pid: Int
            let window: String
        }
        // Return as simple action result with details
        return (ActionResult(result: "\(name) (pid \(pid), window: \(windowTitle))"), nil)
    }

    /// Lists all windows for an app.
    static func listWindows(pid: Int) -> [[String: String]] {
        let appRef = AXUIElementCreateApplication(Int32(pid))
        let windows = axWindows(appRef)
        var result: [[String: String]] = []
        for (i, win) in windows.enumerated() {
            let title = axString(win, "AXTitle") ?? ""
            let role = axString(win, "AXRole") ?? ""
            result.append(["index": "\(i)", "title": title, "role": role])
        }
        return result
    }

    /// Activates every existing window, or asks LaunchServices to reopen a
    /// window when the app is alive but windowless. This is app-agnostic and
    /// mirrors clicking an app's Dock icon without sending Apple Events (and
    /// therefore without introducing an Automation-TCC dependency).
    private static func activateAndExposeWindow(_ app: NSRunningApplication, timeout: TimeInterval) -> Bool {
        app.unhide()
        _ = app.activate(options: [.activateAllWindows])

        if hasWindow(app) {
            return true
        }

        if let bundleURL = app.bundleURL {
            let configuration = NSWorkspace.OpenConfiguration()
            configuration.activates = true
            NSWorkspace.shared.openApplication(at: bundleURL, configuration: configuration) { _, _ in }
        }

        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if hasWindow(app) {
                _ = app.activate(options: [.activateAllWindows])
                return true
            }
            Thread.sleep(forTimeInterval: 0.1)
        }
        return false
    }

    private static func hasWindow(_ app: NSRunningApplication) -> Bool {
        let appRef = AXUIElementCreateApplication(app.processIdentifier)
        return !axWindows(appRef).isEmpty
    }
}
