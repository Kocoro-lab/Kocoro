import XCTest
@testable import ax_server

final class RunningApplicationSelectionTests: XCTestCase {
    func testForegroundPreparationTurnsAmbiguousWindowlessSetIntoExactLaunchReceipt() {
        XCTAssertEqual(
            foregroundTaskAppPreparationDecision(.ambiguous),
            .launchExact)
        XCTAssertEqual(
            foregroundTaskAppPreparationDecision(.notRunning),
            .launchExact)
        XCTAssertEqual(
            foregroundTaskAppPreparationDecision(.selected(77)),
            .reuseExact(77))

        let configuration = foregroundTaskAppLaunchConfigurationV1()
        XCTAssertTrue(configuration.activates)
        XCTAssertTrue(configuration.createsNewApplicationInstance)
    }

    func testForegroundReuseCannotCrossResolvedBundleIdentity() {
        XCTAssertTrue(foregroundTaskAppReuseMatchesExpectedBundleV1(
            candidateBundleID: "com.example.Editor",
            expectedBundleID: "com.example.Editor"))
        XCTAssertFalse(foregroundTaskAppReuseMatchesExpectedBundleV1(
            candidateBundleID: "com.other.Editor",
            expectedBundleID: "com.example.Editor"))
        XCTAssertFalse(foregroundTaskAppReuseMatchesExpectedBundleV1(
            candidateBundleID: nil,
            expectedBundleID: "com.example.Editor"))
    }

    func testForegroundWindowReadinessRequiresVisibleNormalCGWindow() {
        let hidden = CGWindowIdentityCandidate(
            windowID: 101,
            ownerPID: 77,
            layer: 0,
            title: "Hidden",
            frame: AXFrame(x: 0, y: 0, width: 800, height: 600),
            isOnScreen: false)
        let overlay = CGWindowIdentityCandidate(
            windowID: 102,
            ownerPID: 77,
            layer: 3,
            title: "Overlay",
            frame: AXFrame(x: 0, y: 0, width: 800, height: 600))
        let transparent = CGWindowIdentityCandidate(
            windowID: 103,
            ownerPID: 77,
            layer: 0,
            title: "Transparent",
            frame: AXFrame(x: 0, y: 0, width: 800, height: 600),
            alpha: 0)
        let visible = CGWindowIdentityCandidate(
            windowID: 104,
            ownerPID: 77,
            layer: 0,
            title: "Visible",
            frame: AXFrame(x: 0, y: 0, width: 800, height: 600))

        XCTAssertFalse(foregroundTaskAppHasCapturableWindowV1(
            targetPID: 77,
            candidates: [hidden, overlay, transparent]))
        XCTAssertTrue(foregroundTaskAppHasCapturableWindowV1(
            targetPID: 77,
            candidates: [hidden, visible]))
        XCTAssertFalse(foregroundTaskAppHasCapturableWindowV1(
            targetPID: 88,
            candidates: [visible]))
        XCTAssertEqual(foregroundTaskAppWindowReadyTimeoutV1(), 5)
    }

    func testIdentityResolutionFallsBackToInstalledBundleForAmbiguousProcesses() {
        XCTAssertTrue(
            taskAppIdentityNeedsInstalledLookupV1(.ambiguous))
        XCTAssertTrue(
            taskAppIdentityNeedsInstalledLookupV1(.notRunning))
        XCTAssertFalse(
            taskAppIdentityNeedsInstalledLookupV1(.selected(77)))
    }

    func testBackgroundLaunchConfigurationNeverActivatesTarget() {
        let configuration = backgroundTaskAppLaunchConfigurationV1()
        XCTAssertFalse(configuration.activates)
        XCTAssertTrue(configuration.createsNewApplicationInstance)
    }

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

    func testBackgroundWindowReadinessUsesOneBoundedGracePeriod() {
        XCTAssertEqual(
            backgroundTaskAppWindowReadyTimeoutV1(),
            2)
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
