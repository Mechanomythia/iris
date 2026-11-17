import AppKit
import LocalAuthentication
import LocalAuthenticationEmbeddedUI
import SwiftUI

/// Touch ID drawn inside the panel (like 1Password): evaluating a policy on
/// this view's context shows the sensor prompt here, not as a system sheet.
struct TouchIDView: NSViewRepresentable {
    let context: LAContext
    func makeNSView(context _: Context) -> LAAuthenticationView {
        LAAuthenticationView(context: context, controlSize: .regular)
    }
    func updateNSView(_: LAAuthenticationView, context _: Context) {}
}

/// Borderless panels can't become key by default; we need key for Return/Escape.
final class KeyablePanel: NSPanel {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { false }
}

/// One floating, centred plate per approval request. Return = the shortest
/// option, Escape = Deny, no answer = Deny (the broker times out).
@MainActor
final class ApprovalPanelController: NSObject, NSWindowDelegate {
    private let event: BrokerEvent
    private let onChoice: (String) -> Void
    private var panel: NSPanel?
    private var decided = false
    private let offset: Int
    let shutter = ShutterModel()

    init(event: BrokerEvent, offset: Int, onChoice: @escaping (String) -> Void) {
        self.event = event
        self.offset = offset
        self.onChoice = onChoice
    }

    func show() {
        let view = ApprovalView(event: event, shutter: shutter) { [weak self] choice in self?.choose(choice) }
            .environment(\.colorScheme, .dark)
        let host = NSHostingView(rootView: view)
        let size = host.fittingSize

        let p = KeyablePanel(contentRect: NSRect(origin: .zero, size: size),
                             styleMask: [.borderless, .nonactivatingPanel],
                             backing: .buffered, defer: false)
        p.contentView = host
        p.isOpaque = false
        p.backgroundColor = .clear
        p.hasShadow = true
        p.level = .modalPanel
        p.isFloatingPanel = true
        p.hidesOnDeactivate = false
        p.isMovableByWindowBackground = true
        p.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .transient]
        p.delegate = self
        p.setFrameOrigin(centeredOrigin(for: size))
        panel = p

        p.alphaValue = 0
        NSApp.activate(ignoringOtherApps: true)
        p.makeKeyAndOrderFront(nil)
        NSAnimationContext.runAnimationGroup { ctx in
            ctx.duration = 0.12
            p.animator().alphaValue = 1
        }
        NSSound(named: "Tink")?.play()
    }

    /// Exact centre of the screen the pointer is on (visible area), stacked
    /// slightly when several requests are open.
    private func centeredOrigin(for size: NSSize) -> NSPoint {
        let mouse = NSEvent.mouseLocation
        let screen = NSScreen.screens.first { NSMouseInRect(mouse, $0.frame, false) } ?? NSScreen.main
        let vf = screen?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
        let step = CGFloat(offset) * 28
        return NSPoint(x: vf.midX - size.width / 2 + step, y: vf.midY - size.height / 2 - step)
    }

    private func choose(_ choice: String) {
        guard !decided else { return }
        decided = true
        onChoice(choice)
    }

    /// Iris opens (access granted), then `done` sends the decision.
    func playOpen(then done: @escaping @MainActor () -> Void) { shutter.open(then: done) }

    /// Iris stays shut.
    func playDeny(then done: @escaping @MainActor () -> Void) {
        guard panel != nil else { return done() }
        shutter.deny(then: done)
    }

    /// Arms the embedded Touch ID: a successful touch calls `onTouch`
    /// (allow once). Returns false when there's no biometry (closed lid, no
    /// sensor); the buttons then work as before.
    @discardableResult
    func armTouchID(reason: String, onTouch: @escaping @MainActor () -> Void) -> Bool {
        let ctx = LAContext()
        var err: NSError?
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: &err) else { return false }
        shutter.touchContext = ctx
        // The view must be on screen with this context before evaluating.
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: 250_000_000)
            guard self.shutter.touchContext === ctx, !self.decided else { return }
            ctx.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: reason) { ok, _ in
                Task { @MainActor in
                    guard self.shutter.touchContext === ctx else { return } // disarmed meanwhile
                    self.shutter.touchContext = nil
                    if ok { onTouch() } // failure/cancel: fall back to the buttons
                }
            }
        }
        return true
    }

    /// Cancels an armed Touch ID (another option was chosen, or Deny).
    func disarmTouchID() {
        guard let ctx = shutter.touchContext else { return }
        shutter.touchContext = nil
        ctx.invalidate()
    }

    func close() {
        disarmTouchID()
        decided = true
        guard let p = panel else { return }
        panel = nil
        p.delegate = nil
        NSAnimationContext.runAnimationGroup({ ctx in
            ctx.duration = 0.12
            p.animator().alphaValue = 0
        }, completionHandler: { p.close() })
    }

    nonisolated func windowWillClose(_ notification: Notification) {
        Task { @MainActor in self.choose("deny") }
    }
}

// MARK: - View

struct ApprovalView: View {
    let event: BrokerEvent
    @ObservedObject var shutter: ShutterModel
    let onChoice: (String) -> Void
    @State private var now = Date()
    private let tick = Timer.publish(every: 1, on: .main, in: .common).autoconnect()

    private var options: [OptionInfo] { event.options ?? [] }
    private var remaining: Int {
        guard let d = event.deadline else { return 0 }
        return max(0, Int(d.timeIntervalSince(now)))
    }
    private var touchID: Bool { event.touchID == true }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            Neatline()
            legend
            Neatline()
            footer
        }
        .frame(width: 580)
        .background(Hy.skyRaised)
        .clipShape(RoundedRectangle(cornerRadius: Hy.r2))
        .overlay(RoundedRectangle(cornerRadius: Hy.r2).strokeBorder(Hy.neatline, lineWidth: 1))
        .onReceive(tick) { now = $0 }
    }

    // MARK: header

    private var header: some View {
        HStack(alignment: .center, spacing: Hy.s5) {
            IrisShutter(openness: shutter.openness, granted: shutter.granted, denied: shutter.denied)
                .frame(width: 124, height: 124)
                .modifier(Shake(animatableData: shutter.shake))
            VStack(alignment: .leading, spacing: Hy.s2) {
                HyLabel(event.profile.map { "Credential request · \($0)" } ?? "Credential request")
                Text(toolTitle)
                    .font(Hy.display(28, 350))
                    .foregroundStyle(Hy.starlight)
                Text(event.target ?? "")
                    .font(Hy.mono(13))
                    .foregroundStyle(Hy.dust)
                    .lineLimit(2)
                    .truncationMode(.middle)
                    .textSelection(.enabled)
                HStack(spacing: Hy.s2) {
                    if event.passthrough == true {
                        // Class K: your own identity is in play, read carefully.
                        HyTag(text: "Your credentials", kind: .k)
                    } else if let scope = event.scope, !scope.isEmpty {
                        HyTag(text: scope)
                    }
                    status
                }
                .padding(.top, 2)
            }
            Spacer(minLength: 0)
        }
        .padding(Hy.s5)
    }

    @ViewBuilder private var status: some View {
        if shutter.granted {
            HStack(spacing: Hy.s1) { StarGlyph(state: .live, size: 12); Text("Allowed").font(Hy.ui(12)).foregroundStyle(Hy.classA) }
        } else if shutter.denied {
            HStack(spacing: Hy.s1) { StarGlyph(state: .failed, size: 12); Text("Denied").font(Hy.ui(12)).foregroundStyle(Hy.classM) }
        } else {
            HStack(spacing: Hy.s1) { StarGlyph(state: .building, size: 12); Text("Waiting for you").font(Hy.ui(12)).foregroundStyle(Hy.dust) }
        }
    }

    private var toolTitle: String {
        switch event.tool {
        case "kube": return "Kubernetes"
        case "aws": return "AWS"
        case "gcp": return "Google Cloud"
        case "git": return "Git"
        case "docker": return "Docker registry"
        case "http": return "HTTP API"
        case "network": return "Network access"
        case "ssh":
            if event.target?.hasPrefix("signature:") == true { return "Signature" }
            if event.target?.hasPrefix("login:") == true { return "SSH login" }
            return "SSH key"
        default: return event.tool ?? "Credential"
        }
    }

    // MARK: legend (hyad: a narrow label column left of the content)

    private var legend: some View {
        VStack(alignment: .leading, spacing: Hy.s3) {
            if let cmd = event.command, !cmd.isEmpty {
                row("Command") {
                    HStack(alignment: .top, spacing: Hy.s2) {
                        Text("$").foregroundStyle(Hy.faint)
                        Text(cmd).foregroundStyle(Hy.starlight).textSelection(.enabled)
                    }
                    .font(Hy.mono(12.5))
                    .lineLimit(4)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, Hy.s3)
                    .padding(.vertical, Hy.s2)
                    .background(RoundedRectangle(cornerRadius: Hy.r2).fill(Hy.sky))
                    .overlay(RoundedRectangle(cornerRadius: Hy.r2).strokeBorder(Hy.neatline, lineWidth: 1))
                }
            }
            if let argv = event.argv, !argv.isEmpty {
                row("Process") { mono(argv.joined(separator: " ")) }
            }
            if let cwd = event.cwd, !cwd.isEmpty {
                row("In") { mono((cwd as NSString).abbreviatingWithTildeInPath) }
            }
            if let pid = event.pid, pid > 0 {
                row("PID") { mono(String(pid)) }
            }
            Text("The command comes from Claude and may be inaccurate. Access is limited by the identity, not by the command.")
                .font(Hy.ui(11.5))
                .foregroundStyle(Hy.faint)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.leading, 84)
        }
        .padding(.horizontal, Hy.s5)
        .padding(.vertical, Hy.s4)
    }

    private func row<C: View>(_ label: String, @ViewBuilder _ content: () -> C) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: Hy.s3) {
            HyLabel(label).frame(width: 72, alignment: .leading)
            content()
        }
    }

    private func mono(_ s: String) -> some View {
        Text(s)
            .font(Hy.mono(12))
            .foregroundStyle(Hy.dust)
            .lineLimit(2)
            .truncationMode(.middle)
            .textSelection(.enabled)
    }

    // MARK: footer

    private var footer: some View {
        HStack(spacing: Hy.s2) {
            Button("Deny") { onChoice("deny") }
                .buttonStyle(HyButton(kind: .danger))
                .keyboardShortcut(.cancelAction)
            Text("\(remaining)s")
                .font(Hy.mono(11))
                .monospacedDigit()
                .foregroundStyle(remaining <= 10 ? Hy.classK : Hy.faint)
                .padding(.leading, Hy.s1)
            Spacer()
            // Longer options as secondary buttons, the shortest on the right:
            // with Touch ID armed it's the sensor itself (touch = allow once).
            ForEach(options.dropFirst(), id: \.self) { opt in
                Button(opt.display) { onChoice(opt.name) }
                    .buttonStyle(HyButton(kind: .secondary))
            }
            if let ctx = shutter.touchContext, let first = options.first {
                HStack(spacing: Hy.s2) {
                    TouchIDView(context: ctx)
                        .frame(width: 32, height: 32)
                    Text(first.display == "Once" ? "Touch ID to allow once" : "Touch ID to allow \(first.display)")
                        .font(Hy.ui(13, 600))
                        .foregroundStyle(Hy.starlight)
                }
                .padding(.leading, Hy.s2)
            } else if let first = options.first {
                if touchID {
                    Image(systemName: "touchid")
                        .font(.system(size: 15, weight: .regular))
                        .foregroundStyle(Hy.dust)
                        .help("Allowing asks for Touch ID")
                }
                Button(first.display == "Once" ? "Allow once" : "Allow \(first.display)") { onChoice(first.name) }
                    .buttonStyle(HyButton(kind: .primary))
                    .keyboardShortcut(.defaultAction)
            }
        }
        .disabled(shutter.granted || shutter.denied)
        .padding(.horizontal, Hy.s5)
        .padding(.vertical, Hy.s4)
    }
}

// MARK: - CLI output window

@MainActor
enum OutputWindow {
    private static var windows: [NSWindow] = []

    static func show(title: String, text: String) {
        let view = VStack(alignment: .leading, spacing: 0) {
            HStack {
                HyLabel(title)
                Spacer()
            }
            .padding(.horizontal, Hy.s4)
            .frame(height: 36)
            Neatline()
            ScrollView {
                Text(text)
                    .font(Hy.mono(12))
                    .foregroundStyle(Hy.starlight)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(Hy.s4)
            }
        }
        .frame(minWidth: 660, minHeight: 380)
        .background(Hy.sky)
        .environment(\.colorScheme, .dark)

        let w = NSWindow(contentViewController: NSHostingController(rootView: view))
        w.title = title
        w.titlebarAppearsTransparent = true
        w.backgroundColor = NSColor(Hy.sky)
        w.appearance = NSAppearance(named: .darkAqua)
        w.isReleasedWhenClosed = false
        w.center()
        windows.append(w)
        NSApp.activate(ignoringOtherApps: true)
        w.makeKeyAndOrderFront(nil)
    }
}
