import AppKit
import Foundation
import LocalAuthentication
import ServiceManagement

@MainActor
final class AppState: ObservableObject {
    @Published var connected = false
    @Published var grants: [GrantInfo] = []
    @Published var status: StatusInfo?
    @Published var recent: [AuditEntry] = []
    @Published var lastError: String?
    @Published var launchAtLogin = SMAppService.mainApp.status == .enabled
    /// Uses that went through without a popup, newest first.
    @Published var autoAllowed: [BrokerEvent] = []
    /// Menu bar iris aperture while it flutters for an auto-allowed use.
    @Published var iconOpenness: CGFloat = 0
    private var flutter: Timer?
    let island = IslandController()

    private let subscription = Subscription()
    private var panels: [String: ApprovalPanelController] = [:]
    private var refreshTimer: Timer?

    init() {
        subscription.onConnection = { [weak self] up in
            self?.connected = up
            if up { self?.refresh() }
        }
        subscription.onEvent = { [weak self] ev in self?.handle(ev) }
        subscription.start()
        // Countdown labels + expiry pruning in the menu.
        refreshTimer = Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }

    }

    var activeGrantCount: Int { grants.filter { $0.expiresAt > Date() }.count }

    // MARK: events

    private func handle(_ ev: BrokerEvent) {
        switch ev.type {
        case "approval_request":
            guard let id = ev.id, panels[id] == nil else { return }
            let c = ApprovalPanelController(event: ev, offset: panels.count) { [weak self] choice in
                self?.decide(ev, choice: choice)
            }
            panels[id] = c
            c.show()
            // Like 1Password: Touch ID is armed right away, touch = allow once.
            if ev.touchID == true, let first = ev.options?.first {
                let who = ev.profile.map { "Claude (\($0))" } ?? "Claude"
                c.armTouchID(reason: "allow \(who) to use \(ev.label ?? "a credential") once") { [weak self] in
                    self?.decide(ev, choice: first.name, authenticated: true)
                }
            }
        case "cancel":
            if let id = ev.id, let p = panels.removeValue(forKey: id) { p.close() }
        case "grants_changed":
            refresh()
        case "auto_allowed":
            autoAllowed.insert(ev, at: 0)
            if autoAllowed.count > 100 { autoAllowed.removeLast(autoAllowed.count - 100) }
            flutterIcon()
            island.show(autoAllowed)
        default:
            break
        }
    }

    private func decide(_ ev: BrokerEvent, choice: String, authenticated: Bool = false) {
        guard let id = ev.id else { return }
        let panel = panels[id]
        if !authenticated { panel?.disarmTouchID() }
        let send: @MainActor (String) -> Void = { [weak self] c in
            self?.subscription.send(["op": "decision", "id": id, "choice": c])
            self?.panels.removeValue(forKey: id)?.close()
        }
        // The iris opens only once access is actually granted (after Touch ID
        // when required); the decision is sent when the animation finishes.
        let grant: @MainActor () -> Void = {
            if let panel { panel.playOpen { send(choice) } } else { send(choice) }
        }
        let deny: @MainActor () -> Void = {
            if let panel { panel.playDeny { send("deny") } } else { send("deny") }
        }

        if choice == "deny" { return deny() }
        guard ev.touchID == true, !authenticated else { return grant() }

        let ctx = LAContext()
        var err: NSError?
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else {
            return deny() // no biometrics/password available: fail closed
        }
        let who = ev.profile.map { "Claude (\($0))" } ?? "Claude"
        let reason = "allow \(who) to use \(ev.label ?? "a credential")"
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason) { ok, _ in
            Task { @MainActor in
                if ok { grant() } else { deny() }
            }
        }
    }

    // MARK: auto-allowed ticker

    /// "gitlab.com · GET …/merge_requests/510", "staging · kubectl get pods".
    static func describe(_ e: BrokerEvent) -> String {
        let target = e.target ?? e.tool ?? ""
        var what = e.detail ?? ""
        if e.tool == "http", let sp = what.firstIndex(of: " ") {
            let method = what[..<sp]
            var path = String(what[what.index(after: sp)...])
            if let r = path.range(of: "://"), let slash = path[r.upperBound...].firstIndex(of: "/") {
                path = String(path[slash...])
            }
            let parts = path.split(separator: "/")
            path = parts.count > 2 ? "…/" + parts.suffix(2).joined(separator: "/") : path
            what = "\(method) \(path)"
        } else {
            what = what.replacingOccurrences(of: "(latest Bash command) ", with: "")
            what = what.split(whereSeparator: { $0 == "\n" }).first.map(String.init) ?? what
        }
        return what.isEmpty ? target : "\(target) · \(what)"
    }

    /// The menu bar iris opens a little and closes again.
    private func flutterIcon() {
        flutter?.invalidate()
        let start = Date()
        let duration = 0.9
        flutter = Timer.scheduledTimer(withTimeInterval: 1.0 / 30, repeats: true) { [weak self] t in
            Task { @MainActor in
                guard let self else { return t.invalidate() }
                let x = Date().timeIntervalSince(start) / duration
                if x >= 1 {
                    self.iconOpenness = 0
                    t.invalidate()
                    return
                }
                self.iconOpenness = CGFloat(sin(x * .pi)) * 0.6 // open and close once
            }
        }
    }

    // MARK: control actions

    func refresh() {
        Task.detached {
            do {
                let g = try Control.call(["op": "grants"])
                let s = try Control.call(["op": "status"])
                let a = try Control.call(["op": "audit", "n": 8])
                let auto = (try? Control.call(["op": "auto"]))?.auto
                await MainActor.run {
                    self.grants = g.grants ?? []
                    self.status = s.status
                    self.recent = (a.audit ?? []).reversed()
                    if let auto { self.autoAllowed = auto.reversed() }
                    self.lastError = nil
                    self.restartStaleBroker()
                }
            } catch {
                await MainActor.run { self.lastError = error.localizedDescription }
            }
        }
    }

    private var triedBrokerRestart = false

    static var appVersion: String {
        Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "dev"
    }

    /// After an update the launchd broker is still the old process: restart it
    /// once per launch. The launch agent runs the bundled iris, which Sparkle
    /// just replaced, so the restart picks up the new version. If it still
    /// differs, the menu shows the mismatch notice with "Update broker".
    private func restartStaleBroker() {
        guard let v = status?.version, v != Self.appVersion, !triedBrokerRestart else { return }
        triedBrokerRestart = true
        let bin = IrisPaths.bundledBin + "/iris"
        guard FileManager.default.isExecutableFile(atPath: bin) else { return }
        Task.detached {
            _ = Self.run(bin, ["restart"])
            try? await Task.sleep(nanoseconds: 1_500_000_000)
            await MainActor.run { self.refresh() }
        }
    }

    func revoke(_ id: String?) {
        Task.detached {
            var req: [String: Any] = ["op": "revoke"]
            if let id { req["id"] = id } else { req["all"] = true }
            _ = try? Control.call(req)
            await MainActor.run { self.refresh() }
        }
    }

    func setLaunchAtLogin(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
        } catch {
            lastError = "Launch at login: \(error.localizedDescription)"
        }
        launchAtLogin = SMAppService.mainApp.status == .enabled
    }

    // MARK: CLI integration (bundled binaries)

    /// Runs the bundled `iris` CLI and shows its output in a window.
    func runCLI(_ args: [String], title: String) {
        let bin = IrisPaths.bundledBin + "/iris"
        guard FileManager.default.isExecutableFile(atPath: bin) else {
            lastError = "iris CLI not found in app bundle (\(bin))"
            return
        }
        Task.detached {
            let output = Self.run(bin, args)
            await MainActor.run {
                OutputWindow.show(title: title, text: output)
                self.refresh()
            }
        }
    }

    nonisolated private static func run(_ bin: String, _ args: [String]) -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: bin)
        p.arguments = args
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        do {
            try p.run()
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            p.waitUntilExit()
            return String(decoding: data, as: UTF8.self) + "\n(exit \(p.terminationStatus))"
        } catch {
            return "failed to run iris: \(error)"
        }
    }

    func openConfig() {
        let url = URL(fileURLWithPath: IrisPaths.configFile)
        if !FileManager.default.fileExists(atPath: url.path) {
            runCLI(["install", "--dry-run"], title: "Create config")
        }
        NSWorkspace.shared.open(url)
    }

    func openAuditLog() {
        NSWorkspace.shared.open(URL(fileURLWithPath: IrisPaths.auditLog))
    }
}
