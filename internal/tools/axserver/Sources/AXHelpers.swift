import ApplicationServices
import AppKit

func axValue(_ el: AXUIElement, _ attr: String) -> CFTypeRef? {
    var val: CFTypeRef?
    let err = AXUIElementCopyAttributeValue(el, attr as CFString, &val)
    return err == .success ? val : nil
}

func axString(_ el: AXUIElement, _ attr: String) -> String? {
    axValue(el, attr) as? String
}

func axBool(_ el: AXUIElement, _ attr: String) -> Bool? {
    guard let val = axValue(el, attr) else { return nil }
    if let num = val as? NSNumber { return num.boolValue }
    return nil
}

func axChildren(_ el: AXUIElement) -> [AXUIElement]? {
    axValue(el, "AXChildren") as? [AXUIElement]
}

func axActions(_ el: AXUIElement) -> [String] {
    var names: CFArray?
    guard AXUIElementCopyActionNames(el, &names) == .success,
          let actions = names as? [String] else {
        return []
    }
    return actions.sorted()
}

func axFocusedElement(_ appRef: AXUIElement) -> AXUIElement? {
    guard let value = axValue(appRef, "AXFocusedUIElement"),
          CFGetTypeID(value) == AXUIElementGetTypeID() else {
        return nil
    }
    return (value as! AXUIElement)
}

func axElementsEqual(_ lhs: AXUIElement, _ rhs: AXUIElement?) -> Bool {
    guard let rhs else { return false }
    return CFEqual(lhs, rhs)
}

/// Returns an app's AX windows with conservative fallbacks for frameworks
/// that omit AXWindows while still exposing AXFocusedWindow or window-role
/// children. Every caller uses the same ordering so generated ref paths remain
/// stable between observation and stale-state preflight.
func orderAXWindows(
    _ windows: [AXUIElement],
    focusedWindow: AXUIElement?
) -> [AXUIElement] {
    guard let focusedWindow else { return windows }
    return [focusedWindow] + windows.filter { !CFEqual($0, focusedWindow) }
}

func axWindows(_ appRef: AXUIElement) -> [AXUIElement] {
    let focusedWindow: AXUIElement?
    if let focused = axValue(appRef, "AXFocusedWindow"),
       CFGetTypeID(focused) == AXUIElementGetTypeID() {
        focusedWindow = (focused as! AXUIElement)
    } else {
        focusedWindow = nil
    }
    if let windows = axValue(appRef, "AXWindows") as? [AXUIElement], !windows.isEmpty {
        // macOS may prepend a transient screen-sharing control window ahead
        // of the app's real focused window. All observation and action paths
        // go through this function, so focused-first remains deterministic
        // while avoiding a tiny overlay becoming window[0].
        return orderAXWindows(windows, focusedWindow: focusedWindow)
    }
    if let focusedWindow {
        return [focusedWindow]
    }
    return (axChildren(appRef) ?? []).filter { axString($0, "AXRole") == "AXWindow" }
}

/// Resolves an element by path (e.g. "window[0]/AXButton[2]/AXStaticText[0]").
func resolveElement(pid: Int, path: String) -> AXUIElement? {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let windows = axWindows(appRef)

    let allParts = path.split(separator: "/")
    guard !allParts.isEmpty else { return nil }

    // Parse window index from "window[N]"
    let winPart = allParts[0]
    var winIndex = 0
    if let bracketStart = winPart.firstIndex(of: "["),
       let bracketEnd = winPart.firstIndex(of: "]") {
        winIndex = Int(winPart[winPart.index(after: bracketStart)..<bracketEnd]) ?? 0
    }
    guard winIndex >= 0 && winIndex < windows.count else { return nil }

    let parts = allParts.dropFirst()
    var current: AXUIElement = windows[winIndex]

    for part in parts {
        guard let bracketStart = part.firstIndex(of: "["),
              let bracketEnd = part.firstIndex(of: "]") else { return nil }
        let role = String(part[part.startIndex..<bracketStart])
        guard let index = Int(part[part.index(after: bracketStart)..<bracketEnd]) else { return nil }

        guard let children = axChildren(current) else { return nil }
        var roleCount = 0
        var found = false
        for child in children {
            if axString(child, "AXRole") == role {
                if roleCount == index {
                    current = child
                    found = true
                    break
                }
                roleCount += 1
            }
        }
        if !found { return nil }
    }
    return current
}

/// Resolves a read_tree path inside an already identity-verified window.
/// Typed observations currently expose only window[0], so any other root is
/// rejected instead of silently selecting a different live window.
func resolveElement(in window: AXUIElement, path: String) -> AXUIElement? {
    let allParts = path.split(separator: "/")
    guard allParts.first == "window[0]" else { return nil }
    var current = window
    for part in allParts.dropFirst() {
        guard let bracketStart = part.firstIndex(of: "["),
              let bracketEnd = part.firstIndex(of: "]"),
              let index = Int(part[part.index(after: bracketStart)..<bracketEnd]) else {
            return nil
        }
        let role = String(part[part.startIndex..<bracketStart])
        let matching = (axChildren(current) ?? []).filter { axString($0, "AXRole") == role }
        guard index >= 0 && index < matching.count else { return nil }
        current = matching[index]
    }
    return current
}

/// Returns the center coordinates (screen space) of an AXUIElement, or nil if position/size unavailable.
func elementCenter(_ el: AXUIElement) -> (Double, Double)? {
    var posVal: CFTypeRef?
    var sizeVal: CFTypeRef?
    guard AXUIElementCopyAttributeValue(el, "AXPosition" as CFString, &posVal) == .success,
          AXUIElementCopyAttributeValue(el, "AXSize" as CFString, &sizeVal) == .success else {
        return nil
    }
    var point = CGPoint.zero
    var size = CGSize.zero
    AXValueGetValue(posVal as! AXValue, .cgPoint, &point)
    AXValueGetValue(sizeVal as! AXValue, .cgSize, &size)
    return (Double(point.x + size.width / 2), Double(point.y + size.height / 2))
}

/// Returns the frame (origin + size) of an AXUIElement in screen coordinates, or nil if unavailable.
func elementFrame(_ el: AXUIElement) -> (x: Double, y: Double, width: Double, height: Double)? {
    var posVal: CFTypeRef?
    var sizeVal: CFTypeRef?
    guard AXUIElementCopyAttributeValue(el, "AXPosition" as CFString, &posVal) == .success,
          AXUIElementCopyAttributeValue(el, "AXSize" as CFString, &sizeVal) == .success else {
        return nil
    }
    var point = CGPoint.zero
    var size = CGSize.zero
    AXValueGetValue(posVal as! AXValue, .cgPoint, &point)
    AXValueGetValue(sizeVal as! AXValue, .cgSize, &size)
    return (Double(point.x), Double(point.y), Double(size.width), Double(size.height))
}

/// Returns context about the current state of an app (window title, focused element, browser URL).
func currentContext(pid: Int) -> AppContext {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let appName: String
    if let app = NSRunningApplication(processIdentifier: Int32(pid)) {
        appName = app.localizedName ?? "Unknown"
    } else {
        appName = "Unknown"
    }

    var windowTitle = ""
    if let win = axWindows(appRef).first {
        windowTitle = axString(win, "AXTitle") ?? ""
    }

    // Check for browser URL
    var url: String? = nil
    if let win = axWindows(appRef).first {
        if let toolbar = findToolbarChild(of: win) {
            if let urlField = findToolbarURLField(in: toolbar) {
                // findToolbarURLField returns the first AXTextField/AXComboBox in
                // the toolbar — it is not URL-specific, so an app whose toolbar
                // hosts a 2FA code, API key, or licence field would surface that
                // value here. Every other AXValue read in this helper is gated;
                // this one must be too.
                if !isSensitiveAXValue(axValueSensitivityMetadata(urlField)),
                   let val = axValue(urlField, "AXValue") {
                    url = "\(val)"
                }
            }
        }
    }

    var focused: String? = nil
    var focusedRef: CFTypeRef?
    if AXUIElementCopyAttributeValue(appRef, "AXFocusedUIElement" as CFString, &focusedRef) == .success,
       let ref = focusedRef {
        // CFTypeRef is non-nil; cast to AXUIElement (CoreFoundation cast always succeeds)
        let el = ref as! AXUIElement
        let role = axString(el, "AXRole") ?? ""
        let title = axString(el, "AXTitle") ?? ""
        if !role.isEmpty {
            focused = title.isEmpty ? role : "\(role) '\(title)'"
        }
    }

    return AppContext(app: appName, window: windowTitle, url: url, focusedElement: focused)
}

/// Finds a child with AXToolbar role (used by currentContext for browser URL detection).
private func findToolbarChild(of el: AXUIElement) -> AXUIElement? {
    guard let children = axChildren(el) else { return nil }
    for child in children {
        if axString(child, "AXRole") == "AXToolbar" {
            return child
        }
    }
    for child in children {
        guard let grandchildren = axChildren(child) else { continue }
        for gc in grandchildren {
            if axString(gc, "AXRole") == "AXToolbar" {
                return gc
            }
        }
    }
    return nil
}

/// Finds a text field inside a toolbar that looks like a URL bar.
private func findToolbarURLField(in el: AXUIElement) -> AXUIElement? {
    guard let children = axChildren(el) else { return nil }
    for child in children {
        let role = axString(child, "AXRole") ?? ""
        if role == "AXTextField" || role == "AXComboBox" {
            return child
        }
        if let found = findToolbarURLField(in: child) {
            return found
        }
    }
    return nil
}

struct RunningApplicationSelectionCandidate: Equatable {
    let pid: Int
    let localizedName: String?
    let bundleID: String?
}

enum RunningApplicationSelection: Equatable {
    case notRunning
    case selected(Int)
    case ambiguous
}

enum BackgroundTaskAppEligibility: Equatable {
    case eligible
    case targetIsFrontmost
    case noVisibleWindow
}

func backgroundTaskAppEligibility(
    targetPID: Int,
    frontmostPID: Int?,
    visibleNormalWindowPIDs: [Int]
) -> BackgroundTaskAppEligibility {
    if frontmostPID == targetPID {
        return .targetIsFrontmost
    }
    if !visibleNormalWindowPIDs.contains(targetPID) {
        return .noVisibleWindow
    }
    return .eligible
}

/// Chooses one exact user-facing process without relying on NSWorkspace's
/// unspecified same-name ordering. Frontmost identity wins, then the first
/// visible normal WindowServer window. Multiple windowless instances are
/// ambiguous rather than silently binding a task to an arbitrary process.
func selectRunningApplication(
    appName: String,
    candidates: [RunningApplicationSelectionCandidate],
    excludedPIDs: Set<Int>,
    frontmostPID: Int?,
    visibleWindowPIDsFrontToBack: [Int]
) -> RunningApplicationSelection {
    let requested = appName.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
    guard !requested.isEmpty else { return .notRunning }

    let available = candidates.filter {
        $0.pid > 0 && !excludedPIDs.contains($0.pid)
    }
    let exactName = available.filter {
        $0.localizedName?.lowercased() == requested
    }
    let exactBundleID = available.filter {
        $0.bundleID?.lowercased() == requested
    }
    let bundleSuffix = available.filter {
        $0.bundleID?.split(separator: ".").last?.lowercased() == requested
    }
    let matches: [RunningApplicationSelectionCandidate]
    if !exactName.isEmpty {
        matches = exactName
    } else if !exactBundleID.isEmpty {
        matches = exactBundleID
    } else {
        matches = bundleSuffix
    }
    guard !matches.isEmpty else { return .notRunning }

    let matchingPIDs = Set(matches.map(\.pid))
    if let frontmostPID, matchingPIDs.contains(frontmostPID) {
        return .selected(frontmostPID)
    }
    for pid in visibleWindowPIDsFrontToBack where matchingPIDs.contains(pid) {
        return .selected(pid)
    }
    if matches.count == 1 {
        return .selected(matches[0].pid)
    }
    return .ambiguous
}

func runningApplicationSelection(
    appName: String,
    excluding excludedPIDs: Set<Int> = []
) -> (
    selection: RunningApplicationSelection,
    applications: [NSRunningApplication]
) {
    // ax_server is a synchronous socket process whose main thread normally
    // blocks in read(2), so AppKit never gets a natural run-loop turn. Without
    // this pump, NSWorkspace.shared.runningApplications remains the snapshot
    // from ax_server startup and cannot see apps launched later in the session.
    refreshAppKitState()

    let applications = NSWorkspace.shared.runningApplications.filter { !$0.isTerminated }
    let candidates = applications.map {
        RunningApplicationSelectionCandidate(
            pid: Int($0.processIdentifier),
            localizedName: $0.localizedName,
            bundleID: $0.bundleIdentifier)
    }
    let frontmostPID = NSWorkspace.shared.frontmostApplication.map {
        Int($0.processIdentifier)
    }
    var visiblePIDs: [Int] = []
    var seenVisiblePIDs = Set<Int>()
    for window in currentCGWindowIdentityCandidates()
    where window.layer == 0 && window.isOnScreen && window.alpha > 0 {
        if seenVisiblePIDs.insert(window.ownerPID).inserted {
            visiblePIDs.append(window.ownerPID)
        }
    }
    return (
        selectRunningApplication(
            appName: appName,
            candidates: candidates,
            excludedPIDs: excludedPIDs,
            frontmostPID: frontmostPID,
            visibleWindowPIDsFrontToBack: visiblePIDs),
        applications)
}

/// Resolves a user-facing app name or bundle identifier to one exact running
/// application. Existing callers without an exclusion set retain the same
/// name/bundle matching surface but no longer inherit arbitrary process order.
func resolveRunningApplication(
    appName: String,
    excluding excludedPIDs: Set<Int> = []
) -> NSRunningApplication? {
    let resolved = runningApplicationSelection(
        appName: appName,
        excluding: excludedPIDs)
    guard case let .selected(pid) = resolved.selection else { return nil }
    return resolved.applications.first {
        Int($0.processIdentifier) == pid
    }
}

/// Resolves an app name to its PID via NSWorkspace.
/// Retries up to 3 times with short delays for apps that just launched.
func resolvePID(appName: String) -> Int? {
    for attempt in 0..<3 {
        if let app = resolveRunningApplication(appName: appName) {
            return Int(app.processIdentifier)
        }
        if attempt < 2 {
            Thread.sleep(forTimeInterval: 0.5)
        }
    }
    return nil
}

/// Gives AppKit a bounded chance to deliver workspace/frontmost-app updates.
/// Keep this short: every ax_server request is serialized on the calling
/// thread, but a blocked socket loop otherwise gives AppKit no run-loop turns.
func refreshAppKitState(for interval: TimeInterval = 0.01) {
    let deadline = Date(timeIntervalSinceNow: max(0, interval))
    repeat {
        let handledEvent = RunLoop.current.run(mode: .default, before: deadline)
        if !handledEvent { break }
    } while Date() < deadline
}
