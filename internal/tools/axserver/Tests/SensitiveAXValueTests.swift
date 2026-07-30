import XCTest
@testable import ax_server

final class SensitiveAXValueTests: XCTestCase {
    func testElectronStyleCredentialMetadataIsSensitiveBeforeValueRead() {
        let password = AXValueSensitivityMetadata(
            role: "AXTextField",
            subrole: nil,
            identifier: "password",
            title: nil,
            description: "Enter password",
            placeholder: "Password",
            help: nil,
            protectedContent: false)
        let token = AXValueSensitivityMetadata(
            role: "AXTextField",
            subrole: nil,
            identifier: "api_token",
            title: "API Token",
            description: nil,
            placeholder: nil,
            help: nil,
            protectedContent: false)

        XCTAssertTrue(isSensitiveAXValue(password))
        XCTAssertTrue(isSensitiveAXValue(token))
    }

    func testChineseJapaneseAndSecureMetadataAreSensitive() {
        XCTAssertTrue(isSensitiveAXValue(.init(
            role: "AXTextField", subrole: nil, identifier: nil,
            title: "验证码", description: nil, placeholder: nil, help: nil,
            protectedContent: false)))
        XCTAssertTrue(isSensitiveAXValue(.init(
            role: "AXTextField", subrole: nil, identifier: nil,
            title: "認証コード", description: nil, placeholder: nil, help: nil,
            protectedContent: false)))
        XCTAssertTrue(isSensitiveAXValue(.init(
            role: "AXSecureTextField", subrole: nil, identifier: nil,
            title: "Body", description: nil, placeholder: nil, help: nil,
            protectedContent: false)))
    }

    func testOrdinaryBodyFieldKeepsValueAndDoesNotSerializeSecret() throws {
        let metadata = AXValueSensitivityMetadata(
            role: "AXTextArea",
            subrole: nil,
            identifier: "message_body",
            title: "Body",
            description: "Draft message",
            placeholder: "Write a message",
            help: nil,
            protectedContent: false)
        XCTAssertFalse(isSensitiveAXValue(metadata))

        let attributes = AXElementSnapshotAttributes(
            role: metadata.role,
            subrole: metadata.subrole,
            identifier: metadata.identifier,
            title: metadata.title,
            description: metadata.description,
            value: "ordinary draft",
            valueRedacted: isSensitiveAXValue(metadata),
            protectedContent: metadata.protectedContent,
            enabled: true,
            focused: false,
            selected: false,
            actions: [],
            frame: nil)
        let ordinary = makeElementSnapshot(
            attributes: attributes, ref: "e1", path: "window[0]/AXTextArea[0]", children: [])
        XCTAssertEqual(ordinary.value, "ordinary draft")
        XCTAssertFalse(ordinary.valueRedacted)

        let sensitiveAttributes = AXElementSnapshotAttributes(
            role: "AXTextField",
            subrole: nil,
            identifier: "password",
            title: "Password",
            description: nil,
            value: "super-secret",
            valueRedacted: true,
            protectedContent: false,
            enabled: true,
            focused: false,
            selected: false,
            actions: [],
            frame: nil)
        let sensitive = makeElementSnapshot(
            attributes: sensitiveAttributes, ref: "e2", path: "window[0]/AXTextField[0]", children: [])
        let encoded = try XCTUnwrap(String(data: makeWireEncoder().encode(sensitive), encoding: .utf8))
        XCTAssertTrue(sensitive.valueRedacted)
        XCTAssertNil(sensitive.value)
        XCTAssertFalse(encoded.contains("super-secret"))
    }

    func testSensitivePathsDecideBeforeReadingAXValue() throws {
        let paths = [
            ("Perception.swift", "axValue(el, \"AXValue\")"),
            ("Find.swift", "axValue(el, \"AXValue\")"),
            ("SemanticPress.swift", "axValue(element, \"AXValue\")"),
            ("Actions.swift", "axValue(el, \"AXValue\")"),
        ]
        for (source, valueReadNeedle) in paths {
            let contents = try String(contentsOf: sourceURL(source), encoding: .utf8)
            let decision = try XCTUnwrap(contents.range(of: "isSensitiveAXValue"))
            let valueRead = try XCTUnwrap(contents.range(of: valueReadNeedle, range: decision.upperBound..<contents.endIndex))
            XCTAssertLessThan(decision.lowerBound, valueRead.lowerBound, "\(source) reads AXValue before redaction")
        }
    }

    private func sourceURL(_ source: String) -> URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Sources/\(source)")
    }
}
