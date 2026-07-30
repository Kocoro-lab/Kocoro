import CoreGraphics
import Foundation

func coordinateFrontmostNormalWindowID(
    at point: CGPoint,
    entries: [[String: Any]]
) -> UInt32? {
    // CGWindowList is front-to-back, but it also contains system composition
    // surfaces such as Dock's opaque, display-sized layer-20 window. Those
    // surfaces are not normal hit-testable app windows and must not shadow the
    // target. Coordinate mutations only admit layer-0 target windows, so apply
    // the same boundary when checking normal-window occlusion.
    for entry in entries {
        guard let rawBounds = entry[kCGWindowBounds as String],
              CFGetTypeID(rawBounds as CFTypeRef) == CFDictionaryGetTypeID(),
              let bounds = CGRect(
                  dictionaryRepresentation: rawBounds as! CFDictionary),
              bounds.contains(point) else {
            continue
        }
        guard let layer = entry[kCGWindowLayer as String] as? NSNumber else {
            return nil
        }
        if layer.intValue != 0 {
            continue
        }
        guard let onScreen = entry[kCGWindowIsOnscreen as String] as? NSNumber,
              onScreen.boolValue,
              let alpha = entry[kCGWindowAlpha as String] as? NSNumber else {
            return nil
        }
        if alpha.doubleValue <= 0 {
            continue
        }
        guard let number = entry[kCGWindowNumber as String] as? NSNumber,
              let exact = UInt32(exactly: number.uint64Value),
              exact > 0 else {
            return nil
        }
        return exact
    }
    return nil
}

func coordinateFrontmostNormalWindowID(at point: CGPoint) -> UInt32? {
    guard let entries = CGWindowListCopyWindowInfo(
        [.optionOnScreenOnly, .excludeDesktopElements],
        kCGNullWindowID) as? [[String: Any]] else {
        return nil
    }
    return coordinateFrontmostNormalWindowID(at: point, entries: entries)
}
