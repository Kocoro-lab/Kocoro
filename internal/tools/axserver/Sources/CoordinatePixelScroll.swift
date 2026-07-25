import AppKit
import CoreGraphics
import CryptoKit
import Foundation

private let coordinatePixelScrollPointerToleranceV1 = 2.0
private let coordinatePixelScrollMaximumDeadlineHorizonV1 = 3.0

private struct CoordinatePixelScrollRectPayloadV1: Codable, Equatable {
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
        catch {
            throw StrictMutationWireError.invalid(
                "invalid expected_window_quartz_bounds")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(value.x, forKey: strictMutationKey("x"))
        try container.encode(value.y, forKey: strictMutationKey("y"))
        try container.encode(value.width, forKey: strictMutationKey("width"))
        try container.encode(value.height, forKey: strictMutationKey("height"))
    }
}

struct CoordinatePixelScrollCGDeltasV1: Equatable {
    let axis1: Int32
    let axis2: Int32
}

func coordinatePixelScrollCGDeltasV1(
    providerX: Int64,
    providerY: Int64,
    scaleX: Double = 1,
    scaleY: Double = 1
) -> CoordinatePixelScrollCGDeltasV1? {
    let limit = Int64(Int32.max)
    guard (-limit...limit).contains(providerX),
          (-limit...limit).contains(providerY),
          providerX != 0 || providerY != 0,
          scaleX.isFinite, scaleY.isFinite,
          scaleX > 0, scaleY > 0 else { return nil }
    let rawAxis1 = -Double(providerY) * scaleY
    let rawAxis2 = -Double(providerX) * scaleX
    guard rawAxis1.isFinite, rawAxis2.isFinite else { return nil }
    let roundedAxis1 = rawAxis1.rounded(.toNearestOrAwayFromZero)
    let roundedAxis2 = rawAxis2.rounded(.toNearestOrAwayFromZero)
    guard roundedAxis1 >= -Double(Int32.max),
          roundedAxis1 <= Double(Int32.max),
          roundedAxis2 >= -Double(Int32.max),
          roundedAxis2 <= Double(Int32.max),
          let exactAxis1 = Int32(exactly: Int64(roundedAxis1)),
          let exactAxis2 = Int32(exactly: Int64(roundedAxis2)),
          providerY == 0 || exactAxis1 != 0,
          providerX == 0 || exactAxis2 != 0 else { return nil }
    return .init(axis1: exactAxis1, axis2: exactAxis2)
}

struct CoordinatePixelScrollRequestV1: Codable, Equatable {
    let schemaVersion: Int
    let topologyRef: CoordinateMouseEventTopologyRefV1
    let helperBootID: String
    let pid: Int
    let bundleID: String
    let windowID: UInt32
    let expectedWindowQuartzBounds: DisplayTopologyRectV1
    let displayID: UInt32
    let quartzPoint: CoordinateMouseEventPointV1
    let unit: String
    let providerDeltaX: Int64
    let providerDeltaY: Int64
    let providerToQuartzScaleX: Double
    let providerToQuartzScaleY: Double
    let cgPointDeltaAxis1: Int64
    let cgPointDeltaAxis2: Int64
    let modifiers: [String]
    let targetPolicy: String
    let commitDeadlineAt: String

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "topology_ref", "helper_boot_id", "pid", "bundle_id",
            "window_id", "expected_window_quartz_bounds", "display_id",
            "quartz_point", "unit", "provider_delta_x", "provider_delta_y",
            "provider_to_quartz_scale_x", "provider_to_quartz_scale_y",
            "cg_point_delta_axis1", "cg_point_delta_axis2", "modifiers",
            "target_policy", "commit_deadline_at",
        ], field: "coordinate_pixel_scroll params")
        schemaVersion = try container.decode(
            Int.self, forKey: strictMutationKey("schema_version"))
        topologyRef = try container.decode(
            CoordinateMouseEventTopologyRefV1.self,
            forKey: strictMutationKey("topology_ref"))
        helperBootID = try container.decode(
            String.self, forKey: strictMutationKey("helper_boot_id"))
        pid = try container.decode(Int.self, forKey: strictMutationKey("pid"))
        bundleID = try container.decode(
            String.self, forKey: strictMutationKey("bundle_id"))
        let rawWindowID = try container.decode(
            Int64.self, forKey: strictMutationKey("window_id"))
        expectedWindowQuartzBounds = try container.decode(
            CoordinatePixelScrollRectPayloadV1.self,
            forKey: strictMutationKey("expected_window_quartz_bounds")).value
        let rawDisplayID = try container.decode(
            Int64.self, forKey: strictMutationKey("display_id"))
        quartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self,
            forKey: strictMutationKey("quartz_point"))
        unit = try container.decode(
            String.self, forKey: strictMutationKey("unit"))
        providerDeltaX = try container.decode(
            Int64.self, forKey: strictMutationKey("provider_delta_x"))
        providerDeltaY = try container.decode(
            Int64.self, forKey: strictMutationKey("provider_delta_y"))
        providerToQuartzScaleX = try container.decode(
            Double.self, forKey: strictMutationKey("provider_to_quartz_scale_x"))
        providerToQuartzScaleY = try container.decode(
            Double.self, forKey: strictMutationKey("provider_to_quartz_scale_y"))
        cgPointDeltaAxis1 = try container.decode(
            Int64.self, forKey: strictMutationKey("cg_point_delta_axis1"))
        cgPointDeltaAxis2 = try container.decode(
            Int64.self, forKey: strictMutationKey("cg_point_delta_axis2"))
        modifiers = try container.decode(
            [String].self, forKey: strictMutationKey("modifiers"))
        targetPolicy = try container.decode(
            String.self, forKey: strictMutationKey("target_policy"))
        commitDeadlineAt = try container.decode(
            String.self, forKey: strictMutationKey("commit_deadline_at"))
        guard schemaVersion == 1, topologyRef.generation > 0,
              strictMutationIdentity(helperBootID), pid > 0,
              strictMutationIdentity(bundleID),
              let exactWindowID = UInt32(exactly: rawWindowID), exactWindowID > 0,
              let exactDisplayID = UInt32(exactly: rawDisplayID), exactDisplayID > 0,
              strictInputModifiersV1(modifiers),
              unit == "pixel", targetPolicy == "same_window",
              let expectedDeltas = coordinatePixelScrollCGDeltasV1(
                providerX: providerDeltaX, providerY: providerDeltaY,
                scaleX: providerToQuartzScaleX,
                scaleY: providerToQuartzScaleY),
              cgPointDeltaAxis1 == Int64(expectedDeltas.axis1),
              cgPointDeltaAxis2 == Int64(expectedDeltas.axis2),
              strictMutationDate(commitDeadlineAt) != nil else {
            throw StrictMutationWireError.invalid(
                "invalid coordinate_pixel_scroll authority or policy")
        }
        windowID = exactWindowID
        displayID = exactDisplayID
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
            CoordinatePixelScrollRectPayloadV1(expectedWindowQuartzBounds),
            forKey: strictMutationKey("expected_window_quartz_bounds"))
        try container.encode(displayID, forKey: strictMutationKey("display_id"))
        try container.encode(quartzPoint, forKey: strictMutationKey("quartz_point"))
        try container.encode(unit, forKey: strictMutationKey("unit"))
        try container.encode(providerDeltaX, forKey: strictMutationKey("provider_delta_x"))
        try container.encode(providerDeltaY, forKey: strictMutationKey("provider_delta_y"))
        try container.encode(
            providerToQuartzScaleX,
            forKey: strictMutationKey("provider_to_quartz_scale_x"))
        try container.encode(
            providerToQuartzScaleY,
            forKey: strictMutationKey("provider_to_quartz_scale_y"))
        try container.encode(
            cgPointDeltaAxis1, forKey: strictMutationKey("cg_point_delta_axis1"))
        try container.encode(
            cgPointDeltaAxis2, forKey: strictMutationKey("cg_point_delta_axis2"))
        try container.encode(modifiers, forKey: strictMutationKey("modifiers"))
        try container.encode(targetPolicy, forKey: strictMutationKey("target_policy"))
        try container.encode(commitDeadlineAt, forKey: strictMutationKey("commit_deadline_at"))
    }
}

struct CoordinatePixelScrollRPCRequestV1: Codable, Equatable {
    let id: Int64
    let method: String
    let params: CoordinatePixelScrollRequestV1

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(
            container, exactly: ["id", "method", "params"],
            field: "coordinate_pixel_scroll envelope")
        id = try container.decode(Int64.self, forKey: strictMutationKey("id"))
        method = try container.decode(String.self, forKey: strictMutationKey("method"))
        params = try container.decode(
            CoordinatePixelScrollRequestV1.self, forKey: strictMutationKey("params"))
        guard id > 0, method == "coordinate_pixel_scroll" else {
            throw StrictMutationWireError.invalid(
                "invalid coordinate_pixel_scroll envelope")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(id, forKey: strictMutationKey("id"))
        try container.encode(method, forKey: strictMutationKey("method"))
        try container.encode(params, forKey: strictMutationKey("params"))
    }
}

func decodeCoordinatePixelScrollRPCRequestV1(
    _ payload: Data
) throws -> CoordinatePixelScrollRPCRequestV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(CoordinatePixelScrollRPCRequestV1.self, from: payload)
}

struct CoordinatePixelScrollAcknowledgementV1: Codable, Equatable {
    let quartzPoint: CoordinateMouseEventPointV1
    let unit: String
    let providerDeltaX: Int64
    let providerDeltaY: Int64
    let providerToQuartzScaleX: Double
    let providerToQuartzScaleY: Double
    let cgPointDeltaAxis1: Int64
    let cgPointDeltaAxis2: Int64

    init(request: CoordinatePixelScrollRequestV1) {
        quartzPoint = request.quartzPoint
        unit = request.unit
        providerDeltaX = request.providerDeltaX
        providerDeltaY = request.providerDeltaY
        providerToQuartzScaleX = request.providerToQuartzScaleX
        providerToQuartzScaleY = request.providerToQuartzScaleY
        cgPointDeltaAxis1 = request.cgPointDeltaAxis1
        cgPointDeltaAxis2 = request.cgPointDeltaAxis2
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "quartz_point", "unit", "provider_delta_x", "provider_delta_y",
            "provider_to_quartz_scale_x", "provider_to_quartz_scale_y",
            "cg_point_delta_axis1", "cg_point_delta_axis2",
        ], field: "coordinate_pixel_scroll requested")
        quartzPoint = try container.decode(
            CoordinateMouseEventPointV1.self,
            forKey: strictMutationKey("quartz_point"))
        unit = try container.decode(String.self, forKey: strictMutationKey("unit"))
        providerDeltaX = try container.decode(
            Int64.self, forKey: strictMutationKey("provider_delta_x"))
        providerDeltaY = try container.decode(
            Int64.self, forKey: strictMutationKey("provider_delta_y"))
        providerToQuartzScaleX = try container.decode(
            Double.self, forKey: strictMutationKey("provider_to_quartz_scale_x"))
        providerToQuartzScaleY = try container.decode(
            Double.self, forKey: strictMutationKey("provider_to_quartz_scale_y"))
        cgPointDeltaAxis1 = try container.decode(
            Int64.self, forKey: strictMutationKey("cg_point_delta_axis1"))
        cgPointDeltaAxis2 = try container.decode(
            Int64.self, forKey: strictMutationKey("cg_point_delta_axis2"))
        guard let expected = coordinatePixelScrollCGDeltasV1(
            providerX: providerDeltaX, providerY: providerDeltaY,
            scaleX: providerToQuartzScaleX,
            scaleY: providerToQuartzScaleY),
              unit == "pixel",
              cgPointDeltaAxis1 == Int64(expected.axis1),
              cgPointDeltaAxis2 == Int64(expected.axis2) else {
            throw StrictMutationWireError.invalid(
                "coordinate_pixel_scroll requested semantics changed")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(quartzPoint, forKey: strictMutationKey("quartz_point"))
        try container.encode(unit, forKey: strictMutationKey("unit"))
        try container.encode(providerDeltaX, forKey: strictMutationKey("provider_delta_x"))
        try container.encode(providerDeltaY, forKey: strictMutationKey("provider_delta_y"))
        try container.encode(
            providerToQuartzScaleX,
            forKey: strictMutationKey("provider_to_quartz_scale_x"))
        try container.encode(
            providerToQuartzScaleY,
            forKey: strictMutationKey("provider_to_quartz_scale_y"))
        try container.encode(
            cgPointDeltaAxis1, forKey: strictMutationKey("cg_point_delta_axis1"))
        try container.encode(
            cgPointDeltaAxis2, forKey: strictMutationKey("cg_point_delta_axis2"))
    }
}

enum CoordinatePixelScrollCommitStateV1: String, Codable, Equatable {
    case notCommitted = "not_committed"
    case committed
    case unknown
}

struct CoordinatePixelScrollResultV1: Codable, Equatable {
    let schemaVersion: Int
    let status: String
    let pointerMoveCommitState: String
    let scrollCommitState: String
    let phase: String
    let failureCode: String?
    let retrySafe: Bool
    let requested: CoordinatePixelScrollAcknowledgementV1?
    let pointerEndpoint: CoordinateMouseEventPointerEndpointV1?

    init(
        status: String,
        pointerMoveCommitState: CoordinatePixelScrollCommitStateV1,
        scrollCommitState: CoordinatePixelScrollCommitStateV1,
        phase: String,
        failureCode: String?,
        requested: CoordinatePixelScrollAcknowledgementV1?,
        pointerEndpoint: CoordinateMouseEventPointerEndpointV1?
    ) {
        schemaVersion = 1
        self.status = status
        self.pointerMoveCommitState = pointerMoveCommitState.rawValue
        self.scrollCommitState = scrollCommitState.rawValue
        self.phase = phase
        self.failureCode = failureCode
        retrySafe = false
        self.requested = requested
        self.pointerEndpoint = pointerEndpoint
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: StrictMutationCodingKey.self)
        try requireStrictMutationKeys(container, exactly: [
            "schema_version", "status", "pointer_move_commit_state",
            "scroll_commit_state", "phase", "failure_code", "retry_safe",
            "requested", "pointer_endpoint",
        ], field: "coordinate_pixel_scroll result")
        schemaVersion = try container.decode(
            Int.self, forKey: strictMutationKey("schema_version"))
        status = try container.decode(String.self, forKey: strictMutationKey("status"))
        pointerMoveCommitState = try container.decode(
            String.self, forKey: strictMutationKey("pointer_move_commit_state"))
        scrollCommitState = try container.decode(
            String.self, forKey: strictMutationKey("scroll_commit_state"))
        phase = try container.decode(String.self, forKey: strictMutationKey("phase"))
        failureCode = try container.decodeIfPresent(
            String.self, forKey: strictMutationKey("failure_code"))
        retrySafe = try container.decode(
            Bool.self, forKey: strictMutationKey("retry_safe"))
        requested = try container.decodeIfPresent(
            CoordinatePixelScrollAcknowledgementV1.self,
            forKey: strictMutationKey("requested"))
        pointerEndpoint = try container.decodeIfPresent(
            CoordinateMouseEventPointerEndpointV1.self,
            forKey: strictMutationKey("pointer_endpoint"))
        guard validateCoordinatePixelScrollResultV1(self) else {
            throw StrictMutationWireError.invalid(
                "invalid coordinate_pixel_scroll result tagged union")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: StrictMutationCodingKey.self)
        try container.encode(schemaVersion, forKey: strictMutationKey("schema_version"))
        try container.encode(status, forKey: strictMutationKey("status"))
        try container.encode(
            pointerMoveCommitState,
            forKey: strictMutationKey("pointer_move_commit_state"))
        try container.encode(
            scrollCommitState, forKey: strictMutationKey("scroll_commit_state"))
        try container.encode(phase, forKey: strictMutationKey("phase"))
        try container.encode(failureCode, forKey: strictMutationKey("failure_code"))
        try container.encode(retrySafe, forKey: strictMutationKey("retry_safe"))
        try container.encode(requested, forKey: strictMutationKey("requested"))
        try container.encode(pointerEndpoint, forKey: strictMutationKey("pointer_endpoint"))
    }
}

func decodeCoordinatePixelScrollResultV1(
    _ payload: Data
) throws -> CoordinatePixelScrollResultV1 {
    try rejectStrictMutationDuplicateJSONMembers(payload)
    return try JSONDecoder().decode(CoordinatePixelScrollResultV1.self, from: payload)
}

private func validateCoordinatePixelScrollResultV1(
    _ result: CoordinatePixelScrollResultV1
) -> Bool {
    guard result.schemaVersion == 1, !result.retrySafe,
          CoordinatePixelScrollCommitStateV1(
            rawValue: result.pointerMoveCommitState) != nil,
          CoordinatePixelScrollCommitStateV1(
            rawValue: result.scrollCommitState) != nil else { return false }
    if let endpoint = result.pointerEndpoint {
        let verified = endpoint.observed.map {
            abs(endpoint.requested.x - $0.x) <= coordinatePixelScrollPointerToleranceV1 &&
                abs(endpoint.requested.y - $0.y) <= coordinatePixelScrollPointerToleranceV1
        } ?? false
        guard endpoint.tolerance == coordinatePixelScrollPointerToleranceV1,
              endpoint.verified == verified,
              endpoint.requested == result.requested?.quartzPoint else { return false }
    }
    let code = result.failureCode ?? ""
    switch result.status {
    case "failed":
        let preflight = Set([
            "invalid_request", "request_expired", "cancelled",
            "topology_unavailable", "stale_topology", "helper_boot_mismatch",
            "process_not_live", "process_identity_mismatch", "window_not_found",
            "window_identity_mismatch", "window_not_actionable",
            "window_bounds_mismatch", "display_not_found", "display_not_actionable",
            "point_outside_window", "point_outside_display", "point_occluded",
            "interference_detection_unavailable",
        ])
        let validPhase =
            (preflight.contains(code) && result.phase == "preflight") ||
            (code == "event_preparation_failed" && result.phase == "preparation") ||
            (code == "pointer_move_not_committed" &&
                result.phase == "pointer_move_commit")
        let requestShape = code == "invalid_request"
            ? result.requested == nil : result.requested != nil
        return result.pointerMoveCommitState == "not_committed" &&
            result.scrollCommitState == "not_committed" &&
            result.failureCode != nil && validPhase && requestShape &&
            result.pointerEndpoint == nil
    case "committed_unverified":
        guard result.pointerMoveCommitState == "committed",
              result.failureCode != nil, result.requested != nil,
              let endpoint = result.pointerEndpoint else { return false }
        let beforeScroll = result.scrollCommitState == "not_committed" &&
            result.phase == "between_commits"
        let afterScroll = result.scrollCommitState == "committed" &&
            result.phase == "post_verification"
        switch code {
        case "scroll_postcondition_not_declared":
            return afterScroll && endpoint.verified
        case "cancelled_before_scroll", "request_expired_before_scroll",
             "scroll_not_committed":
            return beforeScroll
        case "pointer_endpoint_not_verified":
            return beforeScroll && !endpoint.verified
        case "cancelled_after_scroll", "request_expired_after_scroll":
            return afterScroll
        case "interference_detection_unavailable":
            return beforeScroll || afterScroll
        default:
            return false
        }
    case "user_interference":
        let allowed = Set([
            "physical_input_interference", "topology_unavailable",
            "stale_topology", "helper_boot_mismatch", "process_not_live",
            "process_identity_mismatch", "window_not_found",
            "window_identity_mismatch", "window_not_actionable",
            "window_bounds_mismatch", "display_not_found",
            "display_not_actionable", "point_outside_window",
            "point_outside_display", "point_occluded",
        ])
        let preCommit = result.pointerMoveCommitState == "not_committed" &&
            result.scrollCommitState == "not_committed" &&
            result.pointerEndpoint == nil
        let postMove = result.pointerMoveCommitState == "committed" &&
            ["not_committed", "committed"].contains(result.scrollCommitState) &&
            result.pointerEndpoint != nil
        let validPlacement = code == "physical_input_interference"
            ? preCommit || postMove : postMove
        return result.phase == "user_interference" &&
            result.failureCode != nil && allowed.contains(code) &&
            result.requested != nil && validPlacement
    case "commit_unknown":
        let unknownMove = result.pointerMoveCommitState == "unknown" &&
            result.scrollCommitState == "not_committed" &&
            code == "pointer_move_commit_unknown" &&
            result.phase == "pointer_move_commit"
        let unknownScroll = result.pointerMoveCommitState == "committed" &&
            result.scrollCommitState == "unknown" &&
            code == "scroll_commit_unknown" &&
            result.phase == "scroll_commit" &&
            result.pointerEndpoint != nil
        return result.failureCode != nil && result.requested != nil &&
            (unknownMove || unknownScroll)
    default:
        return false
    }
}

struct CoordinatePixelScrollPreparedEventsV1 {
    let point: CoordinateMouseEventPointV1
    let providerDeltaX: Int64
    let providerDeltaY: Int64
    let providerToQuartzScaleX: Double
    let providerToQuartzScaleY: Double
    let cgDeltas: CoordinatePixelScrollCGDeltasV1
    let eventContractVerified: Bool
    let pointerMoveEventType: CGEventType
    let pointerMoveEventLocation: CoordinateMouseEventPointV1
    let pointerMoveExpectedEventCount: UInt32
    let scrollEventType: CGEventType
    let scrollEventLocation: CoordinateMouseEventPointV1
    let scrollEventContinuous: Int64
    let scrollEventPointDeltaAxis1: Int64
    let scrollEventPointDeltaAxis2: Int64
    let postPointerMove: () -> CoordinatePixelScrollCommitStateV1
    let postScroll: () -> CoordinatePixelScrollCommitStateV1

    init(
        point: CoordinateMouseEventPointV1,
        providerDeltaX: Int64,
        providerDeltaY: Int64,
        providerToQuartzScaleX: Double = 1,
        providerToQuartzScaleY: Double = 1,
        cgDeltas: CoordinatePixelScrollCGDeltasV1,
        eventContractVerified: Bool,
        pointerMoveEventType: CGEventType = .mouseMoved,
        pointerMoveEventLocation: CoordinateMouseEventPointV1? = nil,
        pointerMoveExpectedEventCount: UInt32 = 1,
        scrollEventType: CGEventType = .scrollWheel,
        scrollEventLocation: CoordinateMouseEventPointV1? = nil,
        scrollEventContinuous: Int64 = 1,
        scrollEventPointDeltaAxis1: Int64? = nil,
        scrollEventPointDeltaAxis2: Int64? = nil,
        postPointerMove: @escaping () -> CoordinatePixelScrollCommitStateV1,
        postScroll: @escaping () -> CoordinatePixelScrollCommitStateV1
    ) {
        self.point = point
        self.providerDeltaX = providerDeltaX
        self.providerDeltaY = providerDeltaY
        self.providerToQuartzScaleX = providerToQuartzScaleX
        self.providerToQuartzScaleY = providerToQuartzScaleY
        self.cgDeltas = cgDeltas
        self.eventContractVerified = eventContractVerified
        self.pointerMoveEventType = pointerMoveEventType
        self.pointerMoveEventLocation = pointerMoveEventLocation ?? point
        self.pointerMoveExpectedEventCount = pointerMoveExpectedEventCount
        self.scrollEventType = scrollEventType
        self.scrollEventLocation = scrollEventLocation ?? point
        self.scrollEventContinuous = scrollEventContinuous
        self.scrollEventPointDeltaAxis1 =
            scrollEventPointDeltaAxis1 ?? Int64(cgDeltas.axis1)
        self.scrollEventPointDeltaAxis2 =
            scrollEventPointDeltaAxis2 ?? Int64(cgDeltas.axis2)
        self.postPointerMove = postPointerMove
        self.postScroll = postScroll
    }
}

struct CoordinatePixelScrollExpectedEventV1 {
    let type: CGEventType
    let count: UInt32
}

struct CoordinatePixelScrollDependenciesV1 {
    let authorityFailure: (CoordinatePixelScrollRequestV1) -> String?
    let prepare: (
        CoordinateMouseEventPointV1, Int64, Int64, Double, Double, Int64, Int64
    ) -> CoordinatePixelScrollPreparedEventsV1?
    let observePointer: () -> CoordinateMouseEventPointV1?
    let assessPhysicalInput: (
        CoordinateMouseEventPointV1?,
        [CoordinatePixelScrollExpectedEventV1],
        String
    ) -> PhysicalInputInterferenceAssessmentV1
    let isCancelled: () -> Bool
    let now: () -> Date
}

private func pixelScrollPointMatches(
    _ expected: CoordinateMouseEventPointV1,
    _ observed: CoordinateMouseEventPointV1?
) -> Bool {
    guard let observed else { return false }
    return abs(expected.x - observed.x) <= coordinatePixelScrollPointerToleranceV1 &&
        abs(expected.y - observed.y) <= coordinatePixelScrollPointerToleranceV1
}

private func pixelScrollEndpoint(
    request: CoordinatePixelScrollRequestV1,
    observed: CoordinateMouseEventPointV1?
) -> CoordinateMouseEventPointerEndpointV1 {
    .init(
        requested: request.quartzPoint,
        observed: observed,
        tolerance: coordinatePixelScrollPointerToleranceV1,
        verified: pixelScrollPointMatches(request.quartzPoint, observed))
}

private func pixelScrollResult(
    request: CoordinatePixelScrollRequestV1,
    status: String,
    move: CoordinatePixelScrollCommitStateV1,
    scroll: CoordinatePixelScrollCommitStateV1,
    phase: String,
    code: String,
    observed: CoordinateMouseEventPointV1? = nil
) -> CoordinatePixelScrollResultV1 {
    .init(
        status: status, pointerMoveCommitState: move,
        scrollCommitState: scroll, phase: phase, failureCode: code,
        requested: .init(request: request),
        pointerEndpoint: move == .committed
            ? pixelScrollEndpoint(request: request, observed: observed) : nil)
}

private func pixelScrollFailure(
    request: CoordinatePixelScrollRequestV1?,
    _ code: String,
    phase: String = "preflight"
) -> CoordinatePixelScrollResultV1 {
    .init(
        status: "failed", pointerMoveCommitState: .notCommitted,
        scrollCommitState: .notCommitted, phase: phase, failureCode: code,
        requested: request.map(CoordinatePixelScrollAcknowledgementV1.init),
        pointerEndpoint: nil)
}

func runCoordinatePixelScrollV1(
    request: CoordinatePixelScrollRequestV1,
    dependencies: CoordinatePixelScrollDependenciesV1
) -> CoordinatePixelScrollResultV1 {
    guard let deadline = strictMutationDate(request.commitDeadlineAt) else {
        return pixelScrollFailure(request: nil, "invalid_request")
    }
    let horizon = deadline.timeIntervalSince(dependencies.now())
    guard horizon > 0 else {
        return pixelScrollFailure(request: request, "request_expired")
    }
    guard horizon <= coordinatePixelScrollMaximumDeadlineHorizonV1 else {
        return pixelScrollFailure(request: nil, "invalid_request")
    }
    guard !dependencies.isCancelled() else {
        return pixelScrollFailure(request: request, "cancelled")
    }
    if let code = dependencies.authorityFailure(request) {
        return pixelScrollFailure(request: request, code)
    }
    guard let prepared = dependencies.prepare(
        request.quartzPoint, request.providerDeltaX, request.providerDeltaY,
        request.providerToQuartzScaleX, request.providerToQuartzScaleY,
        request.cgPointDeltaAxis1, request.cgPointDeltaAxis2),
          prepared.point == request.quartzPoint,
          prepared.providerDeltaX == request.providerDeltaX,
          prepared.providerDeltaY == request.providerDeltaY,
          prepared.providerToQuartzScaleX == request.providerToQuartzScaleX,
          prepared.providerToQuartzScaleY == request.providerToQuartzScaleY,
          prepared.cgDeltas == coordinatePixelScrollCGDeltasV1(
            providerX: request.providerDeltaX, providerY: request.providerDeltaY,
            scaleX: request.providerToQuartzScaleX,
            scaleY: request.providerToQuartzScaleY),
          Int64(prepared.cgDeltas.axis1) == request.cgPointDeltaAxis1,
          Int64(prepared.cgDeltas.axis2) == request.cgPointDeltaAxis2,
          prepared.eventContractVerified,
          prepared.pointerMoveEventType == .mouseMoved,
          prepared.pointerMoveEventLocation == request.quartzPoint,
          prepared.pointerMoveExpectedEventCount <= 1,
          prepared.scrollEventType == .scrollWheel,
          prepared.scrollEventLocation == request.quartzPoint,
          prepared.scrollEventContinuous != 0,
          prepared.scrollEventPointDeltaAxis1 == Int64(prepared.cgDeltas.axis1),
          prepared.scrollEventPointDeltaAxis2 == Int64(prepared.cgDeltas.axis2) else {
        return pixelScrollFailure(
            request: request, "event_preparation_failed", phase: "preparation")
    }
    guard dependencies.now() < deadline else {
        return pixelScrollFailure(request: request, "request_expired")
    }
    switch dependencies.assessPhysicalInput(nil, [], "precommit") {
    case .interference:
        return pixelScrollResult(
            request: request, status: "user_interference",
            move: .notCommitted, scroll: .notCommitted,
            phase: "user_interference", code: "physical_input_interference")
    case .unavailable:
        return pixelScrollFailure(
            request: request, "interference_detection_unavailable")
    case .unchanged:
        break
    }
    guard !dependencies.isCancelled() else {
        return pixelScrollFailure(request: request, "cancelled")
    }
    guard dependencies.now() < deadline else {
        return pixelScrollFailure(request: request, "request_expired")
    }
    let moveState = prepared.postPointerMove()
    switch moveState {
    case .notCommitted:
        return pixelScrollFailure(
            request: request, "pointer_move_not_committed",
            phase: "pointer_move_commit")
    case .unknown:
        return .init(
            status: "commit_unknown", pointerMoveCommitState: .unknown,
            scrollCommitState: .notCommitted, phase: "pointer_move_commit",
            failureCode: "pointer_move_commit_unknown",
            requested: .init(request: request), pointerEndpoint: nil)
    case .committed:
        break
    }
    var observed = dependencies.observePointer()
    switch dependencies.assessPhysicalInput(
        request.quartzPoint,
        prepared.pointerMoveExpectedEventCount == 0
            ? []
            : [.init(
                type: .mouseMoved,
                count: prepared.pointerMoveExpectedEventCount)],
        "after_move"
    ) {
    case .interference:
        return pixelScrollResult(
            request: request, status: "user_interference",
            move: .committed, scroll: .notCommitted,
            phase: "user_interference", code: "physical_input_interference",
            observed: observed)
    case .unavailable:
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "interference_detection_unavailable",
            observed: observed)
    case .unchanged:
        break
    }
    // The HID counter and cursor position are delivered asynchronously. The
    // monitor above may have waited for the move to settle, so verify against a
    // fresh cursor sample instead of the pre-monitor sample.
    observed = dependencies.observePointer()
    var endpoint = pixelScrollEndpoint(request: request, observed: observed)
    guard endpoint.verified else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "pointer_endpoint_not_verified",
            observed: observed)
    }
    guard !dependencies.isCancelled() else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "cancelled_before_scroll",
            observed: observed)
    }
    guard dependencies.now() < deadline else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "request_expired_before_scroll",
            observed: observed)
    }
    // The reference executor's move and wheel are distinct commits. Re-run the
    // complete authority/hit-test after the move, before any scroll side effect.
    if let code = dependencies.authorityFailure(request) {
        return pixelScrollResult(
            request: request, status: "user_interference",
            move: .committed, scroll: .notCommitted,
            phase: "user_interference", code: code, observed: observed)
    }
    guard !dependencies.isCancelled() else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "cancelled_before_scroll",
            observed: observed)
    }
    guard dependencies.now() < deadline else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "request_expired_before_scroll",
            observed: observed)
    }
    switch dependencies.assessPhysicalInput(
        request.quartzPoint, [], "before_scroll") {
    case .interference:
        return pixelScrollResult(
            request: request, status: "user_interference",
            move: .committed, scroll: .notCommitted,
            phase: "user_interference", code: "physical_input_interference",
            observed: observed)
    case .unavailable:
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "interference_detection_unavailable",
            observed: observed)
    case .unchanged:
        break
    }
    observed = dependencies.observePointer()
    guard !dependencies.isCancelled() else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "cancelled_before_scroll",
            observed: observed)
    }
    guard dependencies.now() < deadline else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "request_expired_before_scroll",
            observed: observed)
    }
    let scrollState = prepared.postScroll()
    switch scrollState {
    case .notCommitted:
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .notCommitted,
            phase: "between_commits", code: "scroll_not_committed",
            observed: observed)
    case .unknown:
        return pixelScrollResult(
            request: request, status: "commit_unknown",
            move: .committed, scroll: .unknown,
            phase: "scroll_commit", code: "scroll_commit_unknown",
            observed: observed)
    case .committed:
        break
    }
    observed = dependencies.observePointer()
    endpoint = pixelScrollEndpoint(request: request, observed: observed)
    switch dependencies.assessPhysicalInput(
        request.quartzPoint,
        [.init(type: .scrollWheel, count: 1)],
        "after_scroll"
    ) {
    case .interference:
        return pixelScrollResult(
            request: request, status: "user_interference",
            move: .committed, scroll: .committed,
            phase: "user_interference", code: "physical_input_interference",
            observed: observed)
    case .unavailable:
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .committed,
            phase: "post_verification", code: "interference_detection_unavailable",
            observed: observed)
    case .unchanged:
        break
    }
    guard !dependencies.isCancelled() else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .committed,
            phase: "post_verification", code: "cancelled_after_scroll",
            observed: observed)
    }
    guard dependencies.now() < deadline else {
        return pixelScrollResult(
            request: request, status: "committed_unverified",
            move: .committed, scroll: .committed,
            phase: "post_verification", code: "request_expired_after_scroll",
            observed: observed)
    }
    if let code = dependencies.authorityFailure(request) {
        return pixelScrollResult(
            request: request, status: "user_interference",
            move: .committed, scroll: .committed,
            phase: "user_interference", code: code, observed: observed)
    }
    return pixelScrollResult(
        request: request, status: "committed_unverified",
        move: .committed, scroll: .committed,
        phase: "post_verification", code: "scroll_postcondition_not_declared",
        observed: observed)
}

func productionCoordinatePixelScrollPreparedEventsV1(
    point: CoordinateMouseEventPointV1,
    providerDeltaX: Int64,
    providerDeltaY: Int64,
    providerToQuartzScaleX: Double = 1,
    providerToQuartzScaleY: Double = 1,
    expectedCGPointDeltaAxis1: Int64? = nil,
    expectedCGPointDeltaAxis2: Int64? = nil,
    observePointer: () -> CoordinateMouseEventPointV1? = {
        guard let event = CGEvent(source: nil) else { return nil }
        return .init(x: event.location.x, y: event.location.y)
    }
) -> CoordinatePixelScrollPreparedEventsV1? {
    guard let deltas = coordinatePixelScrollCGDeltasV1(
        providerX: providerDeltaX, providerY: providerDeltaY,
        scaleX: providerToQuartzScaleX,
        scaleY: providerToQuartzScaleY),
          expectedCGPointDeltaAxis1 == nil ||
            expectedCGPointDeltaAxis1 == Int64(deltas.axis1),
          expectedCGPointDeltaAxis2 == nil ||
            expectedCGPointDeltaAxis2 == Int64(deltas.axis2),
          let currentPointer = observePointer(),
          let source = physicalInputSyntheticEventSourceV1,
          let move = CGEvent(
            mouseEventSource: source, mouseType: .mouseMoved,
            mouseCursorPosition: CGPoint(x: point.x, y: point.y),
            mouseButton: .left),
          // Current macOS increments counters for private-state pixel scrolls
          // but does not route them to the target application. A nil source
          // is routable and still stamps the helper PID on the NSEvent.
          let scroll = CGEvent(
            scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 2,
            wheel1: deltas.axis1, wheel2: deltas.axis2, wheel3: 0) else {
        return nil
    }
    scroll.location = CGPoint(x: point.x, y: point.y)
    let moveLocation = CoordinateMouseEventPointV1(
        x: move.location.x, y: move.location.y)
    let scrollLocation = CoordinateMouseEventPointV1(
        x: scroll.location.x, y: scroll.location.y)
    let continuous = scroll.getIntegerValueField(.scrollWheelEventIsContinuous)
    let axis1 = scroll.getIntegerValueField(.scrollWheelEventPointDeltaAxis1)
    let axis2 = scroll.getIntegerValueField(.scrollWheelEventPointDeltaAxis2)
    let moveRequired = currentPointer != point
    let verified = move.type == .mouseMoved && moveLocation == point &&
        scroll.type == .scrollWheel && scrollLocation == point &&
        continuous != 0 && axis1 == Int64(deltas.axis1) &&
        axis2 == Int64(deltas.axis2)
    guard verified else { return nil }
    return .init(
        point: point, providerDeltaX: providerDeltaX,
        providerDeltaY: providerDeltaY,
        providerToQuartzScaleX: providerToQuartzScaleX,
        providerToQuartzScaleY: providerToQuartzScaleY,
        cgDeltas: deltas,
        eventContractVerified: verified,
        pointerMoveEventType: move.type,
        pointerMoveEventLocation: moveLocation,
        pointerMoveExpectedEventCount: moveRequired ? 1 : 0,
        scrollEventType: scroll.type,
        scrollEventLocation: scrollLocation,
        scrollEventContinuous: continuous,
        scrollEventPointDeltaAxis1: axis1,
        scrollEventPointDeltaAxis2: axis2,
        postPointerMove: {
            if !moveRequired {
                return .committed
            }
            return processInputCommitGateV1.commitSample {
                move.post(tap: .cghidEventTap)
                return true
            }
                ? CoordinatePixelScrollCommitStateV1.committed
                : CoordinatePixelScrollCommitStateV1.notCommitted
        },
        postScroll: {
            // Build the routable event only after the pointer endpoint has
            // been observed. A source-nil scroll created before the move keeps
            // its original routing target even if its location field is later
            // overwritten.
            guard let liveScroll = CGEvent(
                scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 2,
                wheel1: deltas.axis1, wheel2: deltas.axis2, wheel3: 0)
            else {
                return .notCommitted
            }
            let liveLocation = CoordinateMouseEventPointV1(
                x: liveScroll.location.x, y: liveScroll.location.y)
            guard liveScroll.type == .scrollWheel,
                  liveLocation == point,
                  liveScroll.getIntegerValueField(
                    .scrollWheelEventIsContinuous) != 0,
                  liveScroll.getIntegerValueField(
                    .scrollWheelEventPointDeltaAxis1) == Int64(deltas.axis1),
                  liveScroll.getIntegerValueField(
                    .scrollWheelEventPointDeltaAxis2) == Int64(deltas.axis2)
            else {
                return .notCommitted
            }
            return processInputCommitGateV1.commitSample {
                liveScroll.post(tap: .cghidEventTap)
                return true
            }
                ? CoordinatePixelScrollCommitStateV1.committed
                : CoordinatePixelScrollCommitStateV1.notCommitted
        })
}

private func coordinatePixelScrollRectEqual(
    _ left: DisplayTopologyRectV1,
    _ right: DisplayTopologyRectV1
) -> Bool {
    abs(left.x - right.x) < 0.000_001 &&
        abs(left.y - right.y) < 0.000_001 &&
        abs(left.width - right.width) < 0.000_001 &&
        abs(left.height - right.height) < 0.000_001
}

private func coordinatePixelScrollContains(
    _ rect: DisplayTopologyRectV1,
    _ point: CoordinateMouseEventPointV1
) -> Bool {
    point.x >= rect.x && point.y >= rect.y &&
        point.x < rect.x + rect.width &&
        point.y < rect.y + rect.height
}

private func productionCoordinatePixelScrollWindow(
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
          let bounds = CGRect(
            dictionaryRepresentation: rawBounds as! CFDictionary) else {
        return nil
    }
    return .init(
        windowID: windowID, ownerPID: owner.intValue,
        layer: layer.intValue, isOnScreen: onScreen.boolValue,
        bounds: .init(
            x: bounds.origin.x, y: bounds.origin.y,
            width: bounds.width, height: bounds.height))
}

private func productionCoordinatePixelScrollFrontmostWindowAtPoint(
    _ point: CoordinateMouseEventPointV1
) -> UInt32? {
    coordinateFrontmostNormalWindowID(
        at: CGPoint(x: point.x, y: point.y))
}

private func productionCoordinatePixelScrollAuthorityFailure(
    _ request: CoordinatePixelScrollRequestV1
) -> String? {
    let topology: DisplayTopologyV1
    do {
        topology = try liveDisplayTopologyService.observe()
        try topology.validate()
    } catch { return "topology_unavailable" }
    guard topology.topologyID == request.topologyRef.topologyID,
          topology.generation == request.topologyRef.generation else {
        return "stale_topology"
    }
    guard topology.helperBootID == request.helperBootID else {
        return "helper_boot_mismatch"
    }
    refreshAppKitState()
    guard let exactPID = pid_t(exactly: request.pid),
          let app = NSRunningApplication(processIdentifier: exactPID),
          !app.isTerminated else { return "process_not_live" }
    guard app.bundleIdentifier == request.bundleID else {
        return "process_identity_mismatch"
    }
    let window: CaptureCoordinateWindowWindowSnapshot
    do {
        guard let exact = try productionCoordinatePixelScrollWindow(
            request.windowID) else { return "window_not_found" }
        window = exact
    } catch { return "window_not_found" }
    guard window.windowID == request.windowID,
          window.ownerPID == request.pid else {
        return "window_identity_mismatch"
    }
    guard window.layer == 0, window.isOnScreen else {
        return "window_not_actionable"
    }
    guard coordinatePixelScrollRectEqual(
        window.bounds, request.expectedWindowQuartzBounds) else {
        return "window_bounds_mismatch"
    }
    guard coordinatePixelScrollContains(
        window.bounds, request.quartzPoint) else {
        return "point_outside_window"
    }
    guard let display = topology.displays.first(where: {
        $0.displayID == request.displayID
    }) else { return "display_not_found" }
    guard display.isActive, display.isOnline, !display.isAsleep,
          display.mirrorMasterDisplayID == nil,
          display.rotationDegrees == 0 else {
        return "display_not_actionable"
    }
    guard coordinatePixelScrollContains(
        display.quartzBounds, request.quartzPoint) else {
        return "point_outside_display"
    }
    guard productionCoordinatePixelScrollFrontmostWindowAtPoint(
        request.quartzPoint) == request.windowID else {
        return "point_occluded"
    }
    return nil
}

final class CoordinatePixelScrollPhysicalMonitorV1 {
    private var baseline: PhysicalInputInterferenceSnapshotV1?
    private let expectedSyntheticHeldModifierFlags: UInt64
    private let observe: () -> PhysicalInputInterferenceSnapshotV1?
    private let settle: () -> Void
    private let maximumSettleAttempts: Int

    init(
        expectedSyntheticHeldModifierFlags: UInt64,
        observe: @escaping () -> PhysicalInputInterferenceSnapshotV1? =
            observePhysicalInputInterferenceV1,
        settle: @escaping () -> Void = {
            Thread.sleep(forTimeInterval: 0.005)
        },
        maximumSettleAttempts: Int = 10
    ) {
        self.expectedSyntheticHeldModifierFlags = expectedSyntheticHeldModifierFlags
        self.observe = observe
        self.settle = settle
        self.maximumSettleAttempts = max(0, maximumSettleAttempts)
        baseline = observe()
    }

    func assess(
        expectedPointer: CoordinateMouseEventPointV1?,
        expectedEvents: [CoordinatePixelScrollExpectedEventV1],
        stage _: String
    ) -> PhysicalInputInterferenceAssessmentV1 {
        let privateEvents = expectedEvents.filter { $0.type != .scrollWheel }
        let unattributedEvents = expectedEvents.filter { $0.type == .scrollWheel }
        var settleAttempt = 0
        while true {
            let current = observe()
            let assessment = assessPhysicalInputInterferenceV1(
                baseline: baseline, current: current,
                expectedPointer: expectedPointer,
                expectedSyntheticEvents:
                    privateEvents.map { ($0.type, $0.count) },
                expectedUnattributedSyntheticEvents:
                    unattributedEvents.map { ($0.type, $0.count) },
                expectedSyntheticHeldModifierFlags:
                    expectedSyntheticHeldModifierFlags)
            if assessment != .unavailable ||
                expectedEvents.isEmpty ||
                settleAttempt >= maximumSettleAttempts {
                if assessment == .unchanged {
                    baseline = current
                }
                return assessment
            }
            settleAttempt += 1
            settle()
        }
    }
}

func coordinatePixelScrollCancellationMarkerURL(
    requestID: Int64,
    helperBootID: String
) -> URL {
    let authority = Data("\(helperBootID):\(requestID)".utf8)
    let digest = SHA256.hash(data: authority)
        .map { String(format: "%02x", $0) }.joined()
    return URL(fileURLWithPath: "/tmp", isDirectory: true).appendingPathComponent(
        "kocoro-ax-pixel-scroll-cancel-v1-\(digest)", isDirectory: false)
}

func productionCoordinatePixelScrollDependenciesV1(
    requestID: Int64,
    helperBootID: String,
    modifiers: [String]
) -> CoordinatePixelScrollDependenciesV1 {
    let marker = coordinatePixelScrollCancellationMarkerURL(
        requestID: requestID, helperBootID: helperBootID)
    let monitor = CoordinatePixelScrollPhysicalMonitorV1(
        expectedSyntheticHeldModifierFlags:
            strictInputModifierFlagsV1(modifiers)!.rawValue)
    return .init(
        authorityFailure: productionCoordinatePixelScrollAuthorityFailure,
        prepare: { point, providerX, providerY, scaleX, scaleY, axis1, axis2 in
            productionCoordinatePixelScrollPreparedEventsV1(
                point: point, providerDeltaX: providerX,
                providerDeltaY: providerY,
                providerToQuartzScaleX: scaleX,
                providerToQuartzScaleY: scaleY,
                expectedCGPointDeltaAxis1: axis1,
                expectedCGPointDeltaAxis2: axis2)
        },
        observePointer: {
            guard let event = CGEvent(source: nil) else { return nil }
            return .init(x: event.location.x, y: event.location.y)
        },
        assessPhysicalInput: monitor.assess,
        isCancelled: {
            FileManager.default.fileExists(atPath: marker.path)
        },
        now: Date.init)
}
