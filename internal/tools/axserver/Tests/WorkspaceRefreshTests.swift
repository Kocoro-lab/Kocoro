import Foundation
import XCTest
@testable import ax_server

final class WorkspaceRefreshTests: XCTestCase {
    func testRefreshAppKitStateProcessesQueuedDefaultModeWork() {
        var processed = false
        RunLoop.current.perform(inModes: [.default]) {
            processed = true
        }

        refreshAppKitState(for: 0.05)

        XCTAssertTrue(processed)
    }
}
