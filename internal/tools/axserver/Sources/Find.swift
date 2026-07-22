import ApplicationServices

func findElements(pid: Int, query: String?, role: String?, identifier: String?) -> [FindResult] {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let windows = axWindows(appRef)

    var results: [FindResult] = []
    for (winIdx, window) in windows.enumerated() {
        searchTree(window, path: "window[\(winIdx)]", query: query, role: role,
                   identifier: identifier, results: &results, limit: 50)
    }
    return results
}

private func searchTree(_ el: AXUIElement, path: String, query: String?, role: String?,
                         identifier: String?, results: inout [FindResult], limit: Int) {
    guard results.count < limit else { return }

    let elRole = axString(el, "AXRole") ?? ""
    let title = axString(el, "AXTitle") ?? ""
    let desc = axString(el, "AXDescription") ?? ""
    let value: String
    if let v = axValue(el, "AXValue") {
        value = "\(v)"
    } else {
        value = ""
    }
    let ident = axString(el, "AXIdentifier")

    // Match by identifier (exact)
    if let id = identifier, let actualIdent = ident, actualIdent == id {
        var r = FindResult(path: path, role: elRole, title: title)
        if !desc.isEmpty { r.desc = desc }
        if !value.isEmpty { r.value = String(value.prefix(200)) }
        results.append(r)
        return
    }

    // Match by role + query
    let roleMatch = role == nil || elRole == role
    var textMatch = query == nil
    if let q = query?.lowercased() {
        textMatch = title.lowercased().contains(q)
                 || desc.lowercased().contains(q)
                 || value.lowercased().contains(q)
    }

    if roleMatch && textMatch {
        var r = FindResult(path: path, role: elRole, title: title)
        if !desc.isEmpty { r.desc = desc }
        if !value.isEmpty { r.value = String(value.prefix(200)) }
        results.append(r)
    }

    // Recurse
    guard let children = axChildren(el) else { return }
    var childIndex: [String: Int] = [:]
    for child in children {
        guard let childRole = axString(child, "AXRole") else { continue }
        let idx = childIndex[childRole, default: 0]
        childIndex[childRole] = idx + 1
        searchTree(child, path: "\(path)/\(childRole)[\(idx)]", query: query,
                   role: role, identifier: identifier, results: &results, limit: limit)
    }
}
