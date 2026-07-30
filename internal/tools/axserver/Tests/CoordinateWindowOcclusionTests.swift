import CoreGraphics
import XCTest
@testable import ax_server

final class CoordinateWindowOcclusionTests: XCTestCase {
    func testSystemCompositionLayerDoesNotHideFrontmostNormalWindow() {
        let entries = [
            window(id: 24, layer: 20, bounds: CGRect(x: 0, y: 0, width: 1512, height: 982)),
            window(id: 7001, layer: 0, bounds: CGRect(x: 436, y: 128, width: 640, height: 512)),
            window(id: 8001, layer: 0, bounds: CGRect(x: 0, y: 33, width: 1512, height: 889)),
        ]

        XCTAssertEqual(
            coordinateFrontmostNormalWindowID(
                at: CGPoint(x: 756, y: 384), entries: entries),
            7001)
    }

    func testFrontmostNormalWindowStillDetectsOcclusion() {
        let entries = [
            window(id: 7002, layer: 0, bounds: CGRect(x: 500, y: 300, width: 200, height: 200)),
            window(id: 7001, layer: 0, bounds: CGRect(x: 436, y: 128, width: 640, height: 512)),
        ]

        XCTAssertEqual(
            coordinateFrontmostNormalWindowID(
                at: CGPoint(x: 600, y: 400), entries: entries),
            7002)
    }

    func testTransparentNormalWindowIsIgnored() {
        let entries = [
            window(
                id: 7002, layer: 0,
                bounds: CGRect(x: 500, y: 300, width: 200, height: 200),
                alpha: 0),
            window(id: 7001, layer: 0, bounds: CGRect(x: 436, y: 128, width: 640, height: 512)),
        ]

        XCTAssertEqual(
            coordinateFrontmostNormalWindowID(
                at: CGPoint(x: 600, y: 400), entries: entries),
            7001)
    }

    private func window(
        id: UInt32,
        layer: Int,
        bounds: CGRect,
        alpha: Double = 1
    ) -> [String: Any] {
        [
            kCGWindowNumber as String: NSNumber(value: id),
            kCGWindowLayer as String: NSNumber(value: layer),
            kCGWindowBounds as String: bounds.dictionaryRepresentation,
            kCGWindowIsOnscreen as String: NSNumber(value: true),
            kCGWindowAlpha as String: NSNumber(value: alpha),
        ]
    }
}
