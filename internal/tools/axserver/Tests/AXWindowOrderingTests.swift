import ApplicationServices
import XCTest
@testable import ax_server

final class AXWindowOrderingTests: XCTestCase {
    func testFocusedWindowMovesAheadOfSharingOverlayOrder() {
        let sharingOverlay = AXUIElementCreateApplication(41001)
        let focusedWindow = AXUIElementCreateApplication(41002)

        let ordered = orderAXWindows(
            [sharingOverlay, focusedWindow],
            focusedWindow: focusedWindow)

        XCTAssertEqual(ordered.count, 2)
        XCTAssertTrue(CFEqual(ordered[0], focusedWindow))
        XCTAssertTrue(CFEqual(ordered[1], sharingOverlay))
    }

    func testFocusedWindowMissingFromAXWindowsIsPrepended() {
        let staleWindow = AXUIElementCreateApplication(42001)
        let focusedWindow = AXUIElementCreateApplication(42002)

        let ordered = orderAXWindows(
            [staleWindow],
            focusedWindow: focusedWindow)

        XCTAssertEqual(ordered.count, 2)
        XCTAssertTrue(CFEqual(ordered[0], focusedWindow))
        XCTAssertTrue(CFEqual(ordered[1], staleWindow))
    }

    func testNilFocusedWindowPreservesStableAXWindowsOrder() {
        let first = AXUIElementCreateApplication(43001)
        let second = AXUIElementCreateApplication(43002)

        let ordered = orderAXWindows([first, second], focusedWindow: nil)

        XCTAssertEqual(ordered.count, 2)
        XCTAssertTrue(CFEqual(ordered[0], first))
        XCTAssertTrue(CFEqual(ordered[1], second))
    }
}
