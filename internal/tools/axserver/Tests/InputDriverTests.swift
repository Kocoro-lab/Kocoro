import CoreGraphics
import XCTest
@testable import ax_server

final class InputDriverTests: XCTestCase {
    func testPointMustFallInsideAnActiveDisplay() {
        let displays = [
            CGRect(x: -1280, y: 0, width: 1280, height: 800),
            CGRect(x: 0, y: 0, width: 1512, height: 982),
        ]

        XCTAssertTrue(InputDriver.isPointOnScreen(CGPoint(x: 0, y: 0), displayBounds: displays))
        XCTAssertTrue(InputDriver.isPointOnScreen(CGPoint(x: -1, y: 400), displayBounds: displays))
        XCTAssertFalse(InputDriver.isPointOnScreen(CGPoint(x: 1512, y: 400), displayBounds: displays))
        XCTAssertFalse(InputDriver.isPointOnScreen(CGPoint(x: 400, y: 982), displayBounds: displays))
    }

    func testPointerReadBackAllowsOnlySmallRoundingDifference() {
        let requested = CGPoint(x: 578, y: 559)

        XCTAssertTrue(InputDriver.pointerReached(requested, observed: CGPoint(x: 578.8, y: 558.2)))
        XCTAssertFalse(InputDriver.pointerReached(requested, observed: CGPoint(x: 614, y: 279)))
    }
}
