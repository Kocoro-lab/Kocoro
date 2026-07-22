import ApplicationServices
import AppKit

/// Polls for UI conditions until satisfied or timeout.
func waitFor(pid: Int, condition: String, value: String?, query: String?, role: String?,
             timeout: Double, interval: Double) -> (ActionResult?, ErrorInfo?) {

    let deadline = Date().addingTimeInterval(timeout)
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let hasElementSelector = !(query == nil || query!.isEmpty) || !(role == nil || role!.isEmpty)

    guard condition == "titleContains" || condition == "urlContains" ||
        condition == "titleChanged" || condition == "urlChanged" ||
        hasElementSelector else {
        return (nil, ErrorInfo(
            code: -1,
            message: "elementExists/elementGone require at least one of 'query' or 'role'"
        ))
    }

    // Capture initial values for "changed" conditions
    let initialTitle: String?
    let initialURL: String?
    switch condition {
    case "titleChanged":
        initialTitle = windowTitle(appRef: appRef)
        initialURL = nil
    case "urlChanged":
        initialTitle = nil
        initialURL = browserURL(appRef: appRef)
    default:
        initialTitle = nil
        initialURL = nil
    }

    // Validate: elementExists/elementGone require at least a query or role
    if condition == "elementExists" || condition == "elementGone" {
        if (query == nil || query!.isEmpty) && (role == nil || role!.isEmpty) {
            return (nil, ErrorInfo(code: -1, message: "\(condition) requires at least 'query' or 'role' to identify the target element"))
        }
    }

    while Date() < deadline {
        switch condition {
        case "elementExists":
            let found = findElements(pid: pid, query: query, role: role, identifier: nil)
            if !found.isEmpty {
                return (ActionResult(result: "element found: \(found[0].role) '\(found[0].title)'"), nil)
            }

        case "elementGone":
            let found = findElements(pid: pid, query: query, role: role, identifier: nil)
            if found.isEmpty {
                return (ActionResult(result: "element gone"), nil)
            }

        case "titleContains":
            guard let substring = value else {
                return (nil, ErrorInfo(code: -1, message: "titleContains requires 'value'"))
            }
            if let title = windowTitle(appRef: appRef),
               title.lowercased().contains(substring.lowercased()) {
                return (ActionResult(result: "title contains '\(substring)': \(title)"), nil)
            }

        case "urlContains":
            guard let substring = value else {
                return (nil, ErrorInfo(code: -1, message: "urlContains requires 'value'"))
            }
            if let url = browserURL(appRef: appRef),
               url.lowercased().contains(substring.lowercased()) {
                return (ActionResult(result: "URL contains '\(substring)': \(url)"), nil)
            }

        case "titleChanged":
            if let current = windowTitle(appRef: appRef), current != initialTitle {
                return (ActionResult(result: "title changed from '\(initialTitle ?? "")' to '\(current)'"), nil)
            }

        case "urlChanged":
            if let current = browserURL(appRef: appRef), current != initialURL {
                return (ActionResult(result: "URL changed from '\(initialURL ?? "")' to '\(current)'"), nil)
            }

        default:
            return (nil, ErrorInfo(code: -1,
                message: "unknown condition '\(condition)' — valid: elementExists, elementGone, titleContains, urlContains, titleChanged, urlChanged"))
        }

        Thread.sleep(forTimeInterval: interval)
    }

    // Timeout — report current state
    var detail = ""
    switch condition {
    case "elementExists":
        detail = "element matching query=\(query ?? "nil") role=\(role ?? "nil") not found"
    case "elementGone":
        detail = "element still exists"
    case "titleContains":
        let current = windowTitle(appRef: appRef) ?? "(none)"
        detail = "title is '\(current)', does not contain '\(value ?? "")'"
    case "urlContains":
        let current = browserURL(appRef: appRef) ?? "(none)"
        detail = "URL is '\(current)', does not contain '\(value ?? "")'"
    case "titleChanged":
        detail = "title unchanged: '\(windowTitle(appRef: appRef) ?? "(none)")'"
    case "urlChanged":
        detail = "URL unchanged: '\(browserURL(appRef: appRef) ?? "(none)")'"
    default:
        break
    }

    return (nil, ErrorInfo(code: -2, message: "timeout after \(timeout)s — \(detail)"))
}

// MARK: - Helpers

/// Returns the title of the first window.
private func windowTitle(appRef: AXUIElement) -> String? {
    guard let win = axWindows(appRef).first else { return nil }
    return axString(win, "AXTitle")
}

/// Finds the browser URL bar value by looking for AXTextField inside AXToolbar.
private func browserURL(appRef: AXUIElement) -> String? {
    guard let win = axWindows(appRef).first else { return nil }

    // Search for a text field in the toolbar (browser URL bar pattern)
    if let toolbar = findChild(of: win, role: "AXToolbar") {
        if let urlField = findURLField(in: toolbar) {
            if let val = axValue(urlField, "AXValue") {
                return "\(val)"
            }
        }
    }

    // Fallback: search the whole window for a text field with URL-like value
    return findURLInTree(win, depth: 0, maxDepth: 5)
}

/// Finds a direct or nested child with the given role.
private func findChild(of el: AXUIElement, role: String) -> AXUIElement? {
    guard let children = axChildren(el) else { return nil }
    for child in children {
        if axString(child, "AXRole") == role {
            return child
        }
    }
    // One level deeper
    for child in children {
        if let found = findChild(of: child, role: role) {
            return found
        }
    }
    return nil
}

/// Finds a text field that looks like a URL bar inside a toolbar.
private func findURLField(in el: AXUIElement) -> AXUIElement? {
    guard let children = axChildren(el) else { return nil }
    for child in children {
        let role = axString(child, "AXRole") ?? ""
        if role == "AXTextField" || role == "AXComboBox" {
            return child
        }
        if let found = findURLField(in: child) {
            return found
        }
    }
    return nil
}

/// Searches for a URL-like value in a text field anywhere in the tree.
private func findURLInTree(_ el: AXUIElement, depth: Int, maxDepth: Int) -> String? {
    guard depth < maxDepth else { return nil }
    let role = axString(el, "AXRole") ?? ""
    if role == "AXTextField" || role == "AXComboBox" {
        if let val = axValue(el, "AXValue") {
            let s = "\(val)"
            if s.contains(".") || s.hasPrefix("http") {
                return s
            }
        }
    }
    guard let children = axChildren(el) else { return nil }
    for child in children {
        if let found = findURLInTree(child, depth: depth + 1, maxDepth: maxDepth) {
            return found
        }
    }
    return nil
}
