import ApplicationServices

/// Metadata that can be inspected without reading `AXValue`. Keep this type
/// pure so every AX path makes the same redaction decision before touching a
/// potentially secret value.
struct AXValueSensitivityMetadata: Equatable {
    let role: String
    let subrole: String?
    let identifier: String?
    let title: String?
    let description: String?
    let placeholder: String?
    let help: String?
    let protectedContent: Bool
}

private let sensitiveAXValueTokens = [
    // English.
    "password", "passcode", "otp", "one-time", "one time", "verification code",
    "verification", "token", "secret", "api key", "apikey", "access key", "pin",
    "cvv", "cvc", "2fa", "mfa", "auth code", "security code",
    // Simplified and Traditional Chinese.
    "密码", "密碼", "口令", "口令", "验证码", "驗證碼", "校验码", "校驗碼",
    "动态码", "動態碼", "一次性密码", "一次性密碼", "令牌", "密钥", "密鑰",
    "秘钥", "秘鑰", "访问密钥", "訪問密鑰", "安全码", "安全碼",
    // Japanese.
    "パスワード", "パスコード", "暗証番号", "認証コード", "認証番号",
    "ワンタイム", "検証コード", "トークン", "シークレット", "秘密鍵",
    "apiキー", "アクセスキー", "二段階", "多要素",
]

func isSensitiveAXValue(_ metadata: AXValueSensitivityMetadata) -> Bool {
    if metadata.protectedContent || metadata.role == "AXSecureTextField" ||
        metadata.subrole == "AXSecureTextField" {
        return true
    }
    let labels = [
        metadata.identifier, metadata.title, metadata.description,
        metadata.placeholder, metadata.help,
    ].compactMap { $0?.lowercased() }
    return labels.contains { label in
        sensitiveAXValueTokens.contains { token in label.contains(token) }
    }
}

func axValueSensitivityMetadata(
    _ element: AXUIElement,
    role: String? = nil,
    subrole: String? = nil,
    identifier: String? = nil,
    title: String? = nil,
    description: String? = nil,
    protectedContent: Bool? = nil
) -> AXValueSensitivityMetadata {
    AXValueSensitivityMetadata(
        role: role ?? axString(element, "AXRole") ?? "",
        subrole: subrole ?? axString(element, "AXSubrole"),
        identifier: identifier ?? axString(element, "AXIdentifier"),
        title: title ?? axString(element, "AXTitle"),
        description: description ?? axString(element, "AXDescription"),
        placeholder: axString(element, "AXPlaceholderValue"),
        help: axString(element, "AXHelp"),
        protectedContent: protectedContent ?? axBool(element, "AXProtectedContent") ?? false)
}
