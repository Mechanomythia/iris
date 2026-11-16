import AppKit
import SwiftUI

@main
struct IrisApp: App {
    @StateObject private var state = AppState()

    init() {
        Hy.registerFonts()
    }

    var body: some Scene {
        MenuBarExtra {
            MenuContent().environmentObject(state)
        } label: {
            Image(nsImage: IrisIcon.image(menuIcon))
        }
        .menuBarExtraStyle(.window)
    }

    /// Closed iris = nothing granted; open iris = Claude currently holds a grant.
    private var menuIcon: IrisIcon.State {
        if !state.connected { return .disconnected }
        return state.activeGrantCount > 0 ? .open : .closed
    }
}

// MARK: - Menu

struct MenuContent: View {
    @EnvironmentObject var state: AppState

    var body: some View {
        VStack(alignment: .leading, spacing: Hy.s4) {
            header
            grantsPlate
            recentPlate
            actions
        }
        .padding(Hy.s4)
        .frame(width: 380)
        .background(Hy.sky)
        .environment(\.colorScheme, .dark)
        .onAppear { state.refresh() }
    }

    // MARK: header

    private var header: some View {
        HStack(alignment: .center, spacing: Hy.s3) {
            IrisShutter(openness: state.activeGrantCount > 0 ? 0.55 : 0, granted: state.activeGrantCount > 0, blades: 9)
                .frame(width: 34, height: 34)
            VStack(alignment: .leading, spacing: 2) {
                Text("iris")
                    .font(Hy.display(22, 400))
                    .foregroundStyle(Hy.starlight)
                HStack(spacing: Hy.s1) {
                    StarGlyph(state: state.connected ? .live : .failed, size: 12)
                    Text(state.connected ? "Broker running" : "Broker not reachable")
                        .font(Hy.ui(12.5))
                        .foregroundStyle(state.connected ? Hy.dust : Hy.classM)
                }
            }
            Spacer()
            if let v = state.status?.version {
                Text("v\(v)").font(Hy.mono(11)).foregroundStyle(Hy.faint)
            }
        }
    }

    // MARK: grants

    private var grantsPlate: some View {
        Plate(title: "Active grants",
              caption: state.grants.isEmpty ? nil : "\(state.grants.count)",
              trailing: state.grants.isEmpty ? nil : AnyView(
                  Button("Revoke all") { state.revoke(nil) }
                      .buttonStyle(HyButton(kind: .danger, small: true))
              )) {
            if state.grants.isEmpty {
                HStack(spacing: Hy.s2) {
                    StarGlyph(state: .stopped, size: 12)
                    Text("None. Claude asks before using any credential.")
                        .font(Hy.ui(12.5))
                        .foregroundStyle(Hy.dust)
                }
                .padding(Hy.s4)
            } else {
                VStack(spacing: 0) {
                    ForEach(Array(state.grants.enumerated()), id: \.element.id) { i, g in
                        if i > 0 { Neatline() }
                        GrantRow(grant: g) { state.revoke(g.id) }
                    }
                }
            }
        }
    }

    // MARK: recent

    private var recentPlate: some View {
        Plate(title: "Recent", caption: state.recent.isEmpty ? nil : "audit log") {
            if state.recent.isEmpty {
                Text("No activity yet.")
                    .font(Hy.ui(12.5))
                    .foregroundStyle(Hy.dust)
                    .padding(Hy.s4)
            } else {
                VStack(alignment: .leading, spacing: 0) {
                    ForEach(Array(state.recent.prefix(6).enumerated()), id: \.offset) { i, e in
                        if i > 0 { Neatline() }
                        AuditRow(entry: e)
                    }
                }
            }
        }
    }

    // MARK: actions

    private var actions: some View {
        VStack(alignment: .leading, spacing: Hy.s3) {
            if let err = state.lastError, state.connected {
                HStack(alignment: .top, spacing: Hy.s2) {
                    StarGlyph(state: .failed, size: 12).padding(.top, 3)
                    Text(err).font(Hy.ui(12)).foregroundStyle(Hy.dust)
                }
            }
            HStack(spacing: Hy.s2) {
                Button("Set up Claude Code") { state.runCLI(["install", "--detect"], title: "iris install") }
                    .buttonStyle(HyButton(kind: .primary, small: true))
                Button("Doctor") { state.runCLI(["doctor"], title: "iris doctor") }
                    .buttonStyle(HyButton(kind: .secondary, small: true))
                Spacer()
            }
            HStack(spacing: Hy.s1) {
                Button("Profiles") { state.runCLI(["claude", "detect"], title: "Claude config dirs") }
                Button("Install CLI") { state.runCLI(["link", "--add-to-path"], title: "iris link") }
                Button("Config") { state.openConfig() }
                Button("Audit log") { state.openAuditLog() }
            }
            .buttonStyle(HyButton(kind: .quiet, small: true))

            Neatline()

            Toggle("Launch at login", isOn: Binding(get: { state.launchAtLogin }, set: { state.setLaunchAtLogin($0) }))
                .toggleStyle(HyToggle(onLabel: "On", offLabel: "Off"))

            HStack {
                Text("The broker keeps running when Iris quits.")
                    .font(Hy.ui(11.5))
                    .foregroundStyle(Hy.faint)
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }
                    .buttonStyle(HyButton(kind: .quiet, small: true))
            }
        }
    }
}

// MARK: - Rows

struct GrantRow: View {
    let grant: GrantInfo
    let revoke: () -> Void
    @State private var hover = false

    var body: some View {
        HStack(alignment: .center, spacing: Hy.s3) {
            StarGlyph(state: .live, size: 16)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: Hy.s2) {
                    Text(grant.tool).font(Hy.ui(13.5, 600)).foregroundStyle(Hy.starlight)
                    if !grant.scope.isEmpty { HyTag(text: grant.scope) }
                    if let p = grant.profile, !p.isEmpty { HyTag(text: p, kind: .a) }
                }
                Text(grant.target)
                    .font(Hy.mono(11.5))
                    .foregroundStyle(Hy.dust)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text("\(grant.option) · \(grant.uses) use\(grant.uses == 1 ? "" : "s") · expires \(grant.expiresAt, style: .relative)")
                    .font(Hy.mono(10.5))
                    .foregroundStyle(Hy.faint)
            }
            Spacer(minLength: Hy.s2)
            Button("Revoke", action: revoke)
                .buttonStyle(HyButton(kind: .quiet, small: true))
                .opacity(hover ? 1 : 0.6)
        }
        .padding(.horizontal, Hy.s4)
        .padding(.vertical, Hy.s3)
        .background(hover ? Hy.skyHigh : .clear)
        .onHover { h in withAnimation(.easeOut(duration: 0.12)) { hover = h } }
    }
}

struct AuditRow: View {
    let entry: AuditEntry

    var body: some View {
        HStack(spacing: Hy.s3) {
            Text(entry.time, format: .dateTime.hour().minute())
                .font(Hy.mono(11))
                .foregroundStyle(Hy.faint)
                .frame(width: 38, alignment: .leading)
            HyTag(text: entry.event, kind: kind)
                .frame(width: 70, alignment: .leading)
            Text([entry.tool, entry.target].compactMap { $0 }.joined(separator: " · "))
                .font(Hy.mono(11.5))
                .foregroundStyle(Hy.dust)
                .lineLimit(1)
                .truncationMode(.middle)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, Hy.s4)
        .frame(height: 30)
    }

    /// Colour is state: issued = live (A), granted = pending light (G), denied/failed = M.
    private var kind: HyTag.Kind {
        switch entry.event {
        case "issue", "proxy-inject": return .a
        case "grant": return .g
        case "deny", "timeout", "error": return .m
        default: return .plain
        }
    }
}
