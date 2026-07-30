import XCTest
@testable import ax_server

final class WindowIdentityTests: XCTestCase {
    func testFrontmostNormalCGWindowDoesNotRequireAXIdentity() {
        let windows = [
            CGWindowIdentityCandidate(
                windowID: 41,
                ownerPID: 900,
                layer: 0,
                title: "Slack",
                frame: AXFrame(x: 10, y: 20, width: 800, height: 600)),
            CGWindowIdentityCandidate(
                windowID: 42,
                ownerPID: 901,
                layer: 0,
                title: "Other",
                frame: AXFrame(x: 0, y: 0, width: 100, height: 100)),
        ]

        let target = frontmostNormalCGWindow(pid: 900, candidates: windows)

        XCTAssertEqual(target?.windowID, 41)
        XCTAssertEqual(target?.frame, AXFrame(x: 10, y: 20, width: 800, height: 600))
    }

    func testWindowIdentityMatchesPIDTitleAndBoundsWithinTolerance() {
        let observation = WindowIdentityObservation(
            pid: 42,
            title: "Document",
            frame: AXFrame(x: 100, y: 200, width: 800, height: 600))
        let candidates = [
            CGWindowIdentityCandidate(
                windowID: 7001,
                ownerPID: 42,
                layer: 0,
                title: "Document",
                frame: AXFrame(x: 101.5, y: 199, width: 800.5, height: 599)),
            CGWindowIdentityCandidate(
                windowID: 7002,
                ownerPID: 99,
                layer: 0,
                title: "Document",
                frame: observation.frame),
        ]

        XCTAssertEqual(matchWindowIdentity(observation, candidates: candidates), .unique(7001))
    }

    func testWindowIdentityRejectsBoundsOutsideTolerance() {
        let observation = WindowIdentityObservation(
            pid: 42,
            title: "Document",
            frame: AXFrame(x: 100, y: 200, width: 800, height: 600))
        let candidate = CGWindowIdentityCandidate(
            windowID: 7001,
            ownerPID: 42,
            layer: 0,
            title: "Document",
            frame: AXFrame(x: 103, y: 200, width: 800, height: 600))

        XCTAssertEqual(matchWindowIdentity(observation, candidates: [candidate]), .none)
    }

    func testBlankCGTitleDoesNotRequireScreenRecordingForUniqueBoundsMatch() {
        let observation = WindowIdentityObservation(
            pid: 42,
            title: "Private Document",
            frame: AXFrame(x: 100, y: 200, width: 800, height: 600))
        let candidate = CGWindowIdentityCandidate(
            windowID: 7001,
            ownerPID: 42,
            layer: 0,
            title: nil,
            frame: observation.frame)

        XCTAssertEqual(matchWindowIdentity(observation, candidates: [candidate]), .unique(7001))
    }

    func testSameTitleTwinWindowsRemainAmbiguous() {
        let observation = WindowIdentityObservation(
            pid: 42,
            title: "Untitled",
            frame: AXFrame(x: 100, y: 200, width: 800, height: 600))
        let candidates = [7001, 7002].map {
            CGWindowIdentityCandidate(
                windowID: $0,
                ownerPID: 42,
                layer: 0,
                title: "Untitled",
                frame: observation.frame)
        }

        XCTAssertEqual(matchWindowIdentity(observation, candidates: candidates), .ambiguous)
    }

    func testFocusedFrontmostWindowUsesCGZOrderToResolveVisibleTwins() {
        let observation = WindowIdentityObservation(
            pid: 42,
            title: "Document",
            frame: AXFrame(x: 100, y: 200, width: 800, height: 600))
        let candidates = [
            CGWindowIdentityCandidate(
                windowID: 7000,
                ownerPID: 42,
                layer: 0,
                title: "Document",
                frame: observation.frame,
                isOnScreen: false,
                alpha: 0),
            CGWindowIdentityCandidate(
                windowID: 7002,
                ownerPID: 42,
                layer: 0,
                title: "Document",
                frame: observation.frame),
            CGWindowIdentityCandidate(
                windowID: 7001,
                ownerPID: 42,
                layer: 0,
                title: "Document",
                frame: observation.frame),
        ]

        XCTAssertEqual(
            matchFocusedWindowIdentity(
                observation,
                candidates: candidates,
                targetProcessIsFrontmost: true),
            .unique(7002))
        XCTAssertEqual(
            matchFocusedWindowIdentity(
                observation,
                candidates: candidates,
                targetProcessIsFrontmost: false),
            .ambiguous)
    }
}
