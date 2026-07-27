import AppKit
import CoreGraphics
import CryptoKit
import Foundation

private struct CoordinateDragRectPayloadV1: Codable, Equatable {
    let value: DisplayTopologyRectV1

    init(_ value: DisplayTopologyRectV1) { self.value = value }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["x", "y", "width", "height"],
            field: "expected_window_quartz_bounds")
        value = .init(
            x: try container.decode(Double.self, forKey: strictMutationKey("x")),
            y: try container.decode(Double.self, forKey: strictMutationKey("y")),
            width: try container.decode(Double.self, forKey: strictMutationKey("width")),
            height: try container.decode(Double.self, forKey: strictMutationKey("height")))
        do { try value.validate(field: "expected_window_quartz_bounds") }
        catch { throw StrictMutationWireError.invalid("invalid expected_window_quartz_bounds") }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(value.x, forKey: strictMutationKey("x"))
        try container.encode(value.y, forKey: strictMutationKey("y"))
        try container.encode(value.width, forKey: strictMutationKey("width"))
        try container.encode(value.height, forKey: strictMutationKey("height"))
    }
}

private let coordinateDragMaximumWaypointsV1 = 48

struct CoordinateDragWaypointV1: Codable, Equatable {
    let displayID: UInt32
    let quartzPoint: CoordinateMouseEventPointV1

    init(displayID: UInt32, quartzPoint: CoordinateMouseEventPointV1) {
        self.displayID = displayID
        self.quartzPoint = quartzPoint
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["display_id", "quartz_point"],
            field: "coordinate_drag waypoint")
        let rawDisplayID = try container.decode(
            Int.self, forKey: strictMutationKey("display_id"))
        quartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self,
            forKey: strictMutationKey("quartz_point"))
        guard let exactDisplayID = UInt32(exactly: rawDisplayID),
              exactDisplayID > 0 else {
            throw StrictMutationWireError.invalid(
                "coordinate_drag waypoint display authority is required")
        }
        displayID = exactDisplayID
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(displayID, forKey: strictMutationKey("display_id"))
        try container.encode(quartzPoint, forKey: strictMutationKey("quartz_point"))
    }
}

struct CoordinateDragRequestV1: Codable, Equatable {
    let schemaVersion: Int
    let topologyRef: CoordinateMouseEventTopologyRefV1
    let helperBootID: String
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let expectedWindowQuartzBounds: DisplayTopologyRectV1
    let startDisplayID: UInt32
    let endDisplayID: UInt32
    let startQuartzPoint: CoordinateMouseEventPointV1
    let endQuartzPoint: CoordinateMouseEventPointV1
    let waypoints: [CoordinateDragWaypointV1]
    let button: String
    let modifiers: [String]
    let durationMS: Int
    let endTargetPolicy: String
    let commitDeadlineAt: String

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "topology_ref", "helper_boot_id", "pid", "bundle_id",
            "window_id", "expected_window_quartz_bounds", "start_display_id",
            "end_display_id", "start_quartz_point", "end_quartz_point", "button",
            "waypoints", "modifiers", "duration_ms", "end_target_policy",
            "commit_deadline_at",
        ], field: "coordinate_drag params")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        topologyRef = try container.decode(
            CoordinateMouseEventTopologyRefV1.self, forKey: strictMutationKey("topology_ref"))
        helperBootID = try container.decode(String.self, forKey: strictMutationKey("helper_boot_id"))
        pid = try container.decode(Int.self, forKey: strictMutationKey("pid"))
        bundleID = try container.decode(String.self, forKey: strictMutationKey("bundle_id"))
        let rawWindowID = try container.decode(Int.self, forKey: strictMutationKey("window_id"))
        let rawStartDisplayID = try container.decode(Int.self, forKey: strictMutationKey("start_display_id"))
        let rawEndDisplayID = try container.decode(Int.self, forKey: strictMutationKey("end_display_id"))
        expectedWindowQuartzBounds = try container.decode(
            CoordinateDragRectPayloadV1.self,
            forKey: strictMutationKey("expected_window_quartz_bounds")).value
        startQuartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self, forKey: strictMutationKey("start_quartz_point"))
        endQuartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self, forKey: strictMutationKey("end_quartz_point"))
        waypoints = try container.decode(
            [CoordinateDragWaypointV1].self,
            forKey: strictMutationKey("waypoints"))
        button = try container.decode(String.self, forKey: strictMutationKey("button"))
        modifiers = try container.decode(
            [String].self, forKey: strictMutationKey("modifiers"))
        durationMS = try container.decode(Int.self, forKey: strictMutationKey("duration_ms"))
        endTargetPolicy = try container.decode(
            String.self, forKey: strictMutationKey("end_target_policy"))
        commitDeadlineAt = try container.decode(
            String.self, forKey: strictMutationKey("commit_deadline_at"))
        guard schemaVersion == 1,
              strictMutationIdentity(helperBootID), pid > 0,
              strictMutationIdentity(bundleID),
              let exactWindowID = UInt32(exactly: rawWindowID), exactWindowID > 0,
              let exactStartDisplayID = UInt32(exactly: rawStartDisplayID), exactStartDisplayID > 0,
              let exactEndDisplayID = UInt32(exactly: rawEndDisplayID), exactEndDisplayID > 0,
              strictInputModifiersV1(modifiers),
              button == "left", (120...800).contains(durationMS),
              (2...coordinateDragMaximumWaypointsV1).contains(waypoints.count),
              waypoints.first == CoordinateDragWaypointV1(
                displayID: exactStartDisplayID, quartzPoint: startQuartzPoint),
              waypoints.last == CoordinateDragWaypointV1(
                displayID: exactEndDisplayID, quartzPoint: endQuartzPoint),
              zip(waypoints, waypoints.dropFirst()).allSatisfy({
                left, right in left.quartzPoint != right.quartzPoint
              }),
              endTargetPolicy == "same_window",
              strictMutationIdentity(commitDeadlineAt),
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid("invalid coordinate_drag authority or policy")
        }
        windowID = exactWindowID
        startDisplayID = exactStartDisplayID
        endDisplayID = exactEndDisplayID
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(topologyRef, forKey: strictMutationKey("topology_ref"))
        try container.encode(helperBootID, forKey: strictMutationKey("helper_boot_id"))
        try container.encode(pid, forKey: strictMutationKey("pid"))
        try container.encode(bundleID, forKey: strictMutationKey("bundle_id"))
        try container.encode(windowID, forKey: strictMutationKey("window_id"))
        try container.encode(
            CoordinateDragRectPayloadV1(expectedWindowQuartzBounds),
            forKey: strictMutationKey("expected_window_quartz_bounds"))
        try container.encode(startDisplayID, forKey: strictMutationKey("start_display_id"))
        try container.encode(endDisplayID, forKey: strictMutationKey("end_display_id"))
        try container.encode(startQuartzPoint, forKey: strictMutationKey("start_quartz_point"))
        try container.encode(endQuartzPoint, forKey: strictMutationKey("end_quartz_point"))
        try container.encode(waypoints, forKey: strictMutationKey("waypoints"))
        try container.encode(button, forKey: strictMutationKey("button"))
        try container.encode(modifiers, forKey: strictMutationKey("modifiers"))
        try container.encode(durationMS, forKey: strictMutationKey("duration_ms"))
        try container.encode(endTargetPolicy, forKey: strictMutationKey("end_target_policy"))
        try container.encode(commitDeadlineAt, forKey: strictMutationKey("commit_deadline_at"))
    }
}

struct CoordinateDragRPCRequestV1: Codable, Equatable {
    let id: Int64
    let method: String
    let params: CoordinateDragRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"], field: "coordinate_drag envelope")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(CoordinateDragRequestV1.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "coordinate_drag" else {
            throw StrictMutationWireError.invalid("invalid coordinate_drag envelope")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(id, forKey: strictMutationKey("id"))
        try container.encode(method, forKey: strictMutationKey("method"))
        try container.encode(params, forKey: strictMutationKey("params"))
    }
}

func decodeCoordinateDragRPCRequestV1(_ payload: Data) throws -> CoordinateDragRPCRequestV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(CoordinateDragRPCRequestV1.self, from: payload)
}

struct CoordinateDragResultV1: Codable, Equatable {
    let schemaVersion: Int
    let status: String
    let dragCommitted: Bool
    let mouseDownCommitted: Bool
    let pointerMotionCommitted: Bool
    let mouseUpCommitted: Bool
    let possibleDropSideEffect: Bool
    let phase: String
    let failureCode: String?
    let retrySafe: Bool
    let postcondition: String?
    let pointerEndpoint: CoordinateMouseEventPointerEndpointV1?

    init(
        status: String, dragCommitted: Bool, mouseDownCommitted: Bool,
        pointerMotionCommitted: Bool, mouseUpCommitted: Bool,
        possibleDropSideEffect: Bool, phase: String, failureCode: String?,
        postcondition: String?, pointerEndpoint: CoordinateMouseEventPointerEndpointV1?
    ) {
        schemaVersion = 1
        self.status = status
        self.dragCommitted = dragCommitted
        self.mouseDownCommitted = mouseDownCommitted
        self.pointerMotionCommitted = pointerMotionCommitted
        self.mouseUpCommitted = mouseUpCommitted
        self.possibleDropSideEffect = possibleDropSideEffect
        self.phase = phase
        self.failureCode = failureCode
        retrySafe = false
        self.postcondition = postcondition
        self.pointerEndpoint = pointerEndpoint
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "status", "drag_committed", "mouse_down_committed",
            "pointer_motion_committed", "mouse_up_committed", "possible_drop_side_effect",
            "phase", "failure_code", "retry_safe", "postcondition", "pointer_endpoint",
        ], field: "coordinate_drag result")
        schemaVersion = try container.decode(Int.self, forKey: strictMutationKey("schema_version"))
        status = try container.decode(String.self, forKey: strictMutationKey("status"))
        dragCommitted = try container.decode(Bool.self, forKey: strictMutationKey("drag_committed"))
        mouseDownCommitted = try container.decode(Bool.self, forKey: strictMutationKey("mouse_down_committed"))
        pointerMotionCommitted = try container.decode(Bool.self, forKey: strictMutationKey("pointer_motion_committed"))
        mouseUpCommitted = try container.decode(Bool.self, forKey: strictMutationKey("mouse_up_committed"))
        possibleDropSideEffect = try container.decode(Bool.self, forKey: strictMutationKey("possible_drop_side_effect"))
        phase = try container.decode(String.self, forKey: strictMutationKey("phase"))
        failureCode = try container.decodeIfPresent(String.self, forKey: strictMutationKey("failure_code"))
        retrySafe = try container.decode(Bool.self, forKey: strictMutationKey("retry_safe"))
        postcondition = try container.decodeIfPresent(String.self, forKey: strictMutationKey("postcondition"))
        pointerEndpoint = try container.decodeIfPresent(
            CoordinateMouseEventPointerEndpointV1.self,
            forKey: strictMutationKey("pointer_endpoint"))
        guard validateCoordinateDragResult(self) else {
            throw StrictMutationWireError.invalid("invalid coordinate_drag result tagged union")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(status, forKey: strictMutationKey("status"))
        try container.encode(dragCommitted, forKey: strictMutationKey("drag_committed"))
        try container.encode(mouseDownCommitted, forKey: strictMutationKey("mouse_down_committed"))
        try container.encode(pointerMotionCommitted, forKey: strictMutationKey("pointer_motion_committed"))
        try container.encode(mouseUpCommitted, forKey: strictMutationKey("mouse_up_committed"))
        try container.encode(possibleDropSideEffect, forKey: strictMutationKey("possible_drop_side_effect"))
        try container.encode(phase, forKey: strictMutationKey("phase"))
        try container.encode(failureCode, forKey: strictMutationKey("failure_code"))
        try container.encode(retrySafe, forKey: strictMutationKey("retry_safe"))
        try container.encode(postcondition, forKey: strictMutationKey("postcondition"))
        try container.encode(pointerEndpoint, forKey: strictMutationKey("pointer_endpoint"))
    }
}

func decodeCoordinateDragResultV1(_ payload: Data) throws -> CoordinateDragResultV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(CoordinateDragResultV1.self, from: payload)
}

private func validateCoordinateDragResult(_ result: CoordinateDragResultV1) -> Bool {
    guard result.schemaVersion == 1, !result.retrySafe else { return false }
    if let endpoint = result.pointerEndpoint {
        let geometryVerified = endpoint.observed.map {
            abs(endpoint.requested.x - $0.x) <= coordinateDragPointerToleranceV1 &&
                abs(endpoint.requested.y - $0.y) <= coordinateDragPointerToleranceV1
        } ?? false
        guard endpoint.tolerance == coordinateDragPointerToleranceV1,
              endpoint.verified == geometryVerified else { return false }
    }
    switch result.status {
    case "completed_unverified":
        guard result.dragCommitted && result.mouseDownCommitted &&
                result.possibleDropSideEffect && result.phase == "post_verification" &&
                result.postcondition == nil && result.pointerEndpoint != nil else { return false }
        if result.mouseUpCommitted {
            if result.failureCode == "interference_detection_unavailable" {
                return true
            }
            if result.pointerEndpoint?.verified == true {
                return result.pointerMotionCommitted &&
                    result.failureCode == "drop_postcondition_not_declared"
            }
            return result.pointerMotionCommitted &&
                result.failureCode == "pointer_endpoint_not_verified"
        }
        return result.failureCode == "mouse_up_post_unverified"
    case "user_interference":
        let allowed = Set([
            "pointer_interference", "cancelled_during_drag",
            "request_expired_during_drag", "drag_event_post_failed",
            "end_target_changed", "cancelled_before_drop",
            "request_expired_before_drop",
            "topology_unavailable", "stale_topology", "helper_boot_mismatch",
            "process_not_live", "process_identity_mismatch", "window_not_found",
            "window_identity_mismatch", "window_not_actionable", "window_bounds_mismatch",
            "start_outside_window", "end_outside_window",
            "waypoint_outside_window", "waypoint_display_not_actionable",
            "start_display_not_actionable", "end_display_not_actionable",
            "physical_input_interference", "interference_detection_unavailable",
        ])
        let preCommit = !result.dragCommitted && !result.mouseDownCommitted &&
            !result.pointerMotionCommitted && !result.mouseUpCommitted &&
            !result.possibleDropSideEffect && result.phase == "user_interference" &&
            result.failureCode == "physical_input_interference" &&
            result.postcondition == nil && result.pointerEndpoint == nil
        let afterDown = result.dragCommitted && result.mouseDownCommitted && result.mouseUpCommitted &&
            result.possibleDropSideEffect && result.phase == "cleanup" &&
            result.failureCode.map(allowed.contains) == true &&
            result.postcondition == nil && result.pointerEndpoint != nil
        return preCommit || afterDown
    case "failed":
        let preflight = Set([
            "invalid_request", "request_expired", "topology_unavailable", "stale_topology",
            "helper_boot_mismatch", "process_not_live", "process_identity_mismatch",
            "window_not_found", "window_identity_mismatch", "window_not_actionable",
            "window_bounds_mismatch", "start_outside_window", "end_outside_window",
            "waypoint_outside_window", "waypoint_display_not_actionable",
            "start_display_not_actionable", "end_display_not_actionable",
            "start_point_occluded", "end_point_occluded", "cancelled",
            "interference_detection_unavailable",
        ])
        let exactPhase = result.failureCode.map { code -> Bool in
            if preflight.contains(code) { return result.phase == "preflight" }
            if code == "event_preparation_failed" { return result.phase == "preparation" }
            if code == "mouse_down_failed" { return result.phase == "action" }
            return false
        } ?? false
        return !result.dragCommitted && !result.mouseDownCommitted &&
            !result.pointerMotionCommitted && !result.mouseUpCommitted &&
            !result.possibleDropSideEffect && exactPhase && result.postcondition == nil &&
            result.pointerEndpoint == nil
    default:
        return false
    }
}

struct CoordinateDragPreparedSequence {
    let path: [CoordinateMouseEventPointV1]
    let postDown: () -> Bool
    let postDrag: (Int) -> Bool
    let postUp: (CoordinateMouseEventPointV1) -> Bool
}

struct CoordinateDragDependencies {
    let observeTopology: () throws -> DisplayTopologyV1
    let isPIDLive: (Int) -> Bool
    let bundleIDForPID: (Int) -> String?
    let exactWindow: (UInt32) throws -> CaptureCoordinateWindowWindowSnapshot?
    let frontmostWindowAtPoint: (CoordinateMouseEventPointV1) -> UInt32?
    let prepare: ([CoordinateMouseEventPointV1], String) -> CoordinateDragPreparedSequence?
    let observePointer: () -> CoordinateMouseEventPointV1?
    let observePhysicalInput: () -> PhysicalInputInterferenceSnapshotV1?
    let isCancelled: () -> Bool
    let now: () -> Date
    let sleep: (TimeInterval) -> Void
}

private let coordinateDragPointerToleranceV1 = 2.0
private let coordinateDragMaximumDeadlineHorizonV1 = 3.0
private let coordinateDragCounterSettleDelayV1 = 0.005
private let coordinateDragMaximumCounterSettleAttemptsV1 = 10

func coordinateDragAssessPhysicalInputV1(
    baseline: PhysicalInputInterferenceSnapshotV1?,
    expectedPointer: CoordinateMouseEventPointV1?,
    expectedSyntheticEvents: [(CGEventType, UInt32)] = [],
    expectedSyntheticHeldMouseButtons: UInt32,
    expectedSyntheticHeldModifierFlags: UInt64,
    observe: () -> PhysicalInputInterferenceSnapshotV1?,
    settle: () -> Void,
    maximumSettleAttempts: Int =
        coordinateDragMaximumCounterSettleAttemptsV1
) -> (
    assessment: PhysicalInputInterferenceAssessmentV1,
    snapshot: PhysicalInputInterferenceSnapshotV1?
) {
    // CGEvent delivery and the HID/synthetic counters are asynchronous. A
    // single immediate sample can therefore see the cursor/button state from
    // the committed event while its matching counter has not arrived yet.
    // Retry only the observation for a bounded 50 ms; never repost an event.
    var settleAttempt = 0
    while true {
        let current = observe()
        let assessment = assessPhysicalInputInterferenceV1(
            baseline: baseline,
            current: current,
            expectedPointer: expectedPointer,
            expectedSyntheticEvents: expectedSyntheticEvents,
            expectedSyntheticHeldMouseButtons:
                expectedSyntheticHeldMouseButtons,
            expectedSyntheticHeldModifierFlags:
                expectedSyntheticHeldModifierFlags
        )
        if assessment != .unavailable ||
            settleAttempt >= max(0, maximumSettleAttempts) {
            return (assessment, current)
        }
        settleAttempt += 1
        settle()
    }
}

/// Kocoro-owned bounded minimum-jerk interpolation. The scalar polynomial
/// p(t)=10t^3-15t^4+6t^5 has zero velocity and acceleration at both ends.
/// It is a standard trajectory formula; no Peekaboo source code is copied.
func coordinateDragMinimumJerkPath(
    start: CoordinateMouseEventPointV1,
    end: CoordinateMouseEventPointV1,
    durationMS: Int
) -> [CoordinateMouseEventPointV1] {
    coordinateDragMinimumJerkPolylinePath(
        waypoints: [start, end], durationMS: durationMS)
}

/// Preserves every provider waypoint while bounding the pre-created event
/// sequence to the same 48-sample budget as the two-point helper. Additional
/// samples are assigned to longer segments; every segment still receives at
/// least one interval, so no provider waypoint is silently collapsed.
func coordinateDragMinimumJerkPolylinePath(
    waypoints: [CoordinateMouseEventPointV1],
    durationMS: Int
) -> [CoordinateMouseEventPointV1] {
    guard (2...coordinateDragMaximumWaypointsV1).contains(waypoints.count) else {
        return []
    }
    let regularSamples = max(
        8, Int(ceil(Double(durationMS) / (1000.0 / 60.0))) + 1)
    let sampleCount = min(
        coordinateDragMaximumWaypointsV1,
        max(waypoints.count, regularSamples))
    var segmentIntervals = Array(repeating: 1, count: waypoints.count - 1)
    var remaining = sampleCount - waypoints.count
    let lengths = zip(waypoints, waypoints.dropFirst()).map { start, end in
        hypot(end.x - start.x, end.y - start.y)
    }
    while remaining > 0 {
        let next = segmentIntervals.indices.max { left, right in
            let leftScore = lengths[left] / Double(segmentIntervals[left])
            let rightScore = lengths[right] / Double(segmentIntervals[right])
            if leftScore == rightScore { return left > right }
            return leftScore < rightScore
        }!
        segmentIntervals[next] += 1
        remaining -= 1
    }

    var result: [CoordinateMouseEventPointV1] = []
    result.reserveCapacity(sampleCount)
    for segment in segmentIntervals.indices {
        let start = waypoints[segment]
        let end = waypoints[segment + 1]
        let intervals = segmentIntervals[segment]
        for sample in 0...intervals where segment == 0 || sample > 0 {
            let t = Double(sample) / Double(intervals)
            let t2 = t * t
            let t3 = t2 * t
            let progress = 10 * t3 - 15 * t3 * t + 6 * t3 * t2
            result.append(.init(
                x: start.x + (end.x - start.x) * progress,
                y: start.y + (end.y - start.y) * progress))
        }
    }
    return result
}

private func dragRectEqual(_ a: DisplayTopologyRectV1, _ b: DisplayTopologyRectV1) -> Bool {
    abs(a.x - b.x) < 0.000_001 && abs(a.y - b.y) < 0.000_001 &&
        abs(a.width - b.width) < 0.000_001 && abs(a.height - b.height) < 0.000_001
}

private func dragContains(_ rect: DisplayTopologyRectV1, _ point: CoordinateMouseEventPointV1) -> Bool {
    point.x >= rect.x && point.y >= rect.y &&
        point.x < rect.x + rect.width && point.y < rect.y + rect.height
}

private func dragPointMatches(_ expected: CoordinateMouseEventPointV1, _ observed: CoordinateMouseEventPointV1?) -> Bool {
    guard let observed else { return false }
    return abs(expected.x - observed.x) <= coordinateDragPointerToleranceV1 &&
        abs(expected.y - observed.y) <= coordinateDragPointerToleranceV1
}

private func dragEndpoint(
    requested: CoordinateMouseEventPointV1, observed: CoordinateMouseEventPointV1?
) -> CoordinateMouseEventPointerEndpointV1 {
    .init(
        requested: requested, observed: observed, tolerance: coordinateDragPointerToleranceV1,
        verified: dragPointMatches(requested, observed))
}

private func coordinateDragFailure(_ code: String, phase: String = "preflight") -> CoordinateDragResultV1 {
    .init(
        status: "failed", dragCommitted: false, mouseDownCommitted: false,
        pointerMotionCommitted: false, mouseUpCommitted: false,
        possibleDropSideEffect: false, phase: phase, failureCode: code,
        postcondition: nil, pointerEndpoint: nil)
}

private func coordinateDragPrecommitUserInterference() -> CoordinateDragResultV1 {
    .init(
        status: "user_interference", dragCommitted: false, mouseDownCommitted: false,
        pointerMotionCommitted: false, mouseUpCommitted: false,
        possibleDropSideEffect: false, phase: "user_interference",
        failureCode: "physical_input_interference",
        postcondition: nil, pointerEndpoint: nil)
}

private func coordinateDragAuthorityFailure(
    request: CoordinateDragRequestV1,
    dependencies: CoordinateDragDependencies,
    checkStartHit: Bool,
    checkEndHit: Bool
) -> String? {
    let topology: DisplayTopologyV1
    do { topology = try dependencies.observeTopology(); try topology.validate() }
    catch { return "topology_unavailable" }
    guard topology.topologyID == request.topologyRef.topologyID,
          topology.generation == request.topologyRef.generation else { return "stale_topology" }
    guard topology.helperBootID == request.helperBootID else { return "helper_boot_mismatch" }
    guard dependencies.isPIDLive(request.pid) else { return "process_not_live" }
    guard dependencies.bundleIDForPID(request.pid) == request.bundleID else {
        return "process_identity_mismatch"
    }
    let window: CaptureCoordinateWindowWindowSnapshot
    do {
        guard let exact = try dependencies.exactWindow(request.windowID) else {
            return "window_not_found"
        }
        window = exact
    } catch { return "window_not_found" }
    guard window.windowID == request.windowID, window.ownerPID == request.pid else {
        return "window_identity_mismatch"
    }
    guard window.layer == 0, window.isOnScreen else { return "window_not_actionable" }
    guard dragRectEqual(window.bounds, request.expectedWindowQuartzBounds) else {
        return "window_bounds_mismatch"
    }
    guard dragContains(window.bounds, request.startQuartzPoint) else { return "start_outside_window" }
    guard dragContains(window.bounds, request.endQuartzPoint) else { return "end_outside_window" }
    guard let startDisplay = topology.displays.first(where: { $0.displayID == request.startDisplayID }),
          startDisplay.isActive, startDisplay.isOnline, !startDisplay.isAsleep,
          startDisplay.mirrorMasterDisplayID == nil, startDisplay.rotationDegrees == 0,
          dragContains(startDisplay.quartzBounds, request.startQuartzPoint) else {
        return "start_display_not_actionable"
    }
    guard let endDisplay = topology.displays.first(where: { $0.displayID == request.endDisplayID }),
          endDisplay.isActive, endDisplay.isOnline, !endDisplay.isAsleep,
          endDisplay.mirrorMasterDisplayID == nil, endDisplay.rotationDegrees == 0,
          dragContains(endDisplay.quartzBounds, request.endQuartzPoint) else {
        return "end_display_not_actionable"
    }
    for waypoint in request.waypoints.dropFirst().dropLast() {
        guard dragContains(window.bounds, waypoint.quartzPoint) else {
            return "waypoint_outside_window"
        }
        guard let display = topology.displays.first(where: {
                  $0.displayID == waypoint.displayID
              }),
              display.isActive, display.isOnline, !display.isAsleep,
              display.mirrorMasterDisplayID == nil, display.rotationDegrees == 0,
              dragContains(display.quartzBounds, waypoint.quartzPoint) else {
            return "waypoint_display_not_actionable"
        }
    }
    if checkStartHit,
       dependencies.frontmostWindowAtPoint(request.startQuartzPoint) != request.windowID {
        return "start_point_occluded"
    }
    if checkEndHit,
       dependencies.frontmostWindowAtPoint(request.endQuartzPoint) != request.windowID {
        return "end_point_occluded"
    }
    return nil
}

private func cleanupCoordinateDrag(
    request: CoordinateDragRequestV1,
    prepared: CoordinateDragPreparedSequence,
    dependencies: CoordinateDragDependencies,
    code: String,
    pointerMotionCommitted: Bool,
    lastKnownPoint: CoordinateMouseEventPointV1
) -> CoordinateDragResultV1 {
    let current = dependencies.observePointer() ?? lastKnownPoint
    var upPosted = false
    // A pre-created mouseUp is safe to post more than once. Retry a bounded
    // number of times so a transient posting seam cannot leave the logical
    // button held; no key/modifier event is ever synthesized by this engine.
    for _ in 0..<3 where !upPosted {
        upPosted = prepared.postUp(current)
    }
    if !upPosted {
        return .init(
            status: "completed_unverified", dragCommitted: true,
            mouseDownCommitted: true, pointerMotionCommitted: pointerMotionCommitted,
            mouseUpCommitted: false, possibleDropSideEffect: true,
            phase: "post_verification", failureCode: "mouse_up_post_unverified",
            postcondition: nil,
            pointerEndpoint: dragEndpoint(
                requested: request.endQuartzPoint, observed: current))
    }
    if code == "interference_detection_unavailable" {
        // Monitoring loss is not evidence of physical user interference.
        // mouseUp was acknowledged, so report the real committed-but-
        // unverified boundary and let the caller take a fresh screenshot
        // without replaying the drag.
        return .init(
            status: "completed_unverified", dragCommitted: true,
            mouseDownCommitted: true, pointerMotionCommitted: pointerMotionCommitted,
            mouseUpCommitted: true, possibleDropSideEffect: true,
            phase: "post_verification",
            failureCode: code,
            postcondition: nil,
            pointerEndpoint: dragEndpoint(
                requested: request.endQuartzPoint, observed: current))
    }
    return .init(
        status: "user_interference", dragCommitted: true, mouseDownCommitted: true,
        pointerMotionCommitted: pointerMotionCommitted, mouseUpCommitted: true,
        possibleDropSideEffect: true, phase: "cleanup",
        failureCode: code,
        postcondition: nil,
        pointerEndpoint: dragEndpoint(requested: request.endQuartzPoint, observed: current))
}

func runCoordinateDrag(
    request: CoordinateDragRequestV1,
    dependencies: CoordinateDragDependencies
) -> CoordinateDragResultV1 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return coordinateDragFailure("invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else { return coordinateDragFailure("request_expired") }
    guard horizon <= coordinateDragMaximumDeadlineHorizonV1 else {
        return coordinateDragFailure("invalid_request")
    }
    if let failure = coordinateDragAuthorityFailure(
        request: request, dependencies: dependencies, checkStartHit: true, checkEndHit: true) {
        return coordinateDragFailure(failure)
    }
    let path = coordinateDragMinimumJerkPolylinePath(
        waypoints: request.waypoints.map(\.quartzPoint),
        durationMS: request.durationMS)
    guard let prepared = dependencies.prepare(path, request.button), prepared.path == path else {
        return coordinateDragFailure("event_preparation_failed", phase: "preparation")
    }
    guard !dependencies.isCancelled() else { return coordinateDragFailure("cancelled") }
    guard dependencies.now() < deadline else { return coordinateDragFailure("request_expired") }
    if let failure = coordinateDragAuthorityFailure(
        request: request, dependencies: dependencies, checkStartHit: true, checkEndHit: false) {
        return coordinateDragFailure(failure)
    }
    guard let physicalInputBeforeDown = dependencies.observePhysicalInput() else {
        return coordinateDragFailure("interference_detection_unavailable")
    }
    if assessPhysicalInputInterferenceV1(
        baseline: physicalInputBeforeDown,
        current: physicalInputBeforeDown,
        expectedPointer: nil,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue
    ) == .interference {
        return coordinateDragPrecommitUserInterference()
    }
    guard prepared.postDown() else {
        return coordinateDragFailure("mouse_down_failed", phase: "action")
    }
    var lastKnown = dependencies.observePointer() ?? request.startQuartzPoint
    guard dragPointMatches(request.startQuartzPoint, lastKnown) else {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "pointer_interference", pointerMotionCommitted: false,
            lastKnownPoint: lastKnown)
    }
    let afterDownAssessment = coordinateDragAssessPhysicalInputV1(
        baseline: physicalInputBeforeDown,
        expectedPointer: request.startQuartzPoint,
        expectedSyntheticEvents: [(.leftMouseDown, 1)],
        expectedSyntheticHeldMouseButtons: 1,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue,
        observe: dependencies.observePhysicalInput,
        settle: {
            dependencies.sleep(coordinateDragCounterSettleDelayV1)
        }
    )
    var physicalInputBaseline = afterDownAssessment.snapshot
    switch afterDownAssessment.assessment {
    case .interference:
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "physical_input_interference", pointerMotionCommitted: false,
            lastKnownPoint: lastKnown)
    case .unavailable:
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "interference_detection_unavailable", pointerMotionCommitted: false,
            lastKnownPoint: lastKnown)
    case .unchanged:
        break
    }

    let sampleDelay = Double(request.durationMS) / 1000.0 / Double(max(1, path.count - 1))
    var motionCommitted = false
    for index in 1..<path.count {
        if dependencies.isCancelled() {
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "cancelled_during_drag", pointerMotionCommitted: motionCommitted,
                lastKnownPoint: lastKnown)
        }
        if dependencies.now() >= deadline {
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "request_expired_during_drag", pointerMotionCommitted: motionCommitted,
                lastKnownPoint: lastKnown)
        }
        let beforeDragAssessment = coordinateDragAssessPhysicalInputV1(
            baseline: physicalInputBaseline,
            expectedPointer: lastKnown,
            expectedSyntheticHeldMouseButtons: 1,
            expectedSyntheticHeldModifierFlags:
                strictInputModifierFlagsV1(request.modifiers)!.rawValue,
            observe: dependencies.observePhysicalInput,
            settle: {
                dependencies.sleep(coordinateDragCounterSettleDelayV1)
            }
        )
        let physicalInputBeforeDrag = beforeDragAssessment.snapshot
        switch beforeDragAssessment.assessment {
        case .interference:
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "physical_input_interference", pointerMotionCommitted: motionCommitted,
                lastKnownPoint: lastKnown)
        case .unavailable:
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "interference_detection_unavailable", pointerMotionCommitted: motionCommitted,
                lastKnownPoint: lastKnown)
        case .unchanged:
            break
        }
        guard prepared.postDrag(index) else {
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "drag_event_post_failed", pointerMotionCommitted: motionCommitted,
                lastKnownPoint: lastKnown)
        }
        motionCommitted = true
        let observed = dependencies.observePointer()
        lastKnown = observed ?? lastKnown
        let afterDragAssessment = coordinateDragAssessPhysicalInputV1(
            baseline: physicalInputBeforeDrag,
            expectedPointer: path[index],
            expectedSyntheticEvents: [(.leftMouseDragged, 1)],
            expectedSyntheticHeldMouseButtons: 1,
            expectedSyntheticHeldModifierFlags:
                strictInputModifierFlagsV1(request.modifiers)!.rawValue,
            observe: dependencies.observePhysicalInput,
            settle: {
                dependencies.sleep(coordinateDragCounterSettleDelayV1)
            }
        )
        let physicalInputAfterDrag = afterDragAssessment.snapshot
        // Cursor delivery can lag the event post just like its counter. Bind
        // endpoint verification to a fresh cursor sample after the bounded
        // counter settle, while retaining the monitor snapshot as fallback.
        lastKnown = dependencies.observePointer() ??
            physicalInputAfterDrag?.pointer ?? observed ?? lastKnown
        switch afterDragAssessment.assessment {
        case .interference:
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "physical_input_interference", pointerMotionCommitted: true,
                lastKnownPoint: lastKnown)
        case .unavailable:
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "interference_detection_unavailable", pointerMotionCommitted: true,
                lastKnownPoint: lastKnown)
        case .unchanged:
            break
        }
        physicalInputBaseline = physicalInputAfterDrag
        guard dragPointMatches(path[index], lastKnown) else {
            return cleanupCoordinateDrag(
                request: request, prepared: prepared, dependencies: dependencies,
                code: "pointer_interference", pointerMotionCommitted: true,
                lastKnownPoint: lastKnown)
        }
        if index < path.count - 1 { dependencies.sleep(sampleDelay) }
    }

    if dependencies.isCancelled() {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "cancelled_before_drop", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    if dependencies.now() >= deadline {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "request_expired_before_drop", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    // The drag may span hundreds of milliseconds. Reuse the exact same
    // topology/process/window/display authority check immediately before the
    // irreversible mouseUp/drop instead of trusting the startup snapshot plus
    // a hit-test alone. Any drift after mouseDown is cleanup, not a safe
    // preflight failure: release at the currently observed pointer and report
    // possible_drop_side_effect through the user_interference tagged union.
    if let authorityFailure = coordinateDragAuthorityFailure(
        request: request, dependencies: dependencies,
        checkStartHit: false, checkEndHit: true
    ) {
        let code = authorityFailure == "end_point_occluded"
            ? "end_target_changed" : authorityFailure
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: code, pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    // Cancellation/deadline may arrive while the full authority snapshot is
    // being collected. Check once more at the event-commit boundary.
    if dependencies.isCancelled() {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "cancelled_before_drop", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    if dependencies.now() >= deadline {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "request_expired_before_drop", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    let beforeUpAssessment = coordinateDragAssessPhysicalInputV1(
        baseline: physicalInputBaseline,
        expectedPointer: lastKnown,
        expectedSyntheticHeldMouseButtons: 1,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue,
        observe: dependencies.observePhysicalInput,
        settle: {
            dependencies.sleep(coordinateDragCounterSettleDelayV1)
        }
    )
    let physicalInputBeforeUp = beforeUpAssessment.snapshot
    switch beforeUpAssessment.assessment {
    case .interference:
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "physical_input_interference", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    case .unavailable:
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "interference_detection_unavailable", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    case .unchanged:
        break
    }
    if dependencies.isCancelled() {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "cancelled_before_drop", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    if dependencies.now() >= deadline {
        return cleanupCoordinateDrag(
            request: request, prepared: prepared, dependencies: dependencies,
            code: "request_expired_before_drop", pointerMotionCommitted: motionCommitted,
            lastKnownPoint: lastKnown)
    }
    let upPosted = prepared.postUp(lastKnown)
    let observed = dependencies.observePointer() ?? lastKnown
    let endpoint = dragEndpoint(requested: request.endQuartzPoint, observed: observed)
    guard upPosted else {
        return .init(
            status: "completed_unverified", dragCommitted: true,
            mouseDownCommitted: true, pointerMotionCommitted: motionCommitted,
            mouseUpCommitted: false, possibleDropSideEffect: true,
            phase: "post_verification", failureCode: "mouse_up_post_unverified",
            postcondition: nil, pointerEndpoint: endpoint)
    }
    let afterUpAssessment = coordinateDragAssessPhysicalInputV1(
        baseline: physicalInputBeforeUp,
        expectedPointer: observed,
        expectedSyntheticEvents: [(.leftMouseUp, 1)],
        expectedSyntheticHeldMouseButtons: 0,
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(request.modifiers)!.rawValue,
        observe: dependencies.observePhysicalInput,
        settle: {
            dependencies.sleep(coordinateDragCounterSettleDelayV1)
        }
    )
    switch afterUpAssessment.assessment {
    case .interference:
        return .init(
            status: "user_interference", dragCommitted: true,
            mouseDownCommitted: true, pointerMotionCommitted: motionCommitted,
            mouseUpCommitted: true, possibleDropSideEffect: true,
            phase: "cleanup", failureCode: "physical_input_interference",
            postcondition: nil, pointerEndpoint: endpoint)
    case .unavailable:
        return .init(
            status: "completed_unverified", dragCommitted: true,
            mouseDownCommitted: true, pointerMotionCommitted: motionCommitted,
            mouseUpCommitted: true, possibleDropSideEffect: true,
            phase: "post_verification",
            failureCode: "interference_detection_unavailable",
            postcondition: nil, pointerEndpoint: endpoint)
    case .unchanged:
        break
    }
    if endpoint.verified {
        return .init(
            status: "completed_unverified", dragCommitted: true,
            mouseDownCommitted: true,
            pointerMotionCommitted: motionCommitted, mouseUpCommitted: true,
            possibleDropSideEffect: true, phase: "post_verification",
            failureCode: "drop_postcondition_not_declared", postcondition: nil,
            pointerEndpoint: endpoint)
    }
    return .init(
        status: "completed_unverified", dragCommitted: true,
        mouseDownCommitted: true, pointerMotionCommitted: motionCommitted,
        mouseUpCommitted: true, possibleDropSideEffect: true,
        phase: "post_verification", failureCode: "pointer_endpoint_not_verified",
        postcondition: nil, pointerEndpoint: endpoint)
}

private func productionCoordinateDragWindow(
    _ windowID: UInt32
) throws -> CaptureCoordinateWindowWindowSnapshot? {
    guard let entries = CGWindowListCopyWindowInfo(
        [.optionIncludingWindow], CGWindowID(windowID)) as? [[String: Any]],
          entries.count == 1,
          let entry = entries.first,
          let number = entry[kCGWindowNumber as String] as? NSNumber,
          number.uint32Value == windowID,
          let owner = entry[kCGWindowOwnerPID as String] as? NSNumber,
          let layer = entry[kCGWindowLayer as String] as? NSNumber,
          let onScreen = entry[kCGWindowIsOnscreen as String] as? NSNumber,
          let rawBounds = entry[kCGWindowBounds as String],
          CFGetTypeID(rawBounds as CFTypeRef) == CFDictionaryGetTypeID(),
          let bounds = CGRect(dictionaryRepresentation: rawBounds as! CFDictionary) else {
        return nil
    }
    return .init(
        windowID: windowID, ownerPID: owner.intValue, layer: layer.intValue,
        isOnScreen: onScreen.boolValue,
        bounds: .init(x: bounds.origin.x, y: bounds.origin.y,
                      width: bounds.width, height: bounds.height))
}

private func productionCoordinateDragFrontmostWindowAtPoint(
    _ point: CoordinateMouseEventPointV1
) -> UInt32? {
    coordinateFrontmostNormalWindowID(
        at: CGPoint(x: point.x, y: point.y))
}

private func productionCoordinateDragPreparedSequence(
    path: [CoordinateMouseEventPointV1], button: String
) -> CoordinateDragPreparedSequence? {
    guard button == "left", let start = path.first,
          let syntheticSource = physicalInputSyntheticEventSourceV1,
          let down = CGEvent(
            mouseEventSource: syntheticSource, mouseType: .leftMouseDown,
            mouseCursorPosition: CGPoint(x: start.x, y: start.y), mouseButton: .left) else {
        return nil
    }
    var drags: [CGEvent] = []
    for point in path.dropFirst() {
        guard let event = CGEvent(
            mouseEventSource: syntheticSource, mouseType: .leftMouseDragged,
            mouseCursorPosition: CGPoint(x: point.x, y: point.y), mouseButton: .left) else {
            return nil
        }
        drags.append(event)
    }
    guard let release = productionInputRelease(
        metadata: .mouse(button: button), eventSource: syntheticSource) else { return nil }
    var activeToken: UUID?
    return .init(
        path: path,
        postDown: {
            activeToken = processInputCommitGateV1.registerPress(
                release: release,
                commitDown: { down.post(tap: .cghidEventTap); return true })
            return activeToken != nil
        },
        postDrag: { index in
            guard index > 0, index <= drags.count else { return false }
            return processInputCommitGateV1.commitSample {
                drags[index - 1].post(tap: .cghidEventTap)
                return true
            }
        },
        postUp: { _ in
            guard let token = activeToken else { return false }
            return processInputCommitGateV1.confirmRelease(token: token)
        })
}

func coordinateDragCancellationMarkerURL(
    requestID: Int64, helperBootID: String
) -> URL {
    let authority = Data("\(helperBootID):\(requestID)".utf8)
    let digest = SHA256.hash(data: authority).map { String(format: "%02x", $0) }.joined()
    // Use the system-wide sticky temp directory explicitly. The helper is
    // launched through LaunchServices, which is not required to inherit the
    // daemon's TMPDIR; /tmp is stable across both process environments.
    return URL(fileURLWithPath: "/tmp", isDirectory: true).appendingPathComponent(
        "kocoro-ax-drag-cancel-v1-\(digest)", isDirectory: false)
}

func productionCoordinateDragDependencies(
    requestID: Int64, helperBootID: String
) -> CoordinateDragDependencies {
    let cancellationURL = coordinateDragCancellationMarkerURL(
        requestID: requestID, helperBootID: helperBootID)
    return CoordinateDragDependencies(
        observeTopology: { try liveDisplayTopologyService.observe() },
        isPIDLive: { pid in
            refreshAppKitState()
            guard let exact = pid_t(exactly: pid),
                  let app = NSRunningApplication(processIdentifier: exact) else { return false }
            return !app.isTerminated
        },
        bundleIDForPID: { pid in
            guard let exact = pid_t(exactly: pid) else { return nil }
            return NSRunningApplication(processIdentifier: exact)?.bundleIdentifier
        },
        exactWindow: productionCoordinateDragWindow,
        frontmostWindowAtPoint: productionCoordinateDragFrontmostWindowAtPoint,
        prepare: productionCoordinateDragPreparedSequence,
        observePointer: {
            guard let event = CGEvent(source: nil) else { return nil }
            return .init(x: event.location.x, y: event.location.y)
        },
        observePhysicalInput: observePhysicalInputInterferenceV1,
        // The Go client creates this request-scoped marker after parent
        // cancellation but keeps waiting for our typed cleanup acknowledgement.
        // Polling once per ~60 Hz sample avoids a second socket reader while
        // keeping Stop/Take Over latency bounded to one sample plus mouseUp.
        isCancelled: { FileManager.default.fileExists(atPath: cancellationURL.path) },
        now: Date.init,
        sleep: Thread.sleep)
}
