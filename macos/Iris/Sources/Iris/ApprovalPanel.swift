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
    /// May sit over the menu bar while it grows out of the notch.
    override func constrainFrameRect(_ frameRect: NSRect, to screen: NSScreen?) -> NSRect { frameRect }
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

    private let morph = MorphModel()
    private var geo: IslandGeometry?

    /// The popup grows out of the island (the notch, or the top edge), then
    /// settles at the centre of the screen.
    func show() {
        let view = ApprovalView(event: event, shutter: shutter) { [weak self] choice in self?.choose(choice) }
            .environment(\.colorScheme, .dark)
        let size = NSHostingView(rootView: view).fittingSize
        let g = IslandGeometry.current()
        geo = g
        let final = NSRect(origin: centeredOrigin(for: size), size: size)
        let start = g.tucked

        let host = NSHostingView(rootView: MorphHost(morph: morph, size: size) { view })
        host.sizingOptions = [] // the animated frame drives the size
        let p = KeyablePanel(contentRect: start,
                             styleMask: [.borderless, .nonactivatingPanel],
                             backing: .buffered, defer: false)
        p.contentView = host
        p.isOpaque = false
        p.backgroundColor = .clear
        p.hasShadow = false // until it has landed
        p.level = islandLevel // over the menu bar while it leaves the notch
        p.isFloatingPanel = true
        p.hidesOnDeactivate = false
        p.isMovableByWindowBackground = true
        p.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .transient]
        p.delegate = self
        p.setFrame(start, display: false)
        panel = p

        p.alphaValue = 1
        NSApp.activate(ignoringOtherApps: true)
        p.makeKeyAndOrderFront(nil)
        NSAnimationContext.runAnimationGroup({ ctx in
            ctx.duration = 0.45
            ctx.timingFunction = islandTiming
            p.animator().setFrame(final, display: true)
        }, completionHandler: {
            Task { @MainActor in
                guard self.panel === p else { return }
                p.level = .modalPanel
                p.hasShadow = true
                p.invalidateShadow()
            }
        })
        withAnimation(.easeOut(duration: 0.3).delay(0.14)) { morph.open = true }
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
            try? await Task.sleep(nanoseconds: 550_000_000) // after the popup has landed
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
        // Back into the island.
        let g = geo ?? IslandGeometry.current()
        p.hasShadow = false
        p.level = islandLevel
        withAnimation(.easeIn(duration: 0.16)) { morph.open = false }
        NSAnimationContext.runAnimationGroup({ ctx in
            ctx.duration = 0.34
            ctx.timingFunction = CAMediaTimingFunction(name: .easeIn)
            p.animator().setFrame(g.tucked, display: true)
            if !g.hasNotch { p.animator().alphaValue = 0 }
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
    /// For a read: the time buttons allow reads only (on by default).
    @State private var readsOnly = true
    private let tick = Timer.publish(every: 1, on: .main, in: .common).autoconnect()

    private var isRead: Bool { event.access == "read" }

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
        .frame(width: 640)
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
                if let n = event.recent, n >= 2 {
                    Text("You allowed this \(n)× in the last 10 min. A time grant covers repeats.")
                        .font(Hy.ui(11.5))
                        .foregroundStyle(Hy.classG)
                }
                HStack(spacing: Hy.s2) {
                    if let a = event.access {
                        HyTag(text: a == "read" ? "Read" : "Write", kind: a == "read" ? .a : .m)
                            .help(event.accessWhy ?? "")
                    }
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
                row("Command") { CommandBox(command: cmd, spans: event.commandSpans) }
            }
            if let why = event.accessWhy, !why.isEmpty {
                row(isRead ? "Read" : "Write") {
                    Text(why)
                        .font(Hy.ui(12))
                        .foregroundStyle(isRead ? Hy.classA : Hy.dust)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            if isRead {
                row("Grant") {
                    Toggle(readsOnly ? "Time grants allow only reads on \(event.target ?? "this target"); writes keep asking"
                                     : "Time grants allow everything on \(event.target ?? "this target"), writes included",
                           isOn: $readsOnly)
                        .toggleStyle(HyToggle(onLabel: "Reads only", offLabel: "Everything"))
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

    /// Titles stay short (the footer is full); the Grant row says whether
    /// they cover reads only.
    private func timedTitle(_ o: OptionInfo) -> String { o.display }

    private func timedChoice(_ o: OptionInfo) -> String {
        guard isRead, readsOnly, o.name != "always" else { return o.name }
        return "reads:\(o.name)"
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
                Button(timedTitle(opt)) { onChoice(timedChoice(opt)) }
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

// MARK: - Command box

/// The command, highlighted and broken into lines by the broker
/// (internal/shellfmt). Grows up to `maxLines`, then scrolls.
struct CommandBox: View {
    let command: String
    let spans: [CommandSpan]?

    static let maxLines = 12
    static let fontSize: CGFloat = 12.5
    static let lineHeight: CGFloat = 18
    static let charsPerLine = 58

    /// "(latest Bash command)": iris matched the request to Claude's most
    /// recent command itself, so it says so under the box.
    private var isGuess: Bool { spans?.first?.kind == "note" }

    private var bodySpans: [CommandSpan] {
        guard var s = spans, !s.isEmpty else { return [CommandSpan(text: command, kind: "arg")] }
        if s.first?.kind == "note" {
            s.removeFirst()
            if s.first?.kind == "break" { s.removeFirst() }
        }
        return s
    }

    private var plain: String { bodySpans.map(\.text).joined() }

    private var estimatedLines: Int {
        plain.split(separator: "\n", omittingEmptySubsequences: false)
            .reduce(0) { $0 + max(1, ($1.count + Self.charsPerLine - 1) / Self.charsPerLine) }
    }

    private var attributed: AttributedString {
        var out = AttributedString()
        for s in bodySpans {
            var a = AttributedString(s.text)
            let (color, weight) = Self.style(s.kind)
            a.foregroundColor = color
            a.font = Hy.mono(Self.fontSize, weight)
            out += a
        }
        return out
    }

    static func style(_ kind: String) -> (Color, CGFloat) {
        switch kind {
        case "cmd": return (Hy.starlight, 650)
        case "flag": return (Hy.classA, 400)
        case "string": return (Hy.classG, 400)
        case "var": return (Hy.classK, 400)
        case "keyword": return (Hy.classM, 600)
        case "op": return (Hy.neatlineStrong, 700)
        case "redir", "comment": return (Hy.faint, 400)
        default: return (Hy.dust, 400)
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Hy.s1) {
            box
            if isGuess {
                Text("Iris's guess: Claude's most recent command (Claude didn't report one for this request).")
                    .font(Hy.ui(11))
                    .foregroundStyle(Hy.faint)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private var text: some View {
        Text(attributed)
            .lineSpacing(3)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, Hy.s3)
            .padding(.vertical, Hy.s2)
    }

    @ViewBuilder private var box: some View {
        Group {
            if estimatedLines > Self.maxLines {
                ScrollView(.vertical, showsIndicators: true) { text }
                    .frame(height: CGFloat(Self.maxLines) * Self.lineHeight + 2 * Hy.s2)
            } else {
                text
            }
        }
        .background(RoundedRectangle(cornerRadius: Hy.r2).fill(Hy.sky))
        .overlay(RoundedRectangle(cornerRadius: Hy.r2).strokeBorder(Hy.neatline, lineWidth: 1))
    }
}
