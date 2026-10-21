// Keeps the Mac's display awake while this helper is capturing.
//
// macOS sleeps the display after `pmset -g displaysleep` minutes of no LOCAL input
// (5 on this machine). A remote viewer who is only WATCHING — a movie, a long page —
// injects nothing, so macOS calls the Mac idle, blanks the display, and
// ScreenCaptureKit stops producing frames: the browser freezes on the last frame
// while the WebRTC connection stays perfectly healthy. Actively driving a session
// hides this, because injected CGEvents go to the HID tap (see InputInjector) and do
// reset the idle timer — which is exactly why the freeze looked like it only struck
// idle sessions.
//
// Scope is this process, which the host spawns per session and kills when the session
// ends, so nothing pins the display on once nobody is connected. macOS also drops an
// assertion automatically when its owning process exits, so a crash here cannot leave
// the Mac awake forever.
//
// Inspect a live one with: pmset -g assertions
import Foundation
import IOKit.pwr_mgt

/// Actively wakes a sleeping display by declaring local user activity — what a real
/// keypress does. The assertion below only stops an AWAKE display from sleeping; it
/// cannot light a panel that is already dark, which is how a remote session used to
/// come up as a black screen (SCShareableContent reports zero displays while the
/// display sleeps). Best-effort, like the assertion: failure just means capture will
/// report noDisplay and the session ends with an error instead of video.
func wakeDisplay(reason: String) {
    var id = IOPMAssertionID(0)
    let status = IOPMAssertionDeclareUserActivity(reason as CFString, kIOPMUserActiveLocal, &id)
    if status == kIOReturnSuccess {
        logErr("display: declared user activity to wake a sleeping display")
    } else {
        logErr("display: wake declaration failed (status \(status)); capture will fail if the display is asleep")
    }
}

final class DisplaySleepAssertion {
    private var id = IOPMAssertionID(0)
    private var held = false

    /// Takes the assertion. Failure is deliberately non-fatal: capture still works,
    /// the display just goes back to sleeping on the system's own schedule.
    init(reason: String) {
        var assertionID = IOPMAssertionID(0)
        let status = IOPMAssertionCreateWithName(
            kIOPMAssertionTypePreventUserIdleDisplaySleep as CFString,
            IOPMAssertionLevel(kIOPMAssertionLevelOn),
            reason as CFString,
            &assertionID)
        if status == kIOReturnSuccess {
            id = assertionID
            held = true
            logErr("display: holding off idle display sleep for this session")
        } else {
            logErr("display: sleep assertion failed (status \(status)); the screen may blank mid-session")
        }
    }

    func release() {
        guard held else { return }
        IOPMAssertionRelease(id)
        held = false
        logErr("display: released the sleep assertion")
    }

    deinit { release() }
}
