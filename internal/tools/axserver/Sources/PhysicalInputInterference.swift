import CoreGraphics
import Foundation

// This capability is intentionally narrower than "all user input". V1 does
// not install a global event tap and does not request Input Monitoring. It
// samples the cursor plus the HID-system event-state table exposed by
// CGEventSource. The table covers hardware event counts, mouse buttons, and
// modifier flags; it does not expose event payloads and cannot prove that
// every ordinary key already held before the first sample is visible.
let physicalInputInterferenceCapabilityV1 = "computer_use_physical_interference_v1"
let physicalInputInterferenceScopeV1 =
    "synthetic_pointer_keyboard_drag_semantic_selection_v2_press_v2_scroll_v1_and_pixel_scroll_v1"

// Keep one private source alive for the full helper process. Events created by
// this source still increment the aggregate HID counters when posted at
// cghidEventTap on current macOS, but they also increment this private table.
// Sampling both tables lets us subtract observed Kocoro events without
// guessing a fixed delta or swallowing a same-type physical event.
let physicalInputSyntheticEventSourceV1 = CGEventSource(stateID: .privateState)

struct PhysicalInputInterferenceSnapshotV1: Equatable {
    let pointer: CoordinateMouseEventPointV1?
    let hidEventCounters: [UInt32]
    let syntheticEventCounters: [UInt32]
    let heldMouseButtons: UInt32
    let heldModifierFlags: UInt64
    let syntheticHeldMouseButtons: UInt32
    let syntheticHeldModifierFlags: UInt64

    init(
        pointer: CoordinateMouseEventPointV1?,
        hidEventCounters: [UInt32],
        syntheticEventCounters: [UInt32]? = nil,
        heldMouseButtons: UInt32 = 0,
        heldModifierFlags: UInt64 = 0,
        syntheticHeldMouseButtons: UInt32 = 0,
        syntheticHeldModifierFlags: UInt64 = 0
    ) {
        self.pointer = pointer
        self.hidEventCounters = hidEventCounters
        self.syntheticEventCounters = syntheticEventCounters ?? Array(
            repeating: 0, count: hidEventCounters.count)
        self.heldMouseButtons = heldMouseButtons
        self.heldModifierFlags = heldModifierFlags
        self.syntheticHeldMouseButtons = syntheticHeldMouseButtons
        self.syntheticHeldModifierFlags = syntheticHeldModifierFlags
    }
}

enum PhysicalInputInterferenceAssessmentV1: Equatable {
    case unchanged
    case interference
    case unavailable
}

private let physicalInputPointerToleranceV1 = 2.0

func assessPhysicalInputInterferenceV1(
    baseline: PhysicalInputInterferenceSnapshotV1?,
    current: PhysicalInputInterferenceSnapshotV1?,
    expectedPointer: CoordinateMouseEventPointV1?,
    expectedSyntheticEvents: [(CGEventType, UInt32)]? = [],
    expectedUnattributedSyntheticEvents: [(CGEventType, UInt32)]? = [],
    expectedSyntheticHeldMouseButtons: UInt32 = 0,
    expectedSyntheticHeldModifierFlags: UInt64 = 0,
    tolerance: Double = physicalInputPointerToleranceV1
) -> PhysicalInputInterferenceAssessmentV1 {
    guard let baseline, let current, let expectedSyntheticEvents,
          let expectedUnattributedSyntheticEvents,
          baseline.hidEventCounters.count == current.hidEventCounters.count,
          baseline.syntheticEventCounters.count == current.syntheticEventCounters.count,
          baseline.hidEventCounters.count == baseline.syntheticEventCounters.count else {
        return .unavailable
    }
    // A physically held mouse button or modifier means the user is already
    // interacting with the machine. Yield before combining that state with an
    // agent-generated held-input sequence.
    guard current.syntheticHeldMouseButtons == expectedSyntheticHeldMouseButtons,
          current.syntheticHeldModifierFlags == expectedSyntheticHeldModifierFlags else {
        return .unavailable
    }
    guard current.heldMouseButtons & ~current.syntheticHeldMouseButtons == 0,
          current.heldModifierFlags & ~current.syntheticHeldModifierFlags == 0 else {
        return .interference
    }
    var expectedDeltas = Array(repeating: UInt32(0), count: baseline.hidEventCounters.count)
    for (eventType, count) in expectedSyntheticEvents {
        guard let index = physicalInputHIDEventTypesV1.firstIndex(of: eventType),
              index < expectedDeltas.count else { return .unavailable }
        expectedDeltas[index] &+= count
    }
    var expectedUnattributedDeltas = Array(
        repeating: UInt32(0), count: baseline.hidEventCounters.count)
    for (eventType, count) in expectedUnattributedSyntheticEvents {
        guard let index = physicalInputHIDEventTypesV1.firstIndex(of: eventType),
              index < expectedUnattributedDeltas.count else { return .unavailable }
        expectedUnattributedDeltas[index] &+= count
    }
    for index in baseline.hidEventCounters.indices {
        let aggregate = current.hidEventCounters[index] &- baseline.hidEventCounters[index]
        let own = current.syntheticEventCounters[index] &- baseline.syntheticEventCounters[index]
        if own != expectedDeltas[index] || aggregate < own { return .unavailable }
        let unattributed = aggregate &- own
        if unattributed < expectedUnattributedDeltas[index] { return .unavailable }
        if unattributed > expectedUnattributedDeltas[index] { return .interference }
    }
    if let expectedPointer {
        guard let pointer = current.pointer else { return .unavailable }
        guard abs(pointer.x - expectedPointer.x) <= tolerance,
              abs(pointer.y - expectedPointer.y) <= tolerance else {
            return .interference
        }
    }
    return .unchanged
}

let physicalInputHIDEventTypesV1: [CGEventType] = [
    .leftMouseDown, .leftMouseUp,
    .rightMouseDown, .rightMouseUp,
    .otherMouseDown, .otherMouseUp,
    .mouseMoved, .leftMouseDragged, .rightMouseDragged, .otherMouseDragged,
    .scrollWheel,
    .keyDown, .keyUp, .flagsChanged,
    .tabletPointer, .tabletProximity,
]

private let physicalInputModifierMaskV1: CGEventFlags = [
    .maskShift, .maskControl, .maskAlternate, .maskCommand,
    .maskSecondaryFn,
]

func observePhysicalInputInterferenceV1() -> PhysicalInputInterferenceSnapshotV1? {
    guard let pointerEvent = CGEvent(source: nil),
          let syntheticSource = physicalInputSyntheticEventSourceV1 else { return nil }
    let location = pointerEvent.location
    let counters = physicalInputHIDEventTypesV1.map {
        CGEventSource.counterForEventType(.hidSystemState, eventType: $0)
    }
    let syntheticCounters = physicalInputHIDEventTypesV1.map {
        CGEventSource.counterForEventType(syntheticSource.sourceStateID, eventType: $0)
    }
    var heldMouseButtons: UInt32 = 0
    var syntheticHeldMouseButtons: UInt32 = 0
    for rawButton in 0..<32 {
        guard let button = CGMouseButton(rawValue: UInt32(rawButton)) else { continue }
        if CGEventSource.buttonState(.hidSystemState, button: button) {
            heldMouseButtons |= UInt32(1) << UInt32(rawButton)
        }
        if CGEventSource.buttonState(syntheticSource.sourceStateID, button: button) {
            syntheticHeldMouseButtons |= UInt32(1) << UInt32(rawButton)
        }
    }
    let flags = CGEventSource.flagsState(.hidSystemState)
        .intersection(physicalInputModifierMaskV1)
    let syntheticFlags = CGEventSource.flagsState(syntheticSource.sourceStateID)
        .intersection(physicalInputModifierMaskV1)
    return PhysicalInputInterferenceSnapshotV1(
        pointer: .init(x: Double(location.x), y: Double(location.y)),
        hidEventCounters: counters,
        syntheticEventCounters: syntheticCounters,
        heldMouseButtons: heldMouseButtons,
        heldModifierFlags: flags.rawValue,
        syntheticHeldMouseButtons: syntheticHeldMouseButtons,
        syntheticHeldModifierFlags: syntheticFlags.rawValue)
}
