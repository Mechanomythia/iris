import AppKit
import SwiftUI

// The island: a black shape that grows out of the camera housing (or, on
// screens without one, drops from the top edge). It shows a ticker line for
// every use that went through without a popup, opens into a list on click,
// and approval popups morph out of it.

// MARK: - Geometry

/// Above the menu bar, so the island can sit in it and merge with the notch.
let islandLevel = NSWindow.Level(rawValue: Int(CGWindowLevelForKey(.mainMenuWindow)) + 2)

struct IslandGeometry {
    let screen: NSScreen
    /// The notch in screen coordinates. Without one: a thin slot at the top
    /// centre that the island drops out of.
    let notch: NSRect
    let hasNotch: Bool

    /// The screen under the pointer, like the approval popup.
    static func current() -> IslandGeometry {
        let mouse = NSEvent.mouseLocation
        let screen = NSScreen.screens.first { NSMouseInRect(mouse, $0.frame, false) } ?? NSScreen.main ?? NSScreen.screens[0]
        return IslandGeometry(screen: screen)
    }

    init(screen: NSScreen) {
        self.screen = screen
        let f = screen.frame
        let top = screen.safeAreaInsets.top
        if top > 0, let l = screen.auxiliaryTopLeftArea, let r = screen.auxiliaryTopRightArea {
            let w = f.width - l.width - r.width
            notch = NSRect(x: f.minX + l.width, y: f.maxY - top, width: max(w, 120), height: top)
            hasNotch = true
        } else {
            notch = NSRect(x: f.midX - 90, y: f.maxY - 2, width: 180, height: 2)
            hasNotch = false
        }
    }

    /// The band beside the notch (the menu bar height on notch screens).
    var band: CGFloat { hasNotch ? notch.height : 0 }

    /// A rect hanging from the top edge, centred on the notch.
    func hanging(width: CGFloat, height: CGFloat) -> NSRect {
        let w = max(width, notch.width)
        return NSRect(x: notch.midX - w / 2, y: screen.frame.maxY - height, width: w, height: height)
    }

    /// Where the island hides: exactly the notch (black on black), or the slot.
    var tucked: NSRect { hanging(width: notch.width, height: max(band, 2)) }
}

/// Square top (flush with the screen edge), rounded bottom; both animatable
/// so a popup can morph from island to plate.
struct IslandShape: Shape {
    var radius: CGFloat
    var topRadius: CGFloat = 0

    var animatableData: AnimatablePair<CGFloat, CGFloat> {
        get { AnimatablePair(radius, topRadius) }
        set { radius = newValue.first; topRadius = newValue.second }
    }

    func path(in r: CGRect) -> Path {
        let br = min(radius, r.height / 2, r.width / 2)
        let tr = min(topRadius, r.height / 2, r.width / 2)
        var p = Path()
        p.move(to: CGPoint(x: r.minX + tr, y: r.minY))
        p.addLine(to: CGPoint(x: r.maxX - tr, y: r.minY))
        if tr > 0 { p.addQuadCurve(to: CGPoint(x: r.maxX, y: r.minY + tr), control: CGPoint(x: r.maxX, y: r.minY)) }
        p.addLine(to: CGPoint(x: r.maxX, y: r.maxY - br))
        p.addQuadCurve(to: CGPoint(x: r.maxX - br, y: r.maxY), control: CGPoint(x: r.maxX, y: r.maxY))
        p.addLine(to: CGPoint(x: r.minX + br, y: r.maxY))
        p.addQuadCurve(to: CGPoint(x: r.minX, y: r.maxY - br), control: CGPoint(x: r.minX, y: r.maxY))
        p.addLine(to: CGPoint(x: r.minX, y: r.minY + tr))
        if tr > 0 { p.addQuadCurve(to: CGPoint(x: r.minX + tr, y: r.minY), control: CGPoint(x: r.minX, y: r.minY)) }
        p.closeSubpath()
        return p
    }
}

/// Same curve as Dynamic Island-style motion: fast out, soft landing.
let islandTiming = CAMediaTimingFunction(controlPoints: 0.2, 0.9, 0.25, 1.0)

// MARK: - Windows

/// Borderless, never key, allowed over the menu bar.
final class IslandPanel: NSPanel {
    override var canBecomeKey: Bool { false }
    override var canBecomeMain: Bool { false }
    override func constrainFrameRect(_ frameRect: NSRect, to screen: NSScreen?) -> NSRect { frameRect }
}

/// Clicks work on the first click, without activating Iris first.
final class FirstMouseHostingView<Content: View>: NSHostingView<Content> {
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}

// MARK: - Model

@MainActor
final class IslandModel: ObservableObject {
    enum Mode { case hidden, ticker, expanded }
    @Published var mode: Mode = .hidden
    /// Newest first.
    @Published var events: [BrokerEvent] = []
    /// Bumps per event: keys the ticker line's slide-in.
    @Published var pulse = 0
    @Published var band: CGFloat = 0
    @Published var notchWidth: CGFloat = 0
    @Published var hasNotch = false

    static let rowHeight: CGFloat = 34
    static let maxRows = 8
}

// MARK: - Controller

@MainActor
final class IslandController {
    let model = IslandModel()
    private var panel: IslandPanel?
    private var geo = IslandGeometry.current()
    private var collapse: Task<Void, Never>?

    /// A use went through without a popup.
    func show(_ recent: [BrokerEvent]) {
        model.events = Array(recent.prefix(IslandModel.maxRows))
        withAnimation(.spring(response: 0.42, dampingFraction: 0.8)) { model.pulse += 1 }
        switch model.mode {
        case .expanded:
            resize(.expanded) // the list grew
        case .ticker:
            schedule(after: 4.5)
        case .hidden:
            geo = IslandGeometry.current()
            set(.ticker)
            schedule(after: 4.5)
        }
    }

    func tapped() {
        collapse?.cancel()
        set(model.mode == .expanded ? .hidden : .expanded)
    }

    func hover(_ inside: Bool) {
        if inside {
            collapse?.cancel()
        } else if model.mode != .hidden {
            schedule(after: model.mode == .expanded ? 1.2 : 2.0)
        }
    }

    private func schedule(after seconds: Double) {
        collapse?.cancel()
        collapse = Task { @MainActor in
            try? await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
            guard !Task.isCancelled else { return }
            self.set(.hidden)
        }
    }

    private func frame(for mode: IslandModel.Mode) -> NSRect {
        let sides: CGFloat = 2 * 120
        switch mode {
        case .hidden:
            return geo.tucked
        case .ticker:
            return geo.hanging(width: max(geo.notch.width + sides, 400), height: geo.band + 36)
        case .expanded:
            let rows = CGFloat(max(1, min(model.events.count, IslandModel.maxRows)))
            let header: CGFloat = geo.hasNotch ? geo.band : 32
            return geo.hanging(width: max(geo.notch.width + sides, 480), height: header + 10 + rows * IslandModel.rowHeight + 12)
        }
    }

    private func makePanel() -> IslandPanel {
        let view = IslandView(model: model, onTap: { [weak self] in self?.tapped() }, onHover: { [weak self] in self?.hover($0) })
            .environment(\.colorScheme, .dark)
        let host = FirstMouseHostingView(rootView: view)
        host.sizingOptions = [] // the window's frame drives the size, not the content
        let p = IslandPanel(contentRect: geo.tucked, styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
        p.contentView = host
        p.isOpaque = false
        p.backgroundColor = .clear
        p.hasShadow = false
        p.level = islandLevel
        p.hidesOnDeactivate = false
        p.acceptsMouseMovedEvents = true
        p.collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary, .ignoresCycle]
        panel = p
        return p
    }

    private func set(_ mode: IslandModel.Mode) {
        let p = panel ?? makePanel()
        model.band = geo.band
        model.notchWidth = geo.notch.width
        model.hasNotch = geo.hasNotch
        if mode != .hidden && !p.isVisible {
            p.setFrame(geo.tucked, display: false)
            p.alphaValue = 1
            p.orderFrontRegardless()
        }
        withAnimation(.spring(response: 0.4, dampingFraction: 0.82)) { model.mode = mode }
        resize(mode)
    }

    private func resize(_ mode: IslandModel.Mode) {
        guard let p = panel else { return }
        let target = frame(for: mode)
        let fade = mode == .hidden && !geo.hasNotch // nothing to hide behind
        NSAnimationContext.runAnimationGroup({ ctx in
            ctx.duration = mode == .hidden ? 0.3 : 0.42
            ctx.timingFunction = islandTiming
            p.animator().setFrame(target, display: true)
            if fade { p.animator().alphaValue = 0 }
        }, completionHandler: {
            Task { @MainActor in
                if self.model.mode == .hidden { p.orderOut(nil) }
            }
        })
    }
}

// MARK: - Views

struct IslandView: View {
    @ObservedObject var model: IslandModel
    let onTap: () -> Void
    let onHover: (Bool) -> Void

    private var radius: CGFloat {
        switch model.mode {
        case .hidden: return 10
        case .ticker: return 16
        case .expanded: return 22
        }
    }

    var body: some View {
        IslandShape(radius: radius)
            .fill(Color.black)
            .overlay(alignment: .top) { content.opacity(model.mode == .hidden ? 0 : 1) }
            .clipShape(IslandShape(radius: radius))
            .contentShape(IslandShape(radius: radius))
            .onTapGesture(perform: onTap)
            .onHover(perform: onHover)
    }

    @ViewBuilder private var content: some View {
        VStack(alignment: .leading, spacing: 0) {
            if model.hasNotch {
                // Beside the camera: iris on the left, what it was on the right.
                HStack(spacing: 0) {
                    irisGlyph
                    Spacer(minLength: model.notchWidth + 8)
                    accessTag
                }
                .padding(.horizontal, 14)
                .frame(height: model.band)
            }
            switch model.mode {
            case .expanded:
                list
            default:
                tickerRow
            }
        }
    }

    private var current: BrokerEvent? { model.events.first }

    private var irisGlyph: some View {
        Image(nsImage: IrisIcon.image(.open))
            .renderingMode(.template)
            .resizable()
            .frame(width: 15, height: 15)
            .foregroundStyle(Hy.classA)
    }

    @ViewBuilder private var accessTag: some View {
        if model.mode == .expanded {
            Text("\(model.events.count)")
                .font(Hy.mono(11))
                .foregroundStyle(Hy.faint)
        } else if let e = current {
            Text((e.access ?? "use").uppercased())
                .font(Hy.ui(9.5, 650))
                .tracking(1)
                .foregroundStyle(e.access == "read" ? Hy.classA : Hy.classK)
        }
    }

    /// One line that slides in from below for each new use.
    private var tickerRow: some View {
        HStack(spacing: 8) {
            if !model.hasNotch {
                irisGlyph
            }
            ZStack(alignment: .leading) {
                if let e = current {
                    Marquee(text: AppState.describe(e))
                        .id(model.pulse)
                        .transition(.asymmetric(insertion: .move(edge: .bottom).combined(with: .opacity),
                                                removal: .move(edge: .top).combined(with: .opacity)))
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .clipped()
            if !model.hasNotch {
                accessTag
            }
        }
        .padding(.horizontal, 16)
        .frame(height: 36)
    }

    private var list: some View {
        VStack(alignment: .leading, spacing: 0) {
            if !model.hasNotch {
                HStack {
                    irisGlyph
                    Text("Allowed without asking").font(Hy.ui(12, 600)).foregroundStyle(Hy.starlight)
                    Spacer()
                    accessTag
                }
                .padding(.horizontal, 16)
                .frame(height: 32)
            } else {
                Text("ALLOWED WITHOUT ASKING")
                    .font(Hy.ui(9.5, 650))
                    .tracking(1.2)
                    .foregroundStyle(Hy.faint)
                    .padding(.horizontal, 16)
                    .padding(.top, 4)
            }
            ForEach(Array(model.events.enumerated()), id: \.offset) { _, e in
                HStack(spacing: 10) {
                    Text(e.time ?? Date(), format: .dateTime.hour().minute().second())
                        .font(Hy.mono(10.5))
                        .foregroundStyle(Hy.faint)
                        .frame(width: 58, alignment: .leading)
                    Text((e.access ?? "use").uppercased())
                        .font(Hy.ui(9, 650))
                        .tracking(0.8)
                        .foregroundStyle(e.access == "read" ? Hy.classA : Hy.classK)
                        .frame(width: 40, alignment: .leading)
                    Text(AppState.describe(e))
                        .font(Hy.mono(11.5))
                        .foregroundStyle(Hy.starlight)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer(minLength: 0)
                }
                .padding(.horizontal, 16)
                .frame(height: IslandModel.rowHeight)
                .help((e.detail ?? "") + "\ncovered by " + (e.grant ?? "a grant"))
            }
        }
        .padding(.top, 6)
    }
}

/// Text that scrolls once to its end when it doesn't fit.
struct Marquee: View {
    let text: String
    @State private var offset: CGFloat = 0

    var body: some View {
        GeometryReader { box in
            Text(text)
                .font(Hy.mono(12))
                .foregroundStyle(Hy.starlight)
                .lineLimit(1)
                .fixedSize()
                .background(GeometryReader { t in
                    Color.clear.onAppear { scroll(textWidth: t.size.width, boxWidth: box.size.width) }
                })
                .offset(x: offset)
                .frame(width: box.size.width, height: box.size.height, alignment: .leading)
        }
        .frame(height: 18)
        .clipped()
    }

    private func scroll(textWidth: CGFloat, boxWidth: CGFloat) {
        let over = textWidth - boxWidth
        guard over > 4 else { return }
        withAnimation(.linear(duration: Double(over) / 45).delay(0.9)) { offset = -over }
    }
}

// MARK: - Popups out of the island

@MainActor
final class MorphModel: ObservableObject {
    @Published var open = false
}

/// Hosts a popup: an island-black shape that morphs into the plate, with
/// the popup fading in once it has room.
struct MorphHost<Content: View>: View {
    @ObservedObject var morph: MorphModel
    let size: CGSize
    @ViewBuilder let content: Content

    var body: some View {
        let shape = IslandShape(radius: morph.open ? Hy.r2 : 16, topRadius: morph.open ? Hy.r2 : 0)
        shape
            .fill(morph.open ? Hy.skyRaised : Color.black)
            .overlay(alignment: .top) {
                content
                    .frame(width: size.width, height: size.height)
                    .opacity(morph.open ? 1 : 0)
                    .scaleEffect(morph.open ? 1 : 0.94, anchor: .top)
            }
            .clipShape(shape)
    }
}
