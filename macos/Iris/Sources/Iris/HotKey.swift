import AppKit
import Carbon.HIToolbox

/// A global shortcut through Carbon's RegisterEventHotKey: it works while
/// any app is in front and needs no Accessibility permission.
@MainActor
final class HotKey {
    struct Combo: Codable, Equatable {
        var keyCode: UInt32
        var modifiers: UInt32 // Carbon: cmdKey, optionKey, controlKey, shiftKey
        var display: String
    }

    static let defaultCombo = Combo(keyCode: UInt32(kVK_ANSI_I), modifiers: UInt32(cmdKey | optionKey), display: "⌥⌘I")

    /// Called on the main thread when the shortcut is pressed.
    static var action: (() -> Void)?
    private static var handlerInstalled = false
    private var ref: EventHotKeyRef?

    func register(_ c: Combo) {
        unregister()
        HotKey.installHandler()
        let id = EventHotKeyID(signature: OSType(0x4952_4953), id: 1) // "IRIS"
        let status = RegisterEventHotKey(c.keyCode, c.modifiers, id, GetApplicationEventTarget(), 0, &ref)
        if status != noErr { NSLog("iris: shortcut \(c.display) not registered (\(status)); taken by another app?") }
    }

    func unregister() {
        if let r = ref {
            UnregisterEventHotKey(r)
            ref = nil
        }
    }

    private static func installHandler() {
        guard !handlerInstalled else { return }
        handlerInstalled = true
        var type = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
        InstallEventHandler(GetApplicationEventTarget(), { _, _, _ in
            Task { @MainActor in HotKey.action?() }
            return noErr
        }, 1, &type, nil, nil)
    }

    /// The combo of a key press, if it has ⌘, ⌥ or ⌃ (plain keys would
    /// swallow typing everywhere).
    static func combo(from e: NSEvent) -> Combo? {
        let f = e.modifierFlags.intersection(.deviceIndependentFlagsMask)
        guard f.contains(.command) || f.contains(.option) || f.contains(.control) else { return nil }
        var m: UInt32 = 0
        var d = ""
        if f.contains(.control) { m |= UInt32(controlKey); d += "⌃" }
        if f.contains(.option) { m |= UInt32(optionKey); d += "⌥" }
        if f.contains(.shift) { m |= UInt32(shiftKey); d += "⇧" }
        if f.contains(.command) { m |= UInt32(cmdKey); d += "⌘" }
        return Combo(keyCode: UInt32(e.keyCode), modifiers: m, display: d + keyName(e))
    }

    private static func keyName(_ e: NSEvent) -> String {
        switch Int(e.keyCode) {
        case kVK_Space: return "Space"
        case kVK_Return: return "↩"
        case kVK_Tab: return "⇥"
        case kVK_Delete: return "⌫"
        case kVK_UpArrow: return "↑"
        case kVK_DownArrow: return "↓"
        case kVK_LeftArrow: return "←"
        case kVK_RightArrow: return "→"
        default:
            let s = e.charactersIgnoringModifiers ?? ""
            return s.isEmpty ? "#\(e.keyCode)" : s.uppercased()
        }
    }

    // MARK: stored setting

    private static let defaultsKey = "requestShortcut"

    static var saved: Combo {
        get {
            guard let d = UserDefaults.standard.data(forKey: defaultsKey),
                  let c = try? JSONDecoder().decode(Combo.self, from: d) else { return defaultCombo }
            return c
        }
        set {
            if let d = try? JSONEncoder().encode(newValue) { UserDefaults.standard.set(d, forKey: defaultsKey) }
        }
    }
}
