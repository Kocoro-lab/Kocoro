import Foundation
import XCTest
@testable import ax_server

final class PhysicalInputInterferenceTests: XCTestCase {
    func testStableHIDCountersAndExpectedPointerReportNoInterference() {
        let baseline = snapshot(pointerX: 10, counters: [1, 2, 3])
        let current = snapshot(pointerX: 20, counters: [1, 2, 3])

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: baseline,
                current: current,
                expectedPointer: .init(x: 20, y: 40)),
            .unchanged)
    }

    func testAnyHIDCounterChangeReportsPhysicalCompetition() {
        let baseline = snapshot(pointerX: 20, counters: [1, 2, 3])
        let current = snapshot(pointerX: 20, counters: [1, 3, 3])

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: baseline,
                current: current,
                expectedPointer: .init(x: 20, y: 40)),
            .interference)
    }

    func testExpectedSyntheticCounterDeltaIsNotMistakenForUserInput() {
        let counters = Array(repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        var afterMove = counters
        let moveIndex = physicalInputHIDEventTypesV1.firstIndex(of: .mouseMoved)!
        afterMove[moveIndex] += 1
        let syntheticBefore = Array(
            repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count)
        var syntheticAfter = syntheticBefore
        syntheticAfter[moveIndex] += 1

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(
                    pointer: .init(x: 20, y: 40), hidEventCounters: counters,
                    syntheticEventCounters: syntheticBefore),
                current: .init(
                    pointer: .init(x: 30, y: 40), hidEventCounters: afterMove,
                    syntheticEventCounters: syntheticAfter),
                expectedPointer: .init(x: 30, y: 40),
                expectedSyntheticEvents: [(.mouseMoved, 1)]),
            .unchanged)
    }

    func testMissingExpectedSyntheticCounterDeltaFailsClosed() {
        let counters = Array(repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(pointer: nil, hidEventCounters: counters),
                current: .init(pointer: nil, hidEventCounters: counters),
                expectedPointer: nil,
                expectedSyntheticEvents: [(.keyDown, 1), (.keyUp, 1)]),
            .unavailable)
    }

    func testSameTypePhysicalEventIsNotSwallowedByExpectedSyntheticEvent() {
        let counters = Array(repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        let synthetic = Array(repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count)
        var aggregateAfter = counters
        var syntheticAfter = synthetic
        let moveIndex = physicalInputHIDEventTypesV1.firstIndex(of: .mouseMoved)!
        aggregateAfter[moveIndex] += 2
        syntheticAfter[moveIndex] += 1

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(
                    pointer: .init(x: 20, y: 40), hidEventCounters: counters,
                    syntheticEventCounters: synthetic),
                current: .init(
                    pointer: .init(x: 30, y: 40), hidEventCounters: aggregateAfter,
                    syntheticEventCounters: syntheticAfter),
                expectedPointer: .init(x: 30, y: 40),
                expectedSyntheticEvents: [(.mouseMoved, 1)]),
            .interference)
    }

    func testExpectedUnattributedSyntheticEventIsNotMistakenForUserInput() {
        let counters = Array(
            repeating: UInt32(10),
            count: physicalInputHIDEventTypesV1.count)
        let synthetic = Array(
            repeating: UInt32(20),
            count: physicalInputHIDEventTypesV1.count)
        var aggregateAfter = counters
        let scrollIndex = physicalInputHIDEventTypesV1.firstIndex(of: .scrollWheel)!
        aggregateAfter[scrollIndex] += 1

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(
                    pointer: .init(x: 20, y: 40),
                    hidEventCounters: counters,
                    syntheticEventCounters: synthetic),
                current: .init(
                    pointer: .init(x: 20, y: 40),
                    hidEventCounters: aggregateAfter,
                    syntheticEventCounters: synthetic),
                expectedPointer: .init(x: 20, y: 40),
                expectedUnattributedSyntheticEvents: [(.scrollWheel, 1)]),
            .unchanged)
    }

    func testPhysicalEventBesideExpectedUnattributedSyntheticEventIsDetected() {
        let counters = Array(
            repeating: UInt32(10),
            count: physicalInputHIDEventTypesV1.count)
        let synthetic = Array(
            repeating: UInt32(20),
            count: physicalInputHIDEventTypesV1.count)
        var aggregateAfter = counters
        let scrollIndex = physicalInputHIDEventTypesV1.firstIndex(of: .scrollWheel)!
        aggregateAfter[scrollIndex] += 2

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(
                    pointer: .init(x: 20, y: 40),
                    hidEventCounters: counters,
                    syntheticEventCounters: synthetic),
                current: .init(
                    pointer: .init(x: 20, y: 40),
                    hidEventCounters: aggregateAfter,
                    syntheticEventCounters: synthetic),
                expectedPointer: .init(x: 20, y: 40),
                expectedUnattributedSyntheticEvents: [(.scrollWheel, 1)]),
            .interference)
    }

    func testMissingExpectedUnattributedSyntheticEventFailsClosed() {
        let counters = Array(
            repeating: UInt32(10),
            count: physicalInputHIDEventTypesV1.count)

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(pointer: nil, hidEventCounters: counters),
                current: .init(pointer: nil, hidEventCounters: counters),
                expectedPointer: nil,
                expectedUnattributedSyntheticEvents: [(.scrollWheel, 1)]),
            .unavailable)
    }

    func testSyntheticCounterAheadOfAggregateFailsClosed() {
        let counters = Array(repeating: UInt32(10), count: physicalInputHIDEventTypesV1.count)
        let synthetic = Array(repeating: UInt32(20), count: physicalInputHIDEventTypesV1.count)
        var aggregateAfter = counters
        var syntheticAfter = synthetic
        let keyDownIndex = physicalInputHIDEventTypesV1.firstIndex(of: .keyDown)!
        aggregateAfter[keyDownIndex] += 1
        syntheticAfter[keyDownIndex] += 2

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: .init(
                    pointer: nil, hidEventCounters: counters,
                    syntheticEventCounters: synthetic),
                current: .init(
                    pointer: nil, hidEventCounters: aggregateAfter,
                    syntheticEventCounters: syntheticAfter),
                expectedPointer: nil,
                expectedSyntheticEvents: [(.keyDown, 2)]),
            .unavailable)
    }

    func testPointerDriftReportsInterferenceEvenWhenCountersAreStable() {
        let baseline = snapshot(pointerX: 20, counters: [1, 2, 3])
        let current = snapshot(pointerX: 30, counters: [1, 2, 3])

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: baseline,
                current: current,
                expectedPointer: .init(x: 20, y: 40)),
            .interference)
    }

    func testMissingObservationFailsClosed() {
        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: snapshot(pointerX: 20, counters: [1]),
                current: nil,
                expectedPointer: .init(x: 20, y: 40)),
            .unavailable)
    }

    func testHeldPhysicalModifierYieldsBeforeAgentInput() {
        let baseline = PhysicalInputInterferenceSnapshotV1(
            pointer: .init(x: 20, y: 40),
            hidEventCounters: [1],
            heldModifierFlags: 1)

        XCTAssertEqual(
            assessPhysicalInputInterferenceV1(
                baseline: baseline,
                current: baseline,
                expectedPointer: nil),
            .interference)
    }

    func testCapabilityAndScopeDoNotClaimCompleteKeyboardObservation() {
        XCTAssertEqual(
            physicalInputInterferenceCapabilityV1,
            "computer_use_physical_interference_v1")
        XCTAssertEqual(
            physicalInputInterferenceScopeV1,
            "synthetic_pointer_keyboard_drag_semantic_selection_v2_press_v2_scroll_v1_and_pixel_scroll_v1")
    }

    private func snapshot(
        pointerX: Double,
        counters: [UInt32]
    ) -> PhysicalInputInterferenceSnapshotV1 {
        .init(
            pointer: .init(x: pointerX, y: 40),
            hidEventCounters: counters)
    }
}
