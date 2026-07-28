// InputInjector turns the tiny line protocol the Go host forwards on stdin into
// macOS CGEvents (mouse move/click/drag/scroll, key down/up with modifiers).
//
// Modifier state is ABSOLUTE, not accumulated: every command carries the current
// modifier bitmask (from the browser event's e.shiftKey/ctrlKey/altKey/metaKey), and
// we apply exactly that. Browsers routinely drop keyup events (focus changes, OS
// shortcuts), so tracking down/up transitions leaves modifiers stuck — e.g. a stuck
// Cmd turns every 'a' into Cmd+A and every Dock click into "reveal in Finder". Reading
// the absolute state per event self-heals the moment any event reports the key as up.
//
// Posting CGEvents requires the Accessibility permission granted to this binary. In
// dryRun mode nothing is posted (used to verify decoding without touching the session).
//
// Line protocol (host-validated; modifier bitmask f = shift1|ctrl2|alt4|meta8):
//   m <x> <y> <f>    mouse move, x,y normalized 0..1 across the main display
//   r <dx> <dy> <f>  relative mouse move (game mode / pointer lock), pixel deltas
//   d <button> <x> <y> <f>  button down at position, 0=left 1=middle 2=right
//   D <button> <f>   button down at current cursor (game mode has no position)
//   u <button> <f>   button up
//   w <dx> <dy> <f>  scroll, pixel deltas
//   k <code> <f>     key down, code = browser KeyboardEvent.code
//   K <code> <f>     key up
//   x                release everything (lift held buttons, clear modifiers)

import Foundation
import CoreGraphics
import ApplicationServices

final class InputInjector {
    private let source = CGEventSource(stateID: .hidSystemState)
    private let bounds: CGRect
    private let dryRun: Bool
    private var cursor: CGPoint
    private var buttonsDown = Set<Int>()
    private var flags: CGEventFlags = []
    private var moveCount = 0

    static let modifierCodes: Set<String> = [
        "ShiftLeft", "ShiftRight", "ControlLeft", "ControlRight",
        "AltLeft", "AltRight", "MetaLeft", "MetaRight", "CapsLock",
    ]

    init(dryRun: Bool) {
        self.dryRun = dryRun
        bounds = CGDisplayBounds(CGMainDisplayID())
        cursor = CGPoint(x: bounds.midX, y: bounds.midY)
        logErr("input: injector ready\(dryRun ? " (DRY RUN — nothing is posted)" : ""); main display \(Int(bounds.width))x\(Int(bounds.height)) pt")

        if dryRun { return }
        // CGEvent injection needs Accessibility and, unlike Screen Recording, does not
        // prompt on first use. Trigger the prompt so granting is one click.
        let opts = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        if AXIsProcessTrustedWithOptions(opts) {
            logErr("input: Accessibility granted; injection active")
        } else {
            logErr("input: Accessibility NOT granted — allow this binary in System Settings > Privacy & Security > Accessibility, then rerun the host")
        }
    }

    func handle(_ line: String) {
        let p = line.split(separator: " ")
        guard let cmd = p.first else { return }
        switch cmd {
        case "m":
            guard p.count >= 4, let nx = Double(p[1]), let ny = Double(p[2]), let f = Int(p[3]) else { return }
            setFlags(decodeFlags(f))
            cursor = CGPoint(x: bounds.minX + CGFloat(clamp01(nx)) * bounds.width,
                             y: bounds.minY + CGFloat(clamp01(ny)) * bounds.height)
            let held = buttonsDown.first
            postMouse(moveType(for: held), button: held ?? 0)
            moveCount += 1
            if moveCount % 120 == 0 { logErr("input: \(moveCount) moves; cursor \(Int(cursor.x)),\(Int(cursor.y)) flags=\(desc(flags))") }
        case "r":
            guard p.count >= 4, let dx = Double(p[1]), let dy = Double(p[2]), let f = Int(p[3]) else { return }
            setFlags(decodeFlags(f))
            // Clamp the tracked position to the display, but ALWAYS carry the raw
            // deltas on the event — a camera keeps turning even with the cursor pinned
            // at a screen edge (that is the whole point of relative mode).
            cursor = CGPoint(x: min(max(cursor.x + CGFloat(dx), bounds.minX), bounds.maxX - 1),
                             y: min(max(cursor.y + CGFloat(dy), bounds.minY), bounds.maxY - 1))
            let held = buttonsDown.first
            postMouse(moveType(for: held), button: held ?? 0, dx: dx, dy: dy)
            moveCount += 1
            if moveCount % 120 == 0 { logErr("input: \(moveCount) moves (rel); cursor \(Int(cursor.x)),\(Int(cursor.y)) flags=\(desc(flags))") }
        case "d":
            guard p.count >= 5, let b = Int(p[1]), let nx = Double(p[2]), let ny = Double(p[3]), let f = Int(p[4]) else { return }
            setFlags(decodeFlags(f))
            cursor = CGPoint(x: bounds.minX + CGFloat(clamp01(nx)) * bounds.width,
                             y: bounds.minY + CGFloat(clamp01(ny)) * bounds.height)
            buttonsDown.insert(b)
            logErr("input: mouseDown b=\(b) at \(Int(cursor.x)),\(Int(cursor.y)) flags=\(desc(flags))")
            postMouse(downType(b), button: b)
        case "D":
            guard p.count >= 3, let b = Int(p[1]), let f = Int(p[2]) else { return }
            setFlags(decodeFlags(f))
            buttonsDown.insert(b)
            logErr("input: mouseDown b=\(b) (rel, cursor \(Int(cursor.x)),\(Int(cursor.y))) flags=\(desc(flags))")
            postMouse(downType(b), button: b)
        case "u":
            guard p.count >= 3, let b = Int(p[1]), let f = Int(p[2]) else { return }
            setFlags(decodeFlags(f))
            buttonsDown.remove(b)
            logErr("input: mouseUp b=\(b) flags=\(desc(flags))")
            postMouse(upType(b), button: b)
        case "w":
            guard p.count >= 4, let dx = Double(p[1]), let dy = Double(p[2]), let f = Int(p[3]) else { return }
            setFlags(decodeFlags(f))
            if dryRun { logErr("input: DRY scroll dx=\(dx) dy=\(dy) flags=\(desc(flags))"); return }
            let e = CGEvent(scrollWheelEvent2Source: source, units: .pixel, wheelCount: 2,
                            wheel1: Int32(dy), wheel2: Int32(dx), wheel3: 0)
            e?.flags = flags
            e?.post(tap: .cghidEventTap)
        case "k", "K":
            let down = (cmd == "k")
            guard p.count >= 3, let f = Int(p[2]) else { return }
            setFlags(decodeFlags(f))
            let code = String(p[1])
            // Modifiers are conveyed purely via flags on the events they apply to; do
            // not inject them as standalone key presses (that is what leaves them stuck).
            if Self.modifierCodes.contains(code) { return }
            guard let vk = Self.keyMap[code] else { logErr("input: unmapped key \(code)"); return }
            logErr("input: key\(down ? "Down" : "Up") \(code) flags=\(desc(flags))")
            if dryRun { return }
            let e = CGEvent(keyboardEventSource: source, virtualKey: vk, keyDown: down)
            e?.flags = flags
            e?.post(tap: .cghidEventTap)
        case "x":
            logErr("input: release-all (\(buttonsDown.count) buttons, flags \(desc(flags)))")
            for b in buttonsDown { postMouse(upType(b), button: b) }
            buttonsDown.removeAll()
            setFlags([]) // posts key-ups for any held modifiers, then clears state
        default:
            break
        }
    }

    private func moveType(for held: Int?) -> CGEventType {
        switch held {
        case 2: return .rightMouseDragged
        case 1: return .otherMouseDragged
        case 0: return .leftMouseDragged
        default: return .mouseMoved
        }
    }
    private func downType(_ b: Int) -> CGEventType { b == 2 ? .rightMouseDown : (b == 1 ? .otherMouseDown : .leftMouseDown) }
    private func upType(_ b: Int) -> CGEventType { b == 2 ? .rightMouseUp : (b == 1 ? .otherMouseUp : .leftMouseUp) }

    private func postMouse(_ type: CGEventType, button: Int, dx: Double = 0, dy: Double = 0) {
        if dryRun { return }
        let cgButton: CGMouseButton = button == 2 ? .right : (button == 1 ? .center : .left)
        let e = CGEvent(mouseEventSource: source, mouseType: type, mouseCursorPosition: cursor, mouseButton: cgButton)
        e?.flags = flags
        if dx != 0 || dy != 0 {
            // Games that capture the mouse (pointer-lock style) read per-event deltas,
            // not the cursor position — carry them explicitly on relative moves.
            e?.setIntegerValueField(.mouseEventDeltaX, value: Int64(dx.rounded()))
            e?.setIntegerValueField(.mouseEventDeltaY, value: Int64(dy.rounded()))
        }
        e?.post(tap: .cghidEventTap)
    }

    // Modifier transitions are injected as REAL key events so apps that poll key state
    // (games: shift-lock, sprint) see them — while the state itself stays absolute per
    // message, so a dropped browser keyup still self-heals on the next event.
    private static let modifierKeys: [(CGEventFlags, CGKeyCode)] = [
        (.maskShift, 56), (.maskControl, 59), (.maskAlternate, 58), (.maskCommand, 55),
    ]
    private func setFlags(_ new: CGEventFlags) {
        guard new != flags else { return }
        if !dryRun {
            var cur = flags
            for (mask, vk) in Self.modifierKeys {
                let want = new.contains(mask)
                guard cur.contains(mask) != want else { continue }
                if want { cur.insert(mask) } else { cur.remove(mask) }
                let e = CGEvent(keyboardEventSource: source, virtualKey: vk, keyDown: want)
                e?.flags = cur
                e?.post(tap: .cghidEventTap)
            }
        }
        flags = new
    }

    private func decodeFlags(_ f: Int) -> CGEventFlags {
        var fl: CGEventFlags = []
        if f & 1 != 0 { fl.insert(.maskShift) }
        if f & 2 != 0 { fl.insert(.maskControl) }
        if f & 4 != 0 { fl.insert(.maskAlternate) }
        if f & 8 != 0 { fl.insert(.maskCommand) }
        return fl
    }

    private func desc(_ f: CGEventFlags) -> String {
        var s: [String] = []
        if f.contains(.maskShift) { s.append("shift") }
        if f.contains(.maskControl) { s.append("ctrl") }
        if f.contains(.maskAlternate) { s.append("alt") }
        if f.contains(.maskCommand) { s.append("cmd") }
        return s.isEmpty ? "none" : s.joined(separator: "+")
    }

    private func clamp01(_ v: Double) -> Double { max(0, min(1, v)) }

    // Browser KeyboardEvent.code -> macOS virtual keycode (Carbon HIToolbox values).
    static let keyMap: [String: CGKeyCode] = [
        "KeyA": 0, "KeyS": 1, "KeyD": 2, "KeyF": 3, "KeyH": 4, "KeyG": 5, "KeyZ": 6,
        "KeyX": 7, "KeyC": 8, "KeyV": 9, "KeyB": 11, "KeyQ": 12, "KeyW": 13, "KeyE": 14,
        "KeyR": 15, "KeyY": 16, "KeyT": 17, "KeyO": 31, "KeyU": 32, "KeyI": 34, "KeyP": 35,
        "KeyL": 37, "KeyJ": 38, "KeyK": 40, "KeyN": 45, "KeyM": 46,
        "Digit1": 18, "Digit2": 19, "Digit3": 20, "Digit4": 21, "Digit6": 22, "Digit5": 23,
        "Digit9": 25, "Digit7": 26, "Digit8": 28, "Digit0": 29,
        "Equal": 24, "Minus": 27, "BracketRight": 30, "BracketLeft": 33, "Quote": 39,
        "Semicolon": 41, "Backslash": 42, "Comma": 43, "Slash": 44, "Period": 47,
        "Backquote": 50,
        "Return": 36, "Enter": 36, "Tab": 48, "Space": 49, "Backspace": 51, "Delete": 51,
        "Escape": 53, "ForwardDelete": 117,
        "ArrowLeft": 123, "ArrowRight": 124, "ArrowDown": 125, "ArrowUp": 126,
        "Home": 115, "End": 119, "PageUp": 116, "PageDown": 121,
        "F1": 122, "F2": 120, "F3": 99, "F4": 118, "F5": 96, "F6": 97, "F7": 98,
        "F8": 100, "F9": 101, "F10": 109, "F11": 103, "F12": 111,
    ]
}

// InputReader pumps host commands from stdin into the injector on a background thread.
// Retain the instance for the process lifetime (see the ARC note in main.swift).
final class InputReader {
    private let injector: InputInjector
    init(injector: InputInjector) { self.injector = injector }

    func start() {
        DispatchQueue.global(qos: .userInteractive).async { [injector] in
            logErr("input: reader started (reading commands on stdin)")
            while let line = readLine(strippingNewline: true) {
                if line.isEmpty { continue }
                injector.handle(line)
            }
            logErr("input: stdin closed; reader stopping")
        }
    }
}
