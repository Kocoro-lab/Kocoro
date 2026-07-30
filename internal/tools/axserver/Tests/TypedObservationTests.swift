import Foundation
import XCTest
@testable import ax_server

final class TypedObservationTests: XCTestCase {
    func testProductionEncoderMatchesCanonicalFixture() throws {
        let produced = try makeWireEncoder().encode(Self.fixtureObservation())
        let fixture = try Data(contentsOf: Self.fixtureURL)

        let producedObject = try JSONSerialization.jsonObject(with: produced) as? NSDictionary
        let fixtureObject = try JSONSerialization.jsonObject(with: fixture) as? NSDictionary
        XCTAssertEqual(producedObject, fixtureObject)
    }

    func testSecureFieldOmitsValueAndMarksRedacted() throws {
        let element = makeElementSnapshot(
            attributes: AXElementSnapshotAttributes(
                role: "AXTextField",
                subrole: "AXSecureTextField",
                identifier: "password",
                title: "Password",
                description: nil,
                value: "hunter2",
                protectedContent: false,
                enabled: true,
                focused: true,
                selected: false,
                actions: ["AXConfirm"],
                frame: AXFrame(x: 40, y: 90, width: 220, height: 28)),
            ref: "e1",
            path: "window[0]/AXTextField[0]",
            children: [])

        let payload = try makeWireEncoder().encode(element)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: payload) as? [String: Any])
        XCTAssertNil(object["value"])
        XCTAssertEqual(object["value_redacted"] as? Bool, true)
        XCTAssertFalse(String(decoding: payload, as: UTF8.self).contains("hunter2"))
    }

    func testUnavailableTopLevelFieldsEncodeAsExplicitNull() throws {
        let observation = ReadTreeResult(
            schemaVersion: 1,
            appName: "Fixture App",
            bundleID: nil,
            pid: 4242,
            windowTitle: "Fixture Window",
            windowID: nil,
            windowFrame: nil,
            focusedRef: nil,
            elements: [],
            refPaths: [:])

        let payload = try makeWireEncoder().encode(observation)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: payload) as? [String: Any])
        for key in ["bundle_id", "window_id", "window_frame", "focused_ref"] {
            XCTAssertTrue(object[key] is NSNull, "\(key) should be explicit null")
        }
    }

    func testFingerprintIgnoresRefsPathsAndChildOrder() {
        let attributes = AXElementSnapshotAttributes(
            role: "AXGroup",
            subrole: nil,
            identifier: "toolbar",
            title: "Editing",
            description: nil,
            value: nil,
            protectedContent: false,
            enabled: true,
            focused: false,
            selected: false,
            actions: [],
            frame: nil)
        let childA = makeElementSnapshot(
            attributes: AXElementSnapshotAttributes(
                role: "AXButton", subrole: nil, identifier: "save", title: "Save",
                description: nil, value: nil, protectedContent: false, enabled: true,
                focused: false, selected: false, actions: ["AXPress"], frame: nil),
            ref: "e2", path: "window[0]/AXGroup[0]/AXButton[0]", children: [])
        let childB = makeElementSnapshot(
            attributes: AXElementSnapshotAttributes(
                role: "AXButton", subrole: nil, identifier: "cancel", title: "Cancel",
                description: nil, value: nil, protectedContent: false, enabled: true,
                focused: false, selected: false, actions: ["AXPress"], frame: nil),
            ref: "e3", path: "window[0]/AXGroup[0]/AXButton[1]", children: [])

        let first = makeElementSnapshot(
            attributes: attributes,
            ref: "e1",
            path: "window[0]/AXGroup[0]",
            children: [childA, childB])
        let reordered = makeElementSnapshot(
            attributes: attributes,
            ref: "e99",
            path: "window[0]/AXGroup[7]",
            children: [childB, childA])

        XCTAssertEqual(first.fingerprint, reordered.fingerprint)
    }

    private static let fixtureURL = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent() // Tests
        .deletingLastPathComponent() // axserver
        .deletingLastPathComponent() // tools
        .appendingPathComponent("testdata/ax_read_tree.response.v1.json")

    private static func fixtureObservation() -> ReadTreeResult {
        let save = makeElementSnapshot(
            attributes: AXElementSnapshotAttributes(
                role: "AXButton",
                subrole: nil,
                identifier: "save-button",
                title: "Save",
                description: "Save document",
                value: nil,
                protectedContent: false,
                enabled: true,
                focused: false,
                selected: false,
                actions: ["AXPress"],
                frame: AXFrame(x: 620, y: 390, width: 90, height: 28)),
            ref: "e1",
            path: "window[0]/AXButton[0]",
            children: [])
        let password = makeElementSnapshot(
            attributes: AXElementSnapshotAttributes(
                role: "AXTextField",
                subrole: "AXSecureTextField",
                identifier: "password-field",
                title: "Password",
                description: nil,
                value: "must-not-cross-wire",
                protectedContent: true,
                enabled: true,
                focused: true,
                selected: false,
                actions: ["AXConfirm"],
                frame: AXFrame(x: 620, y: 430, width: 220, height: 28)),
            ref: "e2",
            path: "window[0]/AXTextField[0]",
            children: [])

        return ReadTreeResult(
            schemaVersion: 1,
            appName: "Fixture App",
            bundleID: "run.shannon.ax-fixture",
            pid: 4242,
            windowTitle: "Fixture Window",
            windowID: nil,
            windowFrame: AXFrame(x: 500, y: 250, width: 800, height: 600),
            focusedRef: "e2",
            elements: [save, password],
            refPaths: [
                "e1": RefEntry(path: save.path, role: save.role, fingerprint: save.fingerprint),
                "e2": RefEntry(path: password.path, role: password.role, fingerprint: password.fingerprint),
            ])
    }
}
