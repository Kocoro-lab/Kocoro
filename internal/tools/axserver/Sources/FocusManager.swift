import AppKit
import CoreServices

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

func backgroundTaskAppLaunchConfigurationV1() -> NSWorkspace.OpenConfiguration {
    let configuration = NSWorkspace.OpenConfiguration()
    configuration.activates = false
    configuration.createsNewApplicationInstance = true
    return configuration
}

func foregroundTaskAppLaunchConfigurationV1() -> NSWorkspace.OpenConfiguration {
    let configuration = NSWorkspace.OpenConfiguration()
    configuration.activates = true
    configuration.createsNewApplicationInstance = true
    return configuration
}

func backgroundTaskAppWindowReadyTimeoutV1() -> TimeInterval {
    2
}

func foregroundTaskAppWindowReadyTimeoutV1() -> TimeInterval {
    5
}

func foregroundTaskAppHasCapturableWindowV1(
    targetPID: Int,
    candidates: [CGWindowIdentityCandidate]
) -> Bool {
    frontmostNormalCGWindow(
        pid: targetPID,
        candidates: candidates
    ) != nil
}

enum ForegroundTaskAppPreparationDecisionV1: Equatable {
    case reuseExact(Int)
    case launchExact
}

func foregroundTaskAppPreparationDecision(
    _ selection: RunningApplicationSelection
) -> ForegroundTaskAppPreparationDecisionV1 {
    switch selection {
    case let .selected(pid):
        return .reuseExact(pid)
    case .ambiguous, .notRunning:
        return .launchExact
    }
}

func foregroundTaskAppReuseMatchesExpectedBundleV1(
    candidateBundleID: String?,
    expectedBundleID: String
) -> Bool {
    candidateBundleID == expectedBundleID
}

func taskAppIdentityNeedsInstalledLookupV1(
    _ selection: RunningApplicationSelection
) -> Bool {
    if case .selected = selection { return false }
    return true
}

private final class TaskAppLaunchReceiptV1: @unchecked Sendable {
    private let lock = NSLock()
    private var application: NSRunningApplication?
    private var error: Error?

    func store(application: NSRunningApplication?, error: Error?) {
        lock.lock()
        self.application = application
        self.error = error
        lock.unlock()
    }

    func load() -> (NSRunningApplication?, Error?) {
        lock.lock()
        defer { lock.unlock() }
        return (application, error)
    }
}

func processLaunchDateStringV1(_ application: NSRunningApplication) -> String? {
    application.launchDate.map(processLaunchDateFormatterV1.string(from:))
}

func metadataQueryLiteralV1(_ value: String) -> String {
    value
        .replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"")
        .replacingOccurrences(of: "*", with: "\\*")
        .replacingOccurrences(of: "?", with: "\\?")
}

enum InstalledTaskApplicationLookupResultV1: Equatable {
    case resolved(URL)
    case notFound
    case ambiguous
    case timedOut
    case unavailable
}

enum MetadataApplicationQueryResultV1: Equatable {
    case candidates([URL])
    case timedOut
    case unavailable
}

func installedTaskApplicationSearchScopesV1(
    fileManager: FileManager = .default
) -> [URL] {
    let domains: [FileManager.SearchPathDomainMask] = [
        .userDomainMask,
        .localDomainMask,
        .systemDomainMask,
    ]
    var candidates = domains.flatMap {
        fileManager.urls(for: .applicationDirectory, in: $0)
    }
    candidates.append(URL(
        fileURLWithPath: "/System/Library/CoreServices/Applications",
        isDirectory: true))

    var scopesByPath: [String: URL] = [:]
    for candidate in candidates {
        let normalized = candidate.resolvingSymlinksInPath().standardizedFileURL
        guard let resourceValues = try? normalized.resourceValues(forKeys: [
            .isDirectoryKey,
            .volumeIsLocalKey,
        ]),
            resourceValues.isDirectory == true,
            resourceValues.volumeIsLocal == true else {
            continue
        }
        scopesByPath[normalized.path] = normalized
    }
    return scopesByPath.values.sorted { $0.path < $1.path }
}

func uniqueInstalledTaskApplicationURLV1(
    candidates: [URL],
    bundleIdentifier: (URL) -> String? = { Bundle(url: $0)?.bundleIdentifier }
) -> InstalledTaskApplicationLookupResultV1 {
    var validByPath: [String: URL] = [:]
    for candidate in candidates {
        guard candidate.isFileURL else { continue }
        let normalized = candidate.resolvingSymlinksInPath().standardizedFileURL
        guard normalized.pathExtension.caseInsensitiveCompare("app") == .orderedSame,
              let identifier = bundleIdentifier(normalized)?
                .trimmingCharacters(in: .whitespacesAndNewlines),
              !identifier.isEmpty else {
            continue
        }
        validByPath[normalized.path] = normalized
    }
    if validByPath.isEmpty { return .notFound }
    guard validByPath.count == 1, let applicationURL = validByPath.values.first else {
        return .ambiguous
    }
    return .resolved(applicationURL)
}

typealias MetadataApplicationQueryRunnerV1 = (
    _ predicate: String,
    _ scopes: [URL],
    _ timeout: TimeInterval
) -> MetadataApplicationQueryResultV1

private func metadataApplicationQueryV1(
    predicate: String,
    scopes: [URL],
    timeout: TimeInterval
) -> MetadataApplicationQueryResultV1 {
    guard !scopes.isEmpty,
          let query = MDQueryCreate(
            kCFAllocatorDefault,
            predicate as CFString,
            nil,
            nil) else {
        return .unavailable
    }
    let deliveryQueue = DispatchQueue(
        label: "run.shannon.kocoro.ax-server.app-metadata-query",
        qos: .userInitiated)
    MDQuerySetSearchScope(query, scopes as CFArray, 0)
    MDQuerySetDispatchQueue(query, deliveryQueue)
    guard MDQueryExecute(query, 0) else { return .unavailable }

    let deadline = DispatchTime.now() + max(timeout, 0)
    while !MDQueryIsGatheringComplete(query) {
        guard DispatchTime.now().uptimeNanoseconds < deadline.uptimeNanoseconds else {
            deliveryQueue.async { MDQueryStop(query) }
            return .timedOut
        }
        Thread.sleep(forTimeInterval: 0.01)
    }
    let candidates: [URL] = deliveryQueue.sync {
        (0..<MDQueryGetResultCount(query)).compactMap { index in
            guard let rawItem = MDQueryGetResultAtIndex(query, index) else {
                return nil
            }
            let item = unsafeBitCast(rawItem, to: MDItem.self)
            guard let path = MDItemCopyAttribute(item, kMDItemPath) as? String,
                  !path.isEmpty else {
                return nil
            }
            return URL(fileURLWithPath: path)
        }
    }
    return .candidates(candidates)
}

// Exact display-name metadata queries are limited to standard local app roots.
// This bound keeps the ax_server request loop responsive if Spotlight stalls;
// tests and any future broader caller can supply a different timeout.
func installedTaskApplicationMetadataQueryTimeoutV1() -> TimeInterval {
    2
}

func localizedInstalledTaskApplicationLookupV1(
    appName: String,
    timeout: TimeInterval = installedTaskApplicationMetadataQueryTimeoutV1(),
    queryRunner: MetadataApplicationQueryRunnerV1 = metadataApplicationQueryV1
) -> InstalledTaskApplicationLookupResultV1 {
    let requested = appName.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !requested.isEmpty else { return .notFound }
    let literal = metadataQueryLiteralV1(requested)
    let predicate =
        "kMDItemDisplayName == \"\(literal)\"c && " +
        "kMDItemContentType == \"com.apple.application-bundle\""
    switch queryRunner(
        predicate,
        installedTaskApplicationSearchScopesV1(),
        timeout
    ) {
    case let .candidates(candidates):
        return uniqueInstalledTaskApplicationURLV1(candidates: candidates)
    case .timedOut:
        return .timedOut
    case .unavailable:
        return .unavailable
    }
}

func installedTaskApplicationLookupErrorMessageV1(
    appName: String,
    result: InstalledTaskApplicationLookupResultV1
) -> String? {
    switch result {
    case .resolved:
        return nil
    case .notFound:
        return "App '\(appName)' is not installed"
    case .ambiguous:
        return "Multiple installed applications exactly match '\(appName)'; use the bundle identifier"
    case .timedOut:
        return "Installed application lookup timed out for '\(appName)'; use the bundle identifier"
    case .unavailable:
        return "Installed application lookup is unavailable for '\(appName)'; use the bundle identifier"
    }
}

private func installedTaskApplicationLookupV1(
    appName: String,
    expectedBundleID: String? = nil
) -> InstalledTaskApplicationLookupResultV1 {
    if let expectedBundleID, !expectedBundleID.isEmpty,
       let url = NSWorkspace.shared.urlForApplication(
           withBundleIdentifier: expectedBundleID) {
        return .resolved(url)
    }
    let requested = appName.trimmingCharacters(in: .whitespacesAndNewlines)
    if !requested.isEmpty,
       let url = NSWorkspace.shared.urlForApplication(
           withBundleIdentifier: requested) {
        return .resolved(url)
    }
    if let path = NSWorkspace.shared.fullPath(forApplication: requested) {
        return .resolved(URL(fileURLWithPath: path))
    }
    return localizedInstalledTaskApplicationLookupV1(appName: requested)
}

private func openExactTaskApplicationV1(
    at applicationURL: URL,
    configuration: NSWorkspace.OpenConfiguration,
    timeout: TimeInterval
) -> (NSRunningApplication?, Error?) {
    let receipt = TaskAppLaunchReceiptV1()
    let completed = DispatchSemaphore(value: 0)
    NSWorkspace.shared.openApplication(
        at: applicationURL,
        configuration: configuration
    ) { application, error in
        receipt.store(application: application, error: error)
        completed.signal()
    }
    guard completed.wait(timeout: .now() + timeout) == .success else {
        return (nil, NSError(
            domain: "run.shannon.kocoro.ax-server.task-launch",
            code: 1,
            userInfo: [NSLocalizedDescriptionKey:
                "LaunchServices did not return an exact application receipt"]))
    }
    return receipt.load()
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
        guard taskAppIdentityNeedsInstalledLookupV1(resolution.selection) else {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' is not installed"))
        }
        let lookup = installedTaskApplicationLookupV1(appName: appName)
        guard case let .resolved(applicationURL) = lookup else {
            return (nil, ErrorInfo(
                code: -1,
                message: installedTaskApplicationLookupErrorMessageV1(
                    appName: appName,
                    result: lookup) ?? "App '\(appName)' is not installed"))
        }
        guard let bundle = Bundle(url: applicationURL),
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
            switch foregroundTaskAppPreparationDecision(resolution.selection) {
            case let .reuseExact(pid):
                let candidate = resolution.applications.first {
                    Int($0.processIdentifier) == pid
                }
                if foregroundTaskAppReuseMatchesExpectedBundleV1(
                    candidateBundleID: candidate?.bundleIdentifier,
                    expectedBundleID: expectedBundleID
                ) {
                    target = candidate
                }
            case .launchExact:
                break
            }
        }

        if target == nil {
            let lookup = installedTaskApplicationLookupV1(
                appName: appName,
                expectedBundleID: expectedBundleID)
            guard case let .resolved(applicationURL) = lookup else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: installedTaskApplicationLookupErrorMessageV1(
                        appName: appName,
                        result: lookup) ?? "App '\(appName)' is not installed"))
            }
            let (launched, launchError) = openExactTaskApplicationV1(
                at: applicationURL,
                configuration: foregroundTaskAppLaunchConfigurationV1(),
                timeout: 10)
            guard launchError == nil,
                  let launched,
                  !launched.isTerminated,
                  launched.bundleIdentifier == expectedBundleID,
                  !excludedPIDs.contains(Int(launched.processIdentifier)) else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "App '\(appName)' could not establish an exact foreground process identity"))
            }
            target = launched
        }

        guard let target,
              !target.isTerminated,
              target.bundleIdentifier == expectedBundleID else {
            return (nil, ErrorInfo(
                code: -1,
                message: "App '\(appName)' did not expose the expected process identity"))
        }
        guard activateAndExposeWindow(
            target,
            timeout: foregroundTaskAppWindowReadyTimeoutV1()
        ) else {
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

    /// Binds one visible, non-frontmost app without activating, raising,
    /// unminimizing, or reopening it. If no process identity was resolved,
    /// LaunchServices may start one exact instance with `activates = false`.
    /// There is no foreground fallback.
    static func bindBackgroundTaskApp(
        appName: String,
        expectedBundleID: String,
        expectedPID: Int?,
        excludedPIDs: Set<Int>
    ) -> (BackgroundAppBindingResult?, ErrorInfo?) {
        guard !expectedBundleID.isEmpty else {
            return (nil, ErrorInfo(
                code: -1,
                message: "Background task app bundle identity is required"))
        }

        refreshAppKitState()
        let target: NSRunningApplication
        if let expectedPID {
            guard expectedPID > 0,
                  !excludedPIDs.contains(expectedPID),
                  let processID = pid_t(exactly: expectedPID),
                  let running = NSRunningApplication(processIdentifier: processID),
                  !running.isTerminated,
                  running.bundleIdentifier == expectedBundleID else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "Resolved background task app identity is no longer available"))
            }
            target = running
        } else {
            guard let applicationURL = NSWorkspace.shared.urlForApplication(
                withBundleIdentifier: expectedBundleID) else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "App '\(appName)' is not installed"))
            }
            let (launched, launchError) = openExactTaskApplicationV1(
                at: applicationURL,
                configuration: backgroundTaskAppLaunchConfigurationV1(),
                timeout: 10)
            guard launchError == nil,
                  let launched,
                  !launched.isTerminated,
                  launched.bundleIdentifier == expectedBundleID,
                  !excludedPIDs.contains(Int(launched.processIdentifier)) else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "App '\(appName)' could not establish an exact background process identity"))
            }
            target = launched
        }

        let targetPID = Int(target.processIdentifier)
        let deadline = Date().addingTimeInterval(
            expectedPID == nil
                ? 10
                : backgroundTaskAppWindowReadyTimeoutV1())
        while true {
            refreshAppKitState()
            guard let frontmost = NSWorkspace.shared.frontmostApplication,
                  !frontmost.isTerminated,
                  let frontmostBundleID = frontmost.bundleIdentifier,
                  !frontmostBundleID.isEmpty else {
                return (nil, ErrorInfo(
                    code: -1,
                    message: "Background task requires one exact initial foreground witness"))
            }
            let frontmostPID = Int(frontmost.processIdentifier)
            let visibleNormalWindowPIDs = currentCGWindowIdentityCandidates()
                .compactMap { candidate -> Int? in
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
            switch backgroundTaskAppEligibility(
                targetPID: targetPID,
                frontmostPID: frontmostPID,
                visibleNormalWindowPIDs: visibleNormalWindowPIDs
            ) {
            case .eligible:
                return (
                    BackgroundAppBindingResult(
                        app: target.localizedName ?? appName,
                        bundleID: expectedBundleID,
                        pid: targetPID,
                        launchDate: processLaunchDateStringV1(target),
                        preservedFrontmostPID: frontmostPID,
                        preservedFrontmostBundleID: frontmostBundleID,
                        preservedFrontmostLaunchDate: processLaunchDateStringV1(frontmost)),
                    nil)
            case .targetIsFrontmost:
                return (nil, ErrorInfo(
                    code: -1,
                    message: "Background task launch activated the target app"))
            case .noVisibleWindow:
                if Date() >= deadline {
                    return (nil, ErrorInfo(
                        code: -1,
                        message: "Background task target has no visible normal window"))
                }
                Thread.sleep(forTimeInterval: 0.1)
            }
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
    /// window when the app is alive but has no visible normal WindowServer
    /// surface. AXWindows alone is not sufficient: browsers and menu-bar apps
    /// may expose hidden or synthetic AX windows while still having nothing
    /// that the screenshot contract can capture. This is app-agnostic and
    /// mirrors clicking an app's Dock icon without sending Apple Events (and
    /// therefore without introducing an Automation-TCC dependency).
    private static func activateAndExposeWindow(_ app: NSRunningApplication, timeout: TimeInterval) -> Bool {
        app.unhide()
        _ = app.activate(options: [.activateAllWindows])

        if hasCapturableWindow(app) {
            return true
        }

        if let bundleURL = app.bundleURL {
            let configuration = NSWorkspace.OpenConfiguration()
            configuration.activates = true
            NSWorkspace.shared.openApplication(at: bundleURL, configuration: configuration) { _, _ in }
        }

        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if hasCapturableWindow(app) {
                _ = app.activate(options: [.activateAllWindows])
                return true
            }
            Thread.sleep(forTimeInterval: 0.1)
        }
        return false
    }

    private static func hasCapturableWindow(
        _ app: NSRunningApplication
    ) -> Bool {
        foregroundTaskAppHasCapturableWindowV1(
            targetPID: Int(app.processIdentifier),
            candidates: currentCGWindowIdentityCandidates())
    }
}
