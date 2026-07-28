import XCTest
@testable import ax_server

final class RunningApplicationSelectionTests: XCTestCase {
    func testBackgroundTaskBindingRequiresVisibleNonFrontmostExactPID() {
        XCTAssertEqual(
            backgroundTaskAppEligibility(
                targetPID: 77,
                frontmostPID: 11,
                visibleNormalWindowPIDs: [11, 77]),
            .eligible)
        XCTAssertEqual(
            backgroundTaskAppEligibility(
                targetPID: 77,
                frontmostPID: 77,
                visibleNormalWindowPIDs: [77]),
            .targetIsFrontmost)
        XCTAssertEqual(
            backgroundTaskAppEligibility(
                targetPID: 77,
                frontmostPID: 11,
                visibleNormalWindowPIDs: [11]),
            .noVisibleWindow)
    }

    func testExcludedAutomationInstanceCannotWinSameBundleSelection() {
        let candidates = [
            RunningApplicationSelectionCandidate(
                pid: 77,
                localizedName: "Google Chrome",
                bundleID: "com.google.Chrome"),
            RunningApplicationSelectionCandidate(
                pid: 9002,
                localizedName: "Google Chrome",
                bundleID: "com.google.Chrome"),
        ]

        XCTAssertEqual(
            selectRunningApplication(
                appName: "Google Chrome",
                candidates: candidates,
                excludedPIDs: [9002],
                frontmostPID: 9002,
                visibleWindowPIDsFrontToBack: [9002, 77]),
            .selected(77))
    }

    func testFrontmostMatchingInstanceWinsBeforeWindowOrder() {
        let candidates = [
            RunningApplicationSelectionCandidate(
                pid: 11, localizedName: "Editor",
                bundleID: "com.example.Editor"),
            RunningApplicationSelectionCandidate(
                pid: 22, localizedName: "Editor",
                bundleID: "com.example.Editor"),
        ]

        XCTAssertEqual(
            selectRunningApplication(
                appName: "com.example.Editor",
                candidates: candidates,
                excludedPIDs: [],
                frontmostPID: 22,
                visibleWindowPIDsFrontToBack: [11, 22]),
            .selected(22))
    }

    func testVisibleWindowOrderSelectsOneNonFrontmostInstance() {
        let candidates = [
            RunningApplicationSelectionCandidate(
                pid: 11, localizedName: "Editor",
                bundleID: "com.example.Editor"),
            RunningApplicationSelectionCandidate(
                pid: 22, localizedName: "Editor",
                bundleID: "com.example.Editor"),
        ]

        XCTAssertEqual(
            selectRunningApplication(
                appName: "Editor",
                candidates: candidates,
                excludedPIDs: [],
                frontmostPID: nil,
                visibleWindowPIDsFrontToBack: [22, 11]),
            .selected(22))
    }

    func testMultipleWindowlessInstancesAreAmbiguousInsteadOfFirstMatch() {
        let candidates = [
            RunningApplicationSelectionCandidate(
                pid: 11, localizedName: "Editor",
                bundleID: "com.example.Editor"),
            RunningApplicationSelectionCandidate(
                pid: 22, localizedName: "Editor",
                bundleID: "com.example.Editor"),
        ]

        XCTAssertEqual(
            selectRunningApplication(
                appName: "Editor",
                candidates: candidates,
                excludedPIDs: [],
                frontmostPID: nil,
                visibleWindowPIDsFrontToBack: []),
            .ambiguous)
    }

    func testOnlyExcludedMatchesBehaveAsNotRunning() {
        let candidates = [
            RunningApplicationSelectionCandidate(
                pid: 9002,
                localizedName: "Google Chrome",
                bundleID: "com.google.Chrome"),
        ]

        XCTAssertEqual(
            selectRunningApplication(
                appName: "Google Chrome",
                candidates: candidates,
                excludedPIDs: [9002],
                frontmostPID: 9002,
                visibleWindowPIDsFrontToBack: [9002]),
            .notRunning)
    }

    func testExactNameTierStillPrecedesBundleSuffixFallback() {
        let candidates = [
            RunningApplicationSelectionCandidate(
                pid: 11, localizedName: "Chrome",
                bundleID: "com.example.chrome"),
            RunningApplicationSelectionCandidate(
                pid: 22, localizedName: "Other",
                bundleID: "org.example.Chrome"),
        ]

        XCTAssertEqual(
            selectRunningApplication(
                appName: "Chrome",
                candidates: candidates,
                excludedPIDs: [],
                frontmostPID: 22,
                visibleWindowPIDsFrontToBack: [22, 11]),
            .selected(11))
    }
}
