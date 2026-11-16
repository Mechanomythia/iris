import AppKit
import SwiftUI

@main
struct IrisApp: App {
    @StateObject private var state = AppState()

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

struct MenuContent: View {
    @EnvironmentObject var state: AppState

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            Divider()
            grantsSection
            Divider()
            recentSection
            Divider()
            actions
        }
        .padding(14)
        .frame(width: 380)
        .onAppear { state.refresh() }
    }

    private var header: some View {
        HStack {
            Circle().fill(state.connected ? Color.green : Color.red).frame(width: 8, height: 8)
            Text(state.connected ? "Broker running" : "Broker not reachable")
                .font(.headline)
            Spacer()
            if let v = state.status?.version { Text("v\(v)").font(.caption).foregroundStyle(.secondary) }
        }
    }

    private var grantsSection: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Active grants").font(.subheadline.weight(.semibold))
                Spacer()
                Button("Revoke all") { state.revoke(nil) }
                    .disabled(state.grants.isEmpty)
                    .tint(.red)
            }
            if state.grants.isEmpty {
                Text("None. Claude must ask before using any credential.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            ForEach(state.grants) { g in
                HStack {
                    VStack(alignment: .leading, spacing: 1) {
                        Text("\(g.tool) · \(g.target)\(g.scope.isEmpty ? "" : " · " + g.scope)")
                            .font(.callout)
                        if let p = g.profile, !p.isEmpty {
                            Text(p).font(.caption2).foregroundStyle(.secondary)
                        }
                        Text("\(g.option) · \(g.uses) use\(g.uses == 1 ? "" : "s") · expires \(g.expiresAt, style: .relative)")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button {
                        state.revoke(g.id)
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                    }
                    .buttonStyle(.borderless)
                    .help("Revoke")
                }
            }
        }
    }

    private var recentSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Recent").font(.subheadline.weight(.semibold))
            if state.recent.isEmpty {
                Text("No activity yet").font(.caption).foregroundStyle(.secondary)
            }
            ForEach(state.recent.prefix(6), id: \.self) { e in
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(e.time, style: .time).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                    Text(e.event).font(.caption.weight(.medium)).foregroundStyle(color(e.event))
                    Text([e.tool, e.target].compactMap { $0 }.joined(separator: "/"))
                        .font(.caption).lineLimit(1)
                }
            }
        }
    }

    private var actions: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let err = state.lastError, state.connected {
                Text(err).font(.caption).foregroundStyle(.red)
            }
            HStack {
                Button("Set up Claude Code") { state.runCLI(["install", "--detect"], title: "iris install") }
                Button("Doctor") { state.runCLI(["doctor"], title: "iris doctor") }
            }
            HStack {
                Button("Claude profiles") { state.runCLI(["claude", "detect"], title: "Claude config dirs") }
                Button("Install `iris` command") { state.runCLI(["link", "--add-to-path"], title: "iris link") }
            }
            HStack {
                Button("Edit config") { state.openConfig() }
                Button("Audit log") { state.openAuditLog() }
            }
            Toggle("Launch at login", isOn: Binding(get: { state.launchAtLogin }, set: { state.setLaunchAtLogin($0) }))
                .toggleStyle(.checkbox)
            Button("Quit Iris") { NSApp.terminate(nil) }
                .help("The broker keeps running; prompts fall back to a basic dialog.")
        }
    }

    private func color(_ event: String) -> Color {
        switch event {
        case "deny", "timeout", "error": return .red
        case "grant": return .orange
        case "issue", "proxy-inject": return .green
        default: return .secondary
        }
    }
}
