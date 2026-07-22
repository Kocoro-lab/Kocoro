import ApplicationServices
import AppKit

let interactiveRoles: Set<String> = [
    "AXButton", "AXTextField", "AXTextArea", "AXCheckBox",
    "AXRadioButton", "AXPopUpButton", "AXComboBox", "AXSlider",
    "AXMenuItem", "AXLink", "AXRow", "AXMenuButton",
    "AXIncrementor", "AXColorWell", "AXDisclosureTriangle",
    "AXTabGroup", "AXTab", "AXToolbar", "AXMenuBar",
    "AXMenu", "AXSegmentedControl",
]

/// Layout containers that cost 0 semantic depth.
let layoutRoles: Set<String> = [
    "AXGroup", "AXGenericElement", "AXSection", "AXDiv",
    "AXList", "AXLandmarkMain", "AXLandmarkNavigation",
    "AXLandmarkBanner", "AXLandmarkContentInfo",
    "AXSplitGroup", "AXScrollArea", "AXLayoutArea",
]

var refCounter = 0
var refPaths: [String: RefEntry] = [:]

func walkTree(_ el: AXUIElement, semanticDepth: Int, budget: Int, filter: String, path: String) -> Element? {
    guard let role = axString(el, "AXRole") else { return nil }

    let subrole = axString(el, "AXSubrole")
    let title = axString(el, "AXTitle")
    let desc = axString(el, "AXDescription")
    var valStr: String? = nil
    if let v = axValue(el, "AXValue") {
        let s = "\(v)"
        valStr = s.count > 200 ? String(s.prefix(200)) + "..." : s
    }
    let enabled = axBool(el, "AXEnabled")
    let selected = axBool(el, "AXSelected")

    let hasContent = title != nil || desc != nil || valStr != nil
    let cost = (layoutRoles.contains(role) && !hasContent) ? 0 : 1
    let newDepth = semanticDepth + cost

    guard newDepth <= budget else { return nil }

    var childElements: [Element]? = nil
    if let kids = axChildren(el) {
        var childIndex: [String: Int] = [:]
        var results: [Element] = []
        for kid in kids {
            guard let kidRole = axString(kid, "AXRole") else { continue }
            let idx = childIndex[kidRole, default: 0]
            childIndex[kidRole] = idx + 1
            let childPath = "\(path)/\(kidRole)[\(idx)]"
            if let child = walkTree(kid, semanticDepth: newDepth, budget: budget, filter: filter, path: childPath) {
                results.append(child)
            }
        }
        if !results.isEmpty { childElements = results }
    }

    if filter == "interactive" {
        let isInteractive = interactiveRoles.contains(role)
        let hasInteractiveChildren = childElements != nil && !childElements!.isEmpty
        if !isInteractive && !hasInteractiveChildren { return nil }
    }

    refCounter += 1
    let ref = "e\(refCounter)"
    refPaths[ref] = RefEntry(path: path, role: role)

    var elem = Element(ref: ref, role: role)
    if let s = subrole, !s.isEmpty { elem.subrole = s }
    if let t = title, !t.isEmpty { elem.title = t }
    if let d = desc, !d.isEmpty { elem.desc = d }
    if let v = valStr, !v.isEmpty { elem.value = v }
    if let e = enabled, !e { elem.enabled = e }
    if let s = selected, s { elem.selected = s }
    elem.children = childElements

    return elem
}

func annotateElements(pid: Int, roles: [String]?, maxLabels: Int) -> AnnotateResult? {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let appName: String
    if let app = NSRunningApplication(processIdentifier: Int32(pid)) {
        appName = app.localizedName ?? "Unknown"
    } else {
        appName = "Unknown"
    }

    guard let win = axWindows(appRef).first else {
        return nil
    }

    let winTitle = axString(win, "AXTitle") ?? ""
    let roleFilter: Set<String>? = roles.flatMap { roles in
        roles.isEmpty ? nil : Set(roles)
    }

    var annotations: [AnnotationEntry] = []
    var annotateRefPaths: [String: RefEntry] = [:]
    var labelCounter = 0

    func walkForAnnotation(_ el: AXUIElement, path: String) {
        guard labelCounter < maxLabels else { return }
        guard let role = axString(el, "AXRole") else { return }

        let isInteractive = interactiveRoles.contains(role)
        let matchesFilter = roleFilter == nil || roleFilter!.contains(role)

        if isInteractive && matchesFilter {
            if let frame = elementFrame(el) {
                labelCounter += 1
                let ref = "a\(labelCounter)"
                let title = axString(el, "AXTitle") ?? axString(el, "AXDescription")
                annotations.append(AnnotationEntry(
                    label: labelCounter, ref: ref, role: role,
                    title: title,
                    x: frame.x, y: frame.y,
                    width: frame.width, height: frame.height
                ))
                annotateRefPaths[ref] = RefEntry(path: path, role: role)
            }
        }

        guard labelCounter < maxLabels else { return }
        if let kids = axChildren(el) {
            var childIndex: [String: Int] = [:]
            for kid in kids {
                guard let kidRole = axString(kid, "AXRole") else { continue }
                let idx = childIndex[kidRole, default: 0]
                childIndex[kidRole] = idx + 1
                let childPath = "\(path)/\(kidRole)[\(idx)]"
                walkForAnnotation(kid, path: childPath)
                if labelCounter >= maxLabels { break }
            }
        }
    }

    if let kids = axChildren(win) {
        var childIndex: [String: Int] = [:]
        for kid in kids {
            guard let kidRole = axString(kid, "AXRole") else { continue }
            let idx = childIndex[kidRole, default: 0]
            childIndex[kidRole] = idx + 1
            let path = "window[0]/\(kidRole)[\(idx)]"
            walkForAnnotation(kid, path: path)
            if labelCounter >= maxLabels { break }
        }
    }

    return AnnotateResult(
        app: appName,
        pid: pid,
        window: winTitle,
        annotations: annotations,
        refPaths: annotateRefPaths
    )
}

func readTree(pid: Int, budget: Int, filter: String) -> ReadTreeResult? {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let appName: String
    if let app = NSRunningApplication(processIdentifier: Int32(pid)) {
        appName = app.localizedName ?? "Unknown"
    } else {
        appName = "Unknown"
    }

    guard let win = axWindows(appRef).first else {
        return nil
    }

    let winTitle = axString(win, "AXTitle") ?? ""
    refCounter = 0
    refPaths = [:]
    var elements: [Element] = []
    if let kids = axChildren(win) {
        var childIndex: [String: Int] = [:]
        for kid in kids {
            guard let kidRole = axString(kid, "AXRole") else { continue }
            let idx = childIndex[kidRole, default: 0]
            childIndex[kidRole] = idx + 1
            let path = "window[0]/\(kidRole)[\(idx)]"
            if let elem = walkTree(kid, semanticDepth: 0, budget: budget, filter: filter, path: path) {
                elements.append(elem)
            }
        }
    }

    return ReadTreeResult(
        app: appName,
        pid: pid,
        window: winTitle,
        elements: elements,
        refPaths: refPaths
    )
}
