import Foundation
import ApplicationServices
import CoreGraphics
import AppKit

// MARK: - CLI Permission Modes

let args = CommandLine.arguments

if args.contains("--check-permissions") {
    // One-shot: output permission status JSON and exit.
    // Does NOT require accessibility to be granted — that's what we're checking.
    let status = checkAllPermissions()
    if let data = try? JSONSerialization.data(withJSONObject: status, options: [.sortedKeys]),
       let str = String(data: data, encoding: .utf8) {
        print(str)
    }
    exit(0)
}

if let idx = args.firstIndex(of: "--request-permission"), idx + 1 < args.count {
    // One-shot: trigger permission dialog and exit.
    let permission = args[idx + 1]
    let result = requestPermissionCLI(permission)
    if let data = try? JSONSerialization.data(withJSONObject: result, options: [.sortedKeys]),
       let str = String(data: data, encoding: .utf8) {
        print(str)
    }
    exit(0)
}

// MARK: - Unix Socket Server Mode

if let idx = args.firstIndex(of: "--socket"), idx + 1 < args.count {
    let socketPath = args[idx + 1]
    runSocketServer(path: socketPath)
    exit(0)
}

// MARK: - Normal Stdin Loop Mode

// Every helper start is the daemon's one-shot recovery watchdog: a valid,
// redacted release journal is consumed before any new mutation can commit.
// Malformed or unconfirmed recovery fails the process-global input gate closed;
// observation RPCs remain available for diagnosis.
_ = processInputCommitGateV1.recoverAtStartup()
let stdinShutdownController = SocketServerShutdownController(socketPath: nil)
stdinShutdownController.installSignalSources()

// Check accessibility permission once at startup.
guard AXIsProcessTrusted() else {
    let err = Response(id: 0, error: ErrorInfo(code: -1,
        message: "Accessibility permission not granted. Enable in: System Settings > Privacy & Security > Accessibility. Add your terminal app."))
    writeResponse(err)
    exit(1)
}

let encoder = makeWireEncoder()
let decoder = JSONDecoder()

// Persistent stdin read loop — one JSON request per line.
while let line = readLine(strippingNewline: true) {
    guard let data = line.data(using: .utf8) else { continue }
    writeResponse(dispatchWireRequest(data, decoder: decoder))
}
stdinShutdownController.stop(signal: 0, terminate: false)

// MARK: - Dispatch

private struct WireRequestHeader: Decodable {
    let id: Int64
    let method: String
}

func emptyParams() -> Params {
    Params(
        schemaVersion: nil, topologyRef: nil,
        pid: nil, maxDepth: nil, semanticBudget: nil, filter: nil,
        path: nil, expectedRole: nil, expectedFingerprint: nil,
        windowID: nil, bundleID: nil, expectedQuartzBounds: nil,
        fallbackPolicy: nil, value: nil, appName: nil,
        query: nil, role: nil, identifier: nil, type: nil,
        x: nil, y: nil, button: nil, clicks: nil,
        key: nil, modifiers: nil, dx: nil, dy: nil,
        windowTitle: nil, verify: nil, condition: nil,
        timeout: nil, interval: nil, roles: nil, maxLabels: nil,
        excludedPIDs: nil)
}

/// Routes the coordinate mutation through its method-specific exact-key
/// decoder while preserving the existing permissive Request/Params decoder for
/// every legacy RPC.
func dispatchWireRequest(
    _ data: Data,
    decoder: JSONDecoder = JSONDecoder(),
    coordinateDependencies: CoordinateMouseEventDependencies =
        productionCoordinateMouseEventDependencies,
    coordinateDragDependencies: CoordinateDragDependencies? = nil,
    coordinatePixelScrollDependenciesV1: CoordinatePixelScrollDependenciesV1? = nil,
    semanticTextSelectionDependencies: SemanticTextSelectionDependencies =
        productionSemanticTextSelectionDependencies,
    semanticTextSelectionDependenciesV2: SemanticTextSelectionDependenciesV2 =
        productionSemanticTextSelectionDependenciesV2,
    semanticPressDependenciesV2: SemanticPressDependenciesV2 =
        productionSemanticPressDependenciesV2,
    semanticScrollDependenciesV1: SemanticScrollDependenciesV1? = nil,
    targetBoundInputDependencies: TargetBoundInputDependencies? = nil,
    backgroundTargetedInputDependencies:
        BackgroundTargetedInputDependenciesV1? = nil,
    coordinateDisplayDependencies: CaptureCoordinateDisplayDependencies =
        productionCaptureCoordinateDisplayDependencies
) -> Response {
    guard let header = try? decoder.decode(WireRequestHeader.self, from: data) else {
        return Response(
            id: 0,
            error: ErrorInfo(code: -1, message: "Invalid JSON request"))
    }
    if header.method == "coordinate_mouse_event" {
        do {
            let request = try decodeCoordinateMouseEventRPCRequestV1(data)
            let result = runCoordinateMouseEventWithModifiersV1(
                request: request.params,
                dependencies: coordinateDependencies)
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(CoordinateMouseEventResultV1(
                    status: "failed",
                    action: "unknown",
                    primaryActionCommitted: false,
                    pointerMotionCommitted: false,
                    phase: "preflight",
                    failureCode: "invalid_request",
                    pointerEndpoint: nil)))
        }
    }
    if header.method == "coordinate_drag" {
        do {
            let request = try decodeCoordinateDragRPCRequestV1(data)
            let cancellationURL = coordinateDragCancellationMarkerURL(
                requestID: request.id, helperBootID: request.params.helperBootID)
            defer { try? FileManager.default.removeItem(at: cancellationURL) }
            let result = runCoordinateDragWithModifiersV1(
                request: request.params,
                dependencies: coordinateDragDependencies ?? productionCoordinateDragDependencies(
                    requestID: request.id,
                    helperBootID: request.params.helperBootID))
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(CoordinateDragResultV1(
                    status: "failed", dragCommitted: false,
                    mouseDownCommitted: false, pointerMotionCommitted: false,
                    mouseUpCommitted: false, possibleDropSideEffect: false,
                    phase: "preflight", failureCode: "invalid_request",
                    postcondition: nil, pointerEndpoint: nil)))
        }
    }
    if header.method == "coordinate_pixel_scroll" {
        do {
            let request = try decodeCoordinatePixelScrollRPCRequestV1(data)
            let cancellationURL = coordinatePixelScrollCancellationMarkerURL(
                requestID: request.id, helperBootID: request.params.helperBootID)
            defer { try? FileManager.default.removeItem(at: cancellationURL) }
            let result = runCoordinatePixelScrollWithModifiersV1(
                request: request.params,
                dependencies: coordinatePixelScrollDependenciesV1 ??
                    productionCoordinatePixelScrollDependenciesV1(
                        requestID: request.id,
                        helperBootID: request.params.helperBootID,
                        modifiers: request.params.modifiers))
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(CoordinatePixelScrollResultV1(
                    status: "failed",
                    pointerMoveCommitState: .notCommitted,
                    scrollCommitState: .notCommitted,
                    phase: "preflight", failureCode: "invalid_request",
                    requested: nil, pointerEndpoint: nil)))
        }
    }
    if header.method == "semantic_text_selection" {
        do {
            let request = try decodeSemanticTextSelectionRPCRequestV1(data)
            let result = runSemanticTextSelection(
                request: request.params,
                dependencies: semanticTextSelectionDependencies)
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(SemanticTextSelectionResultV1(
                    status: "failed", selectionCommitted: false,
                    phase: "preflight", failureCode: "invalid_request",
                    postcondition: nil, selectedRange: nil)))
        }
    }
    if header.method == "semantic_text_selection_v2" {
        do {
            let request = try decodeSemanticTextSelectionRPCRequestV2(data)
            let result = runSemanticTextSelectionV2(
                request: request.params,
                dependencies: semanticTextSelectionDependenciesV2)
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(SemanticTextSelectionResultV2(
                    status: "failed", commitState: "not_committed",
                    phase: "preflight", failureCode: "invalid_request")))
        }
    }
    if header.method == "semantic_press_v2" {
        do {
            let request = try decodeSemanticPressRPCRequestV2(data)
            let result = runSemanticPressV2(
                request: request.params,
                dependencies: semanticPressDependenciesV2)
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(SemanticPressResultV2(
                    status: "failed", commitState: "not_committed",
                    phase: "preflight", failureCode: "invalid_request")))
        }
    }
    if header.method == "semantic_scroll_v1" {
        do {
            let request = try decodeSemanticScrollRPCRequestV1(data)
            let result = runSemanticScrollV1(
                request: request.params,
                dependencies: semanticScrollDependenciesV1 ??
                    productionSemanticScrollDependenciesV1(
                        requestID: request.id, request: request.params))
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(SemanticScrollResultV1(
                    status: "failed", commitState: "not_committed",
                    phase: "preflight", failureCode: "invalid_request")))
        }
    }
    if header.method == "target_bound_input" {
        do {
            let request = try decodeTargetBoundInputRPCRequestV1(data)
            let cancellationURL = targetBoundInputCancellationMarkerURLV1(
                requestID: request.id, request: request.params)
            defer { try? FileManager.default.removeItem(at: cancellationURL) }
            let result = runTargetBoundInput(
                request: request.params,
                dependencies: targetBoundInputDependencies ??
                    productionTargetBoundInputDependenciesV1(isCancelled: {
                        FileManager.default.fileExists(atPath: cancellationURL.path)
                    }))
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(TargetBoundInputResultV1(
                    status: "failed", action: "unknown",
                    inputCommitted: false, clipboardTouched: false,
                    clipboardRestored: false, phase: "preflight",
                    failureCode: "invalid_request")))
        }
    }
    if header.method == "background_targeted_input" {
        do {
            let request = try decodeBackgroundTargetedInputRPCRequestV1(data)
            let cancellationURL = targetBoundInputCancellationMarkerURLV1(
                requestID: request.id,
                request: request.params.input)
            defer { try? FileManager.default.removeItem(at: cancellationURL) }
            let result = runBackgroundTargetedInputV1(
                request: request.params,
                dependencies: backgroundTargetedInputDependencies ??
                    productionBackgroundTargetedInputDependenciesV1(isCancelled: {
                        FileManager.default.fileExists(atPath: cancellationURL.path)
                    }))
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(TargetBoundInputResultV1(
                    status: "failed", action: "unknown",
                    inputCommitted: false, clipboardTouched: false,
                    clipboardRestored: false, phase: "preflight",
                    failureCode: "invalid_request")))
        }
    }
    if header.method == "capture_coordinate_display" {
        do {
            let request = try decodeCaptureCoordinateDisplayRPCRequestV1(data)
            let result = captureCoordinateDisplay(
                request: request.params,
                dependencies: coordinateDisplayDependencies)
            return Response(id: request.id, result: AnyCodable(result))
        } catch {
            return Response(
                id: header.id,
                result: AnyCodable(CaptureCoordinateDisplayResultV1.failed(
                    code: "invalid_request",
                    retrySafe: false)))
        }
    }

    guard let request = try? decoder.decode(Request.self, from: data) else {
        return Response(
            id: 0,
            error: ErrorInfo(code: -1, message: "Invalid JSON request"))
    }
    return dispatch(
        id: request.id,
        method: request.method,
        params: request.params ?? emptyParams())
}

func dispatch(
    id: Int64,
    method: String,
    params: Params,
    displayTopologyProvider: () throws -> DisplayTopologyV1 = {
        try liveDisplayTopologyService.observe()
    }
) -> Response {
    switch method {
    case "ping":
        return Response(id: id, result: AnyCodable(["ok": true]))

    case "display_topology":
        do {
            let topology = try displayTopologyProvider()
            try topology.validate()
            return Response(id: id, result: AnyCodable(topology))
        } catch {
            return Response(id: id, error: ErrorInfo(
                code: -1,
                message: "Cannot collect display topology: \(error)"))
        }

    case "capture_coordinate_window":
        do {
            let request = try CaptureCoordinateWindowRequestV1(params: params)
            let result = captureCoordinateWindow(
                request: request,
                dependencies: productionCaptureCoordinateWindowDependencies)
            return Response(id: id, result: AnyCodable(result))
        } catch {
            return Response(id: id, result: AnyCodable(
                CaptureCoordinateWindowResultV1.failed(
                    code: "invalid_request",
                    retrySafe: false)))
        }

    case "read_tree":
        let pid = params.pid ?? frontmostPID()
        guard pid > 0 else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Cannot determine frontmost application"))
        }
        let budget: Int
        if let sb = params.semanticBudget {
            budget = sb
        } else if let md = params.maxDepth {
            budget = md * 6 // backward compat heuristic
        } else {
            budget = 25
        }
        let filter = params.filter ?? "all"
        guard let result = readTree(pid: pid, budget: budget, filter: filter) else {
            return Response(id: id, error: ErrorInfo(
                code: -1,
                message: "No Accessibility window found for pid \(pid)"))
        }
        return Response(id: id, result: AnyCodable(result))

    case "read_window_target":
        let pid = params.pid ?? frontmostPID()
        guard pid > 0 else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Cannot determine frontmost application"))
        }
        guard let result = readCoordinateWindowTarget(pid: pid) else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "No normal coordinate window found for pid \(pid)"))
        }
        return Response(id: id, result: AnyCodable(result))

    case "semantic_press":
        do {
            let request = try SemanticPressRequest(
                pid: params.pid,
                windowID: params.windowID,
                path: params.path,
                expectedRole: params.expectedRole,
                expectedFingerprint: params.expectedFingerprint,
                fallbackPolicy: params.fallbackPolicy)
            let result = runSemanticPress(
                request,
                dependencies: productionSemanticPressDependencies())
            return Response(id: id, result: AnyCodable(result))
        } catch {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Invalid semantic_press request: \(error)"))
        }

    case "click", "press":
        guard let pid = params.pid, let path = params.path else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "\(method) requires 'pid' and 'path'"))
        }
        let (result, err) = performClick(pid: pid, path: path, expectedRole: params.expectedRole)
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "set_value":
        guard let pid = params.pid, let path = params.path, let value = params.value else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "set_value requires 'pid', 'path', and 'value'"))
        }
        let (result, err) = setValue(pid: pid, path: path, value: value, expectedRole: params.expectedRole)
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "get_value":
        guard let pid = params.pid, let path = params.path else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "get_value requires 'pid' and 'path'"))
        }
        let (result, err) = getValue(pid: pid, path: path)
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "find":
        let pid = params.pid ?? frontmostPID()
        guard pid > 0 else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Cannot determine target app"))
        }
        let results = findElements(pid: pid, query: params.query, role: params.role, identifier: params.identifier)
        return Response(id: id, result: AnyCodable(results))

    case "resolve_pid":
        guard let appName = params.appName else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "resolve_pid requires 'app_name'"))
        }
        guard let pid = resolvePID(appName: appName) else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "App '\(appName)' not found or not running"))
        }
        return Response(id: id, result: AnyCodable(["pid": pid]))

    case "resolve_app_identity":
        guard let appName = params.appName else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "resolve_app_identity requires 'app_name'"))
        }
        let (result, err) = FocusManager.resolveAppIdentity(
            appName: appName,
            excludedPIDs: Set(params.excludedPIDs ?? []))
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "prepare_task_app":
        guard let appName = params.appName,
              let bundleID = params.bundleID else {
            return Response(id: id, error: ErrorInfo(
                code: -1,
                message: "prepare_task_app requires 'app_name' and 'bundle_id'"))
        }
        let (result, err) = FocusManager.prepareTaskApp(
            appName: appName,
            expectedBundleID: bundleID,
            expectedPID: params.pid,
            excludedPIDs: Set(params.excludedPIDs ?? []))
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "bind_background_task_app":
        guard let appName = params.appName,
              let bundleID = params.bundleID else {
            return Response(id: id, error: ErrorInfo(
                code: -1,
                message: "bind_background_task_app requires 'app_name' and 'bundle_id'"))
        }
        let (result, err) = FocusManager.bindBackgroundTaskApp(
            appName: appName,
            expectedBundleID: bundleID,
            expectedPID: params.pid,
            excludedPIDs: Set(params.excludedPIDs ?? []))
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "mouse_event":
        guard let type = params.type, let x = params.x, let y = params.y else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "mouse_event requires 'type', 'x', 'y'"))
        }
        let (result, err) = InputDriver.mouseEvent(
            type: type, x: x, y: y,
            button: params.button ?? "left",
            clicks: params.clicks ?? 1
        )
        if let err = err { return Response(id: id, error: err) }
        var r = result!
        r.context = currentContext(pid: frontmostPID())
        return Response(id: id, result: AnyCodable(r))

    case "key_event":
        guard let key = params.key else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "key_event requires 'key'"))
        }
        let (result, err) = InputDriver.keyEvent(key: key, modifiers: params.modifiers ?? [])
        if let err = err { return Response(id: id, error: err) }
        var r = result!
        r.context = currentContext(pid: frontmostPID())
        return Response(id: id, result: AnyCodable(r))

    case "type_text":
        guard let text = params.value else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "type_text requires 'value'"))
        }
        let (result, err) = InputDriver.typeText(text)
        if let err = err { return Response(id: id, error: err) }
        var r = result!
        r.context = currentContext(pid: frontmostPID())
        return Response(id: id, result: AnyCodable(r))

    case "scroll":
        let dx = params.dx ?? 0
        let dy = params.dy ?? 0
        let pid = params.pid ?? frontmostPID()
        let (result, err) = performScroll(pid: pid, path: params.path, dx: dx, dy: dy)
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "focus":
        guard let appName = params.appName else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "focus requires 'app_name'"))
        }
        let (result, err) = FocusManager.focusApp(
            appName: appName,
            windowTitle: params.windowTitle,
            verify: params.verify ?? false,
            expectedPID: params.pid,
            expectedBundleID: params.bundleID,
            excludedPIDs: Set(params.excludedPIDs ?? [])
        )
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "launch_app":
        guard let appName = params.appName else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "launch_app requires 'app_name'"))
        }
        let (result, err) = FocusManager.launchApp(appName: appName)
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "frontmost":
        let (result, err) = FocusManager.frontmost()
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "list_windows":
        let pid = params.pid ?? frontmostPID()
        guard pid > 0 else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Cannot determine target app"))
        }
        let windows = FocusManager.listWindows(pid: pid)
        return Response(id: id, result: AnyCodable(windows))

    case "wait_for":
        guard let condition = params.condition else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "wait_for requires 'condition'"))
        }
        let pid = params.pid ?? frontmostPID()
        guard pid > 0 else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Cannot determine target app"))
        }
        let timeout = params.timeout ?? 10.0
        let interval = params.interval ?? 0.5
        let (result, err) = waitFor(
            pid: pid, condition: condition, value: params.value,
            query: params.query, role: params.role,
            timeout: timeout, interval: interval
        )
        if let err = err { return Response(id: id, error: err) }
        return Response(id: id, result: AnyCodable(result!))

    case "annotate":
        let pid = params.pid ?? frontmostPID()
        guard pid > 0 else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "Cannot determine frontmost application"))
        }
        let maxLabels = params.maxLabels ?? 50
        guard let result = annotateElements(pid: pid, roles: params.roles, maxLabels: maxLabels) else {
            return Response(id: id, error: ErrorInfo(
                code: -1,
                message: "No Accessibility window found for pid \(pid)"))
        }
        return Response(id: id, result: AnyCodable(result))

    case "capture_window":
        let expectedBounds = params.expectedQuartzBounds.map {
            AXFrame(x: $0.x, y: $0.y, width: $0.width, height: $0.height)
        }
        let result = captureWindow(
            pid: params.pid,
            appName: params.appName,
            windowTitle: params.windowTitle,
            windowID: params.windowID,
            expectedBounds: expectedBounds,
            signatureRoles: params.roles,
            signatureMaxLabels: params.maxLabels ?? 50)
        return Response(id: id, result: AnyCodable(result))

    case "check_permissions":
        let status = checkAllPermissions()
        return Response(id: id, result: AnyCodable(status))

    case "request_permission":
        guard let permission = params.value else {
            return Response(id: id, error: ErrorInfo(code: -1, message: "request_permission requires 'value' (permission name)"))
        }
        let result = requestPermissionCLI(permission)
        return Response(id: id, result: AnyCodable(result))

    default:
        return Response(id: id, error: ErrorInfo(code: -1, message: "Unknown method: \(method)"))
    }
}

// MARK: - Helpers

func frontmostPID() -> Int {
    refreshAppKitState()
    guard let app = NSWorkspace.shared.frontmostApplication else { return 0 }
    return Int(app.processIdentifier)
}

func writeResponse(_ resp: Response) {
    if let data = try? encoder.encode(resp),
       var str = String(data: data, encoding: .utf8) {
        str += "\n"
        FileHandle.standardOutput.write(str.data(using: .utf8)!)
    }
}

// MARK: - Permission Checks (CLI Mode)

func checkAllPermissions() -> [String: String] {
    return [
        "accessibility": AXIsProcessTrusted() ? "granted" : "denied",
        "screen_recording": checkScreenRecording(),
        "automation": checkAutomation(),
    ]
}

func checkScreenRecording() -> String {
    // CGPreflightScreenCaptureAccess() returns true if granted, false otherwise.
    // Available macOS 10.15+. Does NOT trigger a prompt.
    if CGPreflightScreenCaptureAccess() {
        return "granted"
    }
    return "denied"
}

func checkAutomation() -> String {
    // Use AEDeterminePermissionToAutomateTarget to check Automation permission
    // WITHOUT triggering a consent dialog. Available macOS 10.14+.
    // We check permission to send events to System Events (com.apple.systemevents).

    // Ensure System Events is running — AEDeterminePermissionToAutomateTarget
    // returns procNotFound (-600) if the target app isn't running.
    // System Events is a background-only daemon, safe to launch.
    ensureSystemEventsRunning()

    let addressDesc = NSAppleEventDescriptor(bundleIdentifier: "com.apple.systemevents")
    let status = AEDeterminePermissionToAutomateTarget(
        addressDesc.aeDesc,     // target
        typeWildCard,           // theAEEventClass
        typeWildCard,           // theAEEventID
        false                   // askUserIfNeeded: false = passive check, no prompt
    )

    switch status {
    case noErr:
        return "granted"
    case OSStatus(errAEEventNotPermitted):
        return "denied"
    case OSStatus(-1744): // errAEEventWouldRequireUserConsent
        return "denied"
    case OSStatus(procNotFound):
        // System Events still not running after launch attempt
        return "unknown"
    default:
        return "unknown"
    }
}

func ensureSystemEventsRunning() {
    refreshAppKitState()
    let running = NSWorkspace.shared.runningApplications.contains {
        $0.bundleIdentifier == "com.apple.systemevents"
    }
    if running { return }

    guard let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.apple.systemevents") else {
        return
    }
    let config = NSWorkspace.OpenConfiguration()
    config.activates = false
    let sem = DispatchSemaphore(value: 0)
    NSWorkspace.shared.openApplication(at: url, configuration: config) { _, _ in
        sem.signal()
    }
    _ = sem.wait(timeout: .now() + 2.0)
}

/// Opens System Settings at the given Privacy & Security anchor; falls back
/// to the Privacy & Security root if the anchor deep-link is rejected
/// (anchor names have drifted across macOS versions).
func openPrivacySettingsPane(_ anchor: String) {
    let pane = URL(string: "x-apple.systempreferences:com.apple.preference.security?\(anchor)")!
    if !NSWorkspace.shared.open(pane) {
        NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security")!)
    }
}

func requestPermissionCLI(_ permission: String) -> [String: String] {
    switch permission {
    case "accessibility":
        // AXIsProcessTrustedWithOptions with prompt: true opens System Settings
        // to the Accessibility pane and highlights this app.
        let opts = [kAXTrustedCheckOptionPrompt.takeUnretainedValue(): true] as CFDictionary
        let granted = AXIsProcessTrustedWithOptions(opts)
        return [
            "permission": "accessibility",
            "status": granted ? "granted" : "prompted",
            "message": granted ? "" : "Permission dialog shown. Enable in: System Settings > Privacy & Security > Accessibility",
        ]

    case "screen_recording":
        // CGRequestScreenCaptureAccess() shows the consent dialog only the
        // FIRST time ever (no TCC entry). Once the user denied or dismissed
        // it, every later call silently returns false with no dialog — the
        // only recovery is toggling the app in System Settings. Open the
        // Screen Recording pane as a fallback so a user-initiated request
        // always has a visible effect (mirrors how the accessibility prompt
        // guides users to System Settings).
        //
        // Known trade-off: the call returns false immediately even on a
        // genuine first request (it does not block on the user's choice), so
        // in that one case the consent dialog and System Settings open
        // together. No public API distinguishes "never prompted" from
        // "already denied" (CGPreflightScreenCaptureAccess is false for
        // both), so the extra Settings window on the first attempt is the
        // accepted cost of making the dead state visible.
        let granted = CGRequestScreenCaptureAccess()
        if !granted {
            openPrivacySettingsPane("Privacy_ScreenCapture")
        }
        return [
            "permission": "screen_recording",
            "status": granted ? "granted" : "requires_settings",
            "message": granted ? "" : "Enable \"Kocoro AX\" in System Settings > Privacy & Security > Screen Recording, then check again.",
        ]

    case "automation":
        // Trigger the "wants to control" dialog by attempting an Apple Event.
        let script = NSAppleScript(source: """
            tell application "System Events" to get name of first process whose frontmost is true
        """)
        var errorInfo: NSDictionary?
        script?.executeAndReturnError(&errorInfo)
        let granted = errorInfo == nil
        return [
            "permission": "automation",
            "status": granted ? "granted" : "prompted",
            "message": granted ? "" : "Permission dialog shown. Enable in: System Settings > Privacy & Security > Automation",
        ]

    default:
        return [
            "permission": permission,
            "status": "unknown",
            "message": "Unsupported permission: \(permission)",
        ]
    }
}
