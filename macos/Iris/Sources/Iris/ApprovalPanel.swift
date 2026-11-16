import AppKit
import SwiftUI

/// A floating, non-modal panel per approval request. Return = shortest
/// option (the brief's default), Escape or closing the window = Deny.
@MainActor
final class ApprovalPanelController: NSObject, NSWindowDelegate {
    private let event: BrokerEvent
    private let onChoice: (String) -> Void
    private var panel: NSPanel?
    private var decided = false
    private let offset: Int

    init(event: BrokerEvent, offset: Int, onChoice: @escaping (String) -> Void) {
        self.event = event
        self.offset = offset
        self.onChoice = onChoice
    }

    func show() {
        let view = ApprovalView(event: event) { [weak self] choice in self?.choose(choice) }
        let host = NSHostingController(rootView: view)
        let p = NSPanel(contentViewController: host)
        p.styleMask = [.titled, .closable, .nonactivatingPanel, .hudWindow, .utilityWindow]
        p.title = "Iris · credential request"
        p.level = .floating
        p.isFloatingPanel = true
        p.hidesOnDeactivate = false
        p.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        p.delegate = self
        p.center()
        if offset > 0 {
            var f = p.frame
            f.origin.x += CGFloat(offset * 24)
            f.origin.y -= CGFloat(offset * 24)
            p.setFrame(f, display: false)
        }
        panel = p
        NSApp.activate(ignoringOtherApps: true)
        p.makeKeyAndOrderFront(nil)
        NSSound(named: "Ping")?.play()
    }

    private func choose(_ choice: String) {
        guard !decided else { return }
        decided = true
        onChoice(choice)
    }

    func close() {
        decided = true
        panel?.delegate = nil
        panel?.close()
        panel = nil
    }

    nonisolated func windowWillClose(_ notification: Notification) {
        Task { @MainActor in self.choose("deny") }
    }
}

struct ApprovalView: View {
    let event: BrokerEvent
    let onChoice: (String) -> Void
    @State private var now = Date()
    private let tick = Timer.publish(every: 1, on: .main, in: .common).autoconnect()

    private var remaining: Int {
        guard let d = event.deadline else { return 0 }
        return max(0, Int(d.timeIntervalSince(now)))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 10) {
                Image(systemName: icon)
                    .font(.system(size: 28))
                    .foregroundStyle(.orange)
                VStack(alignment: .leading, spacing: 2) {
                    Text("Claude wants to use a credential")
                        .font(.headline)
                    Text(event.label ?? "\(event.tool ?? "?") · \(event.target ?? "?")")
                        .font(.system(.title3, design: .rounded).weight(.semibold))
                }
            }

            if let cmd = event.command, !cmd.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Command").font(.caption).foregroundStyle(.secondary)
                    ScrollView {
                        Text(cmd)
                            .font(.system(.body, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .frame(maxHeight: 90)
                    .padding(8)
                    .background(RoundedRectangle(cornerRadius: 6).fill(Color.primary.opacity(0.06)))
                }
            }

            VStack(alignment: .leading, spacing: 2) {
                if let argv = event.argv, !argv.isEmpty {
                    row("Process", argv.joined(separator: " "))
                }
                if let pid = event.pid, pid > 0 { row("PID", String(pid)) }
                if let cwd = event.cwd, !cwd.isEmpty { row("In", cwd) }
                if let scope = event.scope, !scope.isEmpty { row("Scope", scope) }
            }
            .font(.caption)

            Text("Hint text above comes from Claude and may be inaccurate. Scope is set by the identity, not the command.")
                .font(.caption2)
                .foregroundStyle(.secondary)

            HStack {
                Button("Deny") { onChoice("deny") }
                    .keyboardShortcut(.cancelAction)
                Spacer()
                Text("\(remaining)s").monospacedDigit().foregroundStyle(.secondary).font(.caption)
                ForEach(Array((event.options ?? []).enumerated()), id: \.element) { i, opt in
                    if i == 0 {
                        Button(opt.display) { onChoice(opt.name) }
                            .keyboardShortcut(.defaultAction)
                    } else {
                        Button(opt.display) { onChoice(opt.name) }
                    }
                }
            }
            if event.touchID == true {
                Label("Touch ID required to allow", systemImage: "touchid")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(18)
        .frame(width: 520)
        .onReceive(tick) { now = $0 }
    }

    private var icon: String {
        switch event.tool {
        case "kube": return "shippingbox"
        case "aws", "gcp": return "cloud"
        case "git": return "arrow.triangle.branch"
        case "docker": return "cube"
        case "http": return "network"
        default: return "key"
        }
    }

    private func row(_ k: String, _ v: String) -> some View {
        HStack(alignment: .top) {
            Text(k).foregroundStyle(.secondary).frame(width: 52, alignment: .leading)
            Text(v).lineLimit(2).truncationMode(.middle).textSelection(.enabled)
        }
    }
}

/// Simple scrollable text window for CLI output.
@MainActor
enum OutputWindow {
    private static var windows: [NSWindow] = []

    static func show(title: String, text: String) {
        let view = ScrollView {
            Text(text)
                .font(.system(.body, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding()
        }
        .frame(minWidth: 640, minHeight: 360)
        let w = NSWindow(contentViewController: NSHostingController(rootView: view))
        w.title = title
        w.isReleasedWhenClosed = false
        w.center()
        windows.append(w)
        NSApp.activate(ignoringOtherApps: true)
        w.makeKeyAndOrderFront(nil)
    }
}
