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

    func testLocalizedInstalledApplicationSelectionRequiresOneExactValidBundle() {
        let systemSettings = URL(fileURLWithPath:
            "/System/Applications/System Settings.app")
        let weather = URL(fileURLWithPath:
            "/System/Applications/Weather.app")
        let bundleIDs = [
            systemSettings.standardizedFileURL.path:
                "com.apple.systempreferences",
            weather.standardizedFileURL.path: "com.apple.weather",
        ]
        let bundleIdentifier: (URL) -> String? = {
            bundleIDs[$0.standardizedFileURL.path]
        }

        XCTAssertEqual(
            uniqueInstalledTaskApplicationURLV1(
                candidates: [systemSettings],
                bundleIdentifier: bundleIdentifier),
            .resolved(systemSettings))
        XCTAssertEqual(uniqueInstalledTaskApplicationURLV1(
            candidates: [systemSettings, weather],
            bundleIdentifier: bundleIdentifier), .ambiguous)
    }

    func testLocalizedInstalledApplicationSelectionFailsClosedForInvalidCandidates() {
        let notAnApp = URL(fileURLWithPath: "/Applications/Notes.txt")
        let missingBundleID = URL(fileURLWithPath: "/Applications/Broken.app")
        XCTAssertEqual(uniqueInstalledTaskApplicationURLV1(
            candidates: [notAnApp, missingBundleID],
            bundleIdentifier: { _ in nil }), .notFound)
    }

    func testLocalizedInstalledApplicationSelectionDeduplicatesOneMetadataPath() {
        let app = URL(fileURLWithPath: "/Applications/Editor.app")
        XCTAssertEqual(
            uniqueInstalledTaskApplicationURLV1(
                candidates: [app, app],
                bundleIdentifier: { _ in "com.example.Editor" }),
            .resolved(app))
    }

    func testLocalizedInstalledApplicationSelectionDeduplicatesSymlinkTarget() throws {
        let temporaryDirectory = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let application = temporaryDirectory
            .appendingPathComponent("Editor.app", isDirectory: true)
        let alias = temporaryDirectory
            .appendingPathComponent("Editor Alias.app", isDirectory: true)
        try FileManager.default.createDirectory(
            at: application,
            withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(
            at: alias,
            withDestinationURL: application)
        defer { try? FileManager.default.removeItem(at: temporaryDirectory) }

        XCTAssertEqual(uniqueInstalledTaskApplicationURLV1(
            candidates: [alias, application],
            bundleIdentifier: { _ in "com.example.Editor" }),
            .resolved(application.resolvingSymlinksInPath().standardizedFileURL))
    }

    func testLocalizedInstalledApplicationSelectionKeepsDistinctCopiesAmbiguous() {
        let first = URL(fileURLWithPath: "/Applications/Editor.app")
        let second = URL(fileURLWithPath: "/Users/test/Applications/Editor.app")
        XCTAssertEqual(uniqueInstalledTaskApplicationURLV1(
            candidates: [first, second],
            bundleIdentifier: { _ in "com.example.Editor" }), .ambiguous)
    }

    func testInstalledApplicationSearchScopesExcludeNetworkAndMountedVolumes() {
        let scopes = installedTaskApplicationSearchScopesV1()
        XCTAssertFalse(scopes.isEmpty)
        XCTAssertFalse(scopes.contains { $0.path.hasPrefix("/Network/") })
        XCTAssertFalse(scopes.contains { $0.path.hasPrefix("/Volumes/") })

        let utilities = URL(fileURLWithPath:
            "/System/Applications/Utilities", isDirectory: true)
        XCTAssertTrue(scopes.contains {
            utilities.path.hasPrefix($0.path + "/")
        })
        XCTAssertTrue(scopes.contains {
            $0.path == "/System/Library/CoreServices/Applications"
        })
    }

    func testLocalizedLookupPreservesTimeoutAndUsesBoundedStandardScopes() {
        var receivedTimeout: TimeInterval?
        var receivedScopes: [URL] = []
        let result = localizedInstalledTaskApplicationLookupV1(
            appName: "Editor",
            timeout: 0.25
        ) { _, scopes, timeout in
            receivedScopes = scopes
            receivedTimeout = timeout
            return .timedOut
        }
        XCTAssertEqual(result, .timedOut)
        XCTAssertEqual(receivedTimeout, 0.25)
        XCTAssertFalse(receivedScopes.contains {
            $0.path.hasPrefix("/Network/") || $0.path.hasPrefix("/Volumes/")
        })
    }

    func testInstalledLookupDiagnosticsAreDistinctAndDoNotExposePaths() {
        let appName = "Editor"
        let notFound = installedTaskApplicationLookupErrorMessageV1(
            appName: appName,
            result: .notFound)
        let ambiguous = installedTaskApplicationLookupErrorMessageV1(
            appName: appName,
            result: .ambiguous)
        let timedOut = installedTaskApplicationLookupErrorMessageV1(
            appName: appName,
            result: .timedOut)

        XCTAssertNotEqual(notFound, ambiguous)
        XCTAssertNotEqual(ambiguous, timedOut)
        XCTAssertTrue(ambiguous?.contains("bundle identifier") == true)
        XCTAssertTrue(timedOut?.contains("timed out") == true)
        for message in [notFound, ambiguous, timedOut].compactMap({ $0 }) {
            XCTAssertFalse(message.contains("/Applications/"))
        }
    }

    func testMetadataQueryLiteralEscapesQuotesAndBackslashesWithoutWildcards() {
        XCTAssertEqual(
            metadataQueryLiteralV1("App \\\"Name*?"),
            "App \\\\\\\"Name\\*\\?")
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
