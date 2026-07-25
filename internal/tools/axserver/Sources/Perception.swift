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

func walkTree(
    _ el: AXUIElement,
    semanticDepth: Int,
    budget: Int,
    filter: String,
    path: String,
    focusedElement: AXUIElement?,
    focusedRef: inout String?
) -> Element? {
    guard let role = axString(el, "AXRole") else { return nil }

    let subrole = axString(el, "AXSubrole")
    let identifier = axString(el, "AXIdentifier")
    let title = axString(el, "AXTitle")
    let desc = axString(el, "AXDescription")
    let protectedContent = axBool(el, "AXProtectedContent") ?? false
    let valueRedacted = isSensitiveAXValue(axValueSensitivityMetadata(
        el,
        role: role,
        subrole: subrole,
        identifier: identifier,
        title: title,
        description: desc,
        protectedContent: protectedContent))
    var valStr: String? = nil
    if !valueRedacted, let v = axValue(el, "AXValue") {
        let s = "\(v)"
        valStr = s.count > 200 ? String(s.prefix(200)) + "..." : s
    }
    let enabled = axBool(el, "AXEnabled") ?? true
    let selected = axBool(el, "AXSelected") ?? false
    let isFocusedElement = axElementsEqual(el, focusedElement)
    let focused = (axBool(el, "AXFocused") ?? false) || isFocusedElement
    let actions = axActions(el)
    let frame = elementFrame(el).map {
        AXFrame(x: $0.x, y: $0.y, width: $0.width, height: $0.height)
    }

    let hasContent = title != nil || desc != nil || valStr != nil
    let cost = (layoutRoles.contains(role) && !hasContent) ? 0 : 1
    let newDepth = semanticDepth + cost

    guard newDepth <= budget else { return nil }

    var childElements: [Element] = []
    if let kids = axChildren(el) {
        var childIndex: [String: Int] = [:]
        var results: [Element] = []
        for kid in kids {
            guard let kidRole = axString(kid, "AXRole") else { continue }
            let idx = childIndex[kidRole, default: 0]
            childIndex[kidRole] = idx + 1
            let childPath = "\(path)/\(kidRole)[\(idx)]"
            if let child = walkTree(
                kid,
                semanticDepth: newDepth,
                budget: budget,
                filter: filter,
                path: childPath,
                focusedElement: focusedElement,
                focusedRef: &focusedRef
            ) {
                results.append(child)
            }
        }
        childElements = results
    }

    if filter == "interactive" {
        let isInteractive = interactiveRoles.contains(role)
        let hasInteractiveChildren = !childElements.isEmpty
        if !isInteractive && !hasInteractiveChildren { return nil }
    }

    refCounter += 1
    let ref = "e\(refCounter)"
    let attributes = AXElementSnapshotAttributes(
        role: role,
        subrole: subrole?.isEmpty == false ? subrole : nil,
        identifier: identifier?.isEmpty == false ? identifier : nil,
        title: title?.isEmpty == false ? title : nil,
        description: desc?.isEmpty == false ? desc : nil,
        value: valStr?.isEmpty == false ? valStr : nil,
        valueRedacted: valueRedacted,
        protectedContent: protectedContent,
        enabled: enabled,
        focused: focused,
        selected: selected,
        actions: actions,
        frame: frame)
    let element = makeElementSnapshot(
        attributes: attributes,
        ref: ref,
        path: path,
        children: childElements)
    refPaths[ref] = RefEntry(path: path, role: role, fingerprint: element.fingerprint)
    if isFocusedElement {
        focusedRef = ref
    }
    return element
}

func annotateElements(pid: Int, roles: [String]?, maxLabels: Int) -> AnnotateResult? {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let appName: String
    let bundleID: String?
    if let app = NSRunningApplication(processIdentifier: Int32(pid)) {
        appName = app.localizedName ?? "Unknown"
        bundleID = app.bundleIdentifier
    } else {
        appName = "Unknown"
        bundleID = nil
    }

    guard let win = axWindows(appRef).first else {
        return nil
    }

    let winTitle = axString(win, "AXTitle") ?? ""
    let windowFrame = elementFrame(win).map {
        AXFrame(x: $0.x, y: $0.y, width: $0.width, height: $0.height)
    }
    let windowID = uniqueWindowID(pid: pid, title: winTitle, frame: windowFrame)
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
        appName: appName,
        bundleID: bundleID,
        pid: pid,
        window: winTitle,
        windowID: windowID,
        windowFrame: windowFrame,
        annotations: annotations,
        refPaths: annotateRefPaths
    )
}

func readTree(pid: Int, budget: Int, filter: String) -> ReadTreeResult? {
    let appRef = AXUIElementCreateApplication(Int32(pid))
    let appName: String
    let bundleID: String?
    if let app = NSRunningApplication(processIdentifier: Int32(pid)) {
        appName = app.localizedName ?? "Unknown"
        bundleID = app.bundleIdentifier
    } else {
        appName = "Unknown"
        bundleID = nil
    }

    guard let win = axWindows(appRef).first else {
        return nil
    }

    let winTitle = axString(win, "AXTitle") ?? ""
    let windowFrame = elementFrame(win).map {
        AXFrame(x: $0.x, y: $0.y, width: $0.width, height: $0.height)
    }
    let focusedElement = axFocusedElement(appRef)
    var focusedRef: String?
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
            if let elem = walkTree(
                kid,
                semanticDepth: 0,
                budget: budget,
                filter: filter,
                path: path,
                focusedElement: focusedElement,
                focusedRef: &focusedRef
            ) {
                elements.append(elem)
            }
        }
    }

    return ReadTreeResult(
        schemaVersion: 1,
        appName: appName,
        bundleID: bundleID,
        pid: pid,
        windowTitle: winTitle,
        windowID: uniqueWindowID(pid: pid, title: winTitle, frame: windowFrame),
        windowFrame: windowFrame,
        focusedRef: focusedRef,
        elements: elements,
        refPaths: refPaths
    )
}
