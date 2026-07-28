import AppKit

struct AppIdentityResult: Encodable {
    let app: String
    let bundleID: String
    let pid: Int?

    enum CodingKeys: String, CodingKey {
        case app, pid
        case bundleID = "bundle_id"
    }
}

struct BackgroundAppBindingResult: Encodable {
    let app: String
    let bundleID: String
    let pid: Int
    let launchDate: String?
    let preservedFrontmostPID: Int
    let preservedFrontmostBundleID: String
    let preservedFrontmostLaunchDate: String?

    enum CodingKeys: String, CodingKey {
        case app, pid
        case bundleID = "bundle_id"
        case launchDate = "launch_date"
        case preservedFrontmostPID = "preserved_frontmost_pid"
        case preservedFrontmostBundleID = "preserved_frontmost_bundle_id"
        case preservedFrontmostLaunchDate = "preserved_frontmost_launch_date"
    }
}

private let processLaunchDateFormatterV1: ISO8601DateFormatter = {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter
}()

func processLaunchDateStringV1(_ application: NSRunningApplication) -> String? {
    application.launchDate.map(processLaunchDateFormatterV1.string(from:))
}

struct FocusManager {
    static func resolveAppIdentity(
        appName: String,
        excludedPIDs: Set<Int> = []
    ) -> (AppIdentityResult?, ErrorInfo?) {
        let resolution = runningApplicationSelection(
            appName: appName,
            excluding: excludedPIDs)
        if case let .selected(pid) = resolution.selection,
           let running = resolution.applications.first(where: {
               Int($0.processIdentifier) == pid
           }),
           let bundleID = running.bundleIdentifier,
           !bundleID.isEmpty {
            return (
                AppIdentityResult(
                    app: running.localizedName ?? appName,
                    bundleID: bundleID,
                    pid: Int(running.processIdentifier)),
                nil)
        }
        if resolution.selection == .ambiguous {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' has multiple running instances and no unique visible target"))
        }
        guard let path = NSWorkspace.shared.fullPath(forApplication: appName),
              let bundle = Bundle(path: path),
              let bundleID = bundle.bundleIdentifier,
              !bundleID.isEmpty else {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' is not installed"))
        }
        return (
            AppIdentityResult(
                app: bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String ??
                    bundle.object(forInfoDictionaryKey: "CFBundleName") as? String ??
                    appName,
                bundleID: bundleID,
                pid: nil),
            nil)
    }

    /// Prepares exactly one task-owned application instance. A resolved PID is
    /// immutable: if it disappeared or changed bundle identity, preparation
    /// fails instead of falling back to another same-name process. When no PID
    /// existed at resolution time, launch creates one fresh application
    /// instance and returns the exact PID for every later observation/action.
    static func prepareTaskApp(
        appName: String,
        expectedBundleID: String,
        expectedPID: Int?,
        excludedPIDs: Set<Int>
    ) -> (AppIdentityResult?, ErrorInfo?) {
        guard !expectedBundleID.isEmpty else {
            return (nil, ErrorInfo(
                code: -1,
                message: "Task app bundle identity is required"))
        }

        var target: NSRunningApplication?
        if let expectedPID, expectedPID > 0 {
            guard !excludedPIDs.contains(expectedPID),
                  let processID = pid_t(exactly: expectedPID),
                  let running = NSRunningApplication(processIdentifier: processID),
                  !running.isTerminated,
                  running.bundleIdentifier == expectedBundleID else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "Resolved task app identity is no longer available"))
            }
            target = running
        } else {
            let resolution = runningApplicationSelection(
                appName: appName,
                excluding: excludedPIDs)
            switch resolution.selection {
            case let .selected(pid):
                target = resolution.applications.first {
                    Int($0.processIdentifier) == pid
                }
            case .ambiguous:
                return (nil, ErrorInfo(
                    code: -1,
                    message: "App '\(appName)' has multiple running instances and no unique visible target"))
            case .notRunning:
                break
            }
        }

        if target == nil {
            let applicationURL = NSWorkspace.shared.urlForApplication(
                withBundleIdentifier: expectedBundleID)
            guard let applicationURL else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "App '\(appName)' is not installed"))
            }
            let configuration = NSWorkspace.OpenConfiguration()
            configuration.activates = true
            configuration.createsNewApplicationInstance = true
            NSWorkspace.shared.openApplication(
                at: applicationURL,
                configuration: configuration
            ) { _, _ in }

            let deadline = Date().addingTimeInterval(10)
            while Date() < deadline {
                let resolution = runningApplicationSelection(
                    appName: appName,
                    excluding: excludedPIDs)
                switch resolution.selection {
                case let .selected(pid):
                    target = resolution.applications.first {
                        Int($0.processIdentifier) == pid
                    }
                case .ambiguous:
                    return (nil, ErrorInfo(
                        code: -1,
                        message: "App '\(appName)' launched into an ambiguous instance set"))
                case .notRunning:
                    break
                }
                if target != nil { break }
                Thread.sleep(forTimeInterval: 0.1)
            }
        }

        guard let target,
              !target.isTerminated,
              target.bundleIdentifier == expectedBundleID else {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' did not expose the expected process identity"))
        }
        guard activateAndExposeWindow(target, timeout: 2) else {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' did not expose a window"))
        }
        return (
            AppIdentityResult(
                app: target.localizedName ?? appName,
                bundleID: expectedBundleID,
                pid: Int(target.processIdentifier)),
            nil)
    }

    /// Binds one already-running, visible, non-frontmost app without
    /// activating, raising, unminimizing, reopening, or launching it. The
    /// caller may fall back to prepareTaskApp with this same exact identity,
    /// but this method itself is observation-only.
    static func bindBackgroundTaskApp(
        appName: String,
        expectedBundleID: String,
        expectedPID: Int?,
        excludedPIDs: Set<Int>
    ) -> (BackgroundAppBindingResult?, ErrorInfo?) {
        guard !expectedBundleID.isEmpty,
              let expectedPID,
              expectedPID > 0,
              !excludedPIDs.contains(expectedPID),
              let processID = pid_t(exactly: expectedPID) else {
            return (nil, ErrorInfo(
                code: -1,
                message: "Background task app requires one exact running identity"))
        }
        refreshAppKitState()
        guard let target = NSRunningApplication(processIdentifier: processID),
              !target.isTerminated,
              target.bundleIdentifier == expectedBundleID else {
            return (nil, ErrorInfo(
                code: -1,
                message: "Resolved background task app identity is no longer available"))
        }
        let candidates = currentCGWindowIdentityCandidates()
        let visibleNormalWindowPIDs = candidates.compactMap { candidate -> Int? in
            guard candidate.layer == 0,
                  candidate.windowID > 0,
                  candidate.isOnScreen,
                  candidate.alpha > 0,
                  candidate.frame.width > 0,
                  candidate.frame.height > 0 else {
                return nil
            }
            return candidate.ownerPID
        }
        guard let frontmost = NSWorkspace.shared.frontmostApplication,
              !frontmost.isTerminated,
              let frontmostBundleID = frontmost.bundleIdentifier,
              !frontmostBundleID.isEmpty else {
            return (nil, ErrorInfo(
                code: -1,
                message: "Background task requires one exact preserved frontmost process"))
        }
        let frontmostPID = Int(frontmost.processIdentifier)
        switch backgroundTaskAppEligibility(
            targetPID: expectedPID,
            frontmostPID: frontmostPID,
            visibleNormalWindowPIDs: visibleNormalWindowPIDs
        ) {
        case .eligible:
            return (
                BackgroundAppBindingResult(
                    app: target.localizedName ?? appName,
                    bundleID: expectedBundleID,
                    pid: expectedPID,
                    launchDate: processLaunchDateStringV1(target),
                    preservedFrontmostPID: frontmostPID,
                    preservedFrontmostBundleID: frontmostBundleID,
                    preservedFrontmostLaunchDate: processLaunchDateStringV1(frontmost)),
                nil)
        case .targetIsFrontmost:
            return (nil, ErrorInfo(
                code: -1,
                message: "Background task target is already frontmost"))
        case .noVisibleWindow:
            return (nil, ErrorInfo(
                code: -1,
                message: "Background task target has no visible normal window"))
        }
    }

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
    static func focusApp(
        appName: String,
        windowTitle: String?,
        verify: Bool,
        expectedPID: Int? = nil,
        expectedBundleID: String? = nil,
        excludedPIDs: Set<Int> = []
    ) -> (ActionResult?, ErrorInfo?) {
        let app: NSRunningApplication?
        if let expectedPID, expectedPID > 0 {
            if !excludedPIDs.contains(expectedPID),
               let processID = pid_t(exactly: expectedPID),
               let exact = NSRunningApplication(processIdentifier: processID),
               !exact.isTerminated,
               expectedBundleID == nil ||
                    exact.bundleIdentifier == expectedBundleID {
                app = exact
            } else {
                app = nil
            }
        } else {
            app = resolveRunningApplication(
                appName: appName,
                excluding: excludedPIDs)
        }
        guard let app else {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' exact target is not available"))
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
            refreshAppKitState()
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
        refreshAppKitState()
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
        if !axWindows(appRef).isEmpty {
            return true
        }
        return frontmostNormalCGWindow(
            pid: Int(app.processIdentifier),
            candidates: currentCGWindowIdentityCandidates()
        ) != nil
    }
}
