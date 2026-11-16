import AppKit
import SwiftUI

/// Geometry for an aperture-style iris. Neighbouring blades meet along a
/// spiral curve that runs from the aperture to the ring; closed, the curves
/// all start at the centre (a swirl), and as it opens the blades rotate and
/// retract past the ring. Shared by the panel and the menu bar icon.
struct IrisGeometry {
    let center: CGPoint
    let radius: CGFloat      // inner radius of the ring (blade area)
    let openness: CGFloat    // 0 closed ... 1 open
    let count: Int

    private var step: CGFloat { 2 * .pi / CGFloat(count) }
    private var aperture: CGFloat { radius * 1.45 * openness }
    private var spin: CGFloat { openness * step * 1.2 }
    private var twist: CGFloat { (115 - 55 * openness) * .pi / 180 }
    private var rim: CGFloat { radius * 1.06 }

    private struct Seam { let p0, c1, c2, p1: CGPoint; let endAngle: CGFloat }

    private func seam(_ i: Int) -> Seam {
        let a = spin + CGFloat(i) * step
        let e = a + twist
        let p0 = CGPoint(x: center.x + aperture * cos(a), y: center.y + aperture * sin(a))
        let p1 = CGPoint(x: center.x + rim * cos(e), y: center.y + rim * sin(e))
        let c1 = CGPoint(x: p0.x + 0.55 * radius * cos(a + .pi / 2), y: p0.y + 0.55 * radius * sin(a + .pi / 2))
        let c2 = CGPoint(x: p1.x - 0.35 * radius * cos(e), y: p1.y - 0.35 * radius * sin(e))
        return Seam(p0: p0, c1: c1, c2: c2, p1: p1, endAngle: e)
    }

    /// The visible seam between blade i-1 and blade i.
    func edge(_ i: Int) -> CGPath {
        let s = seam(i)
        let p = CGMutablePath()
        p.move(to: s.p0)
        p.addCurve(to: s.p1, control1: s.c1, control2: s.c2)
        return p
    }

    /// Blade i: between seam i and seam i+1, closed along the rim and the aperture.
    func blade(_ i: Int) -> CGPath {
        let s = seam(i), t = seam(i + 1)
        let p = CGMutablePath()
        p.move(to: s.p0)
        p.addCurve(to: s.p1, control1: s.c1, control2: s.c2)
        p.addArc(center: center, radius: rim, startAngle: s.endAngle, endAngle: t.endAngle, clockwise: false)
        p.addCurve(to: t.p0, control1: t.c2, control2: t.c1)
        p.closeSubpath()
        return p
    }
}

/// Animated, shaded iris for the approval panel.
struct IrisShutter: View, Animatable {
    var openness: Double
    var granted: Bool = false
    var blades: Int = 12

    var animatableData: Double {
        get { openness }
        set { openness = newValue }
    }

    var body: some View {
        Canvas { ctx, size in
            render(&ctx, size: size)
        }
        .accessibilityLabel(openness > 0.5 ? "Iris open" : "Iris closed")
    }

    // Drawing is split into small, explicitly typed steps: one big Canvas
    // closure exceeds the Swift type checker's time limit.

    private struct Layout {
        let c: CGPoint
        let r: CGFloat
        let ring: CGFloat
        let side: CGFloat
        let disc: Path
    }

    private func layout(_ size: CGSize) -> Layout {
        let side: CGFloat = min(size.width, size.height)
        let ring: CGFloat = side * 0.07
        let r: CGFloat = side / 2 - ring
        let c = CGPoint(x: size.width / 2, y: size.height / 2)
        let disc = Path(ellipseIn: CGRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r))
        return Layout(c: c, r: r, ring: ring, side: side, disc: disc)
    }

    private func render(_ ctx: inout GraphicsContext, size: CGSize) {
        let L = layout(size)
        drawBackground(&ctx, L)
        drawBlades(&ctx, L)
        drawRing(&ctx, L)
    }

    private func drawBackground(_ ctx: inout GraphicsContext, _ L: Layout) {
        let glow: Color = granted ? Color(red: 0.25, green: 0.85, blue: 0.55) : Color(white: 0.25)
        let gradient = Gradient(colors: [glow.opacity(0.9), Color.black])
        let shading = GraphicsContext.Shading.radialGradient(gradient, center: L.c, startRadius: 0, endRadius: L.r)
        ctx.fill(L.disc, with: shading)
    }

    private func drawBlades(_ ctx: inout GraphicsContext, _ L: Layout) {
        let g = IrisGeometry(center: L.c, radius: L.r, openness: CGFloat(openness), count: blades)
        var l = ctx
        l.clip(to: L.disc)
        let start = CGPoint(x: L.c.x - L.r, y: L.c.y + L.r)
        let end = CGPoint(x: L.c.x + L.r, y: L.c.y - L.r)
        for i in 0..<blades {
            let shade: Double = 0.50 + 0.18 * sin(Double(i) * 1.7)
            let gradient = Gradient(colors: [Color(white: shade + 0.2), Color(white: shade - 0.12)])
            l.fill(Path(g.blade(i)), with: .linearGradient(gradient, startPoint: start, endPoint: end))
        }
        let seamWidth: CGFloat = max(1, L.side / 140)
        for i in 0..<blades {
            l.stroke(Path(g.edge(i)), with: .color(.black.opacity(0.6)), lineWidth: seamWidth)
        }
        l.stroke(L.disc, with: .color(.black.opacity(0.5)), lineWidth: L.side / 40) // inner shadow
    }

    private func drawRing(_ ctx: inout GraphicsContext, _ L: Layout) {
        let mid: CGFloat = L.r + L.ring / 2
        let ringRect = CGRect(x: L.c.x - mid, y: L.c.y - mid, width: 2 * mid, height: 2 * mid)
        let metal = Gradient(colors: [Color(white: 0.75), Color(white: 0.35), Color(white: 0.8), Color(white: 0.3), Color(white: 0.75)])
        ctx.stroke(Path(ellipseIn: ringRect), with: .conicGradient(metal, center: L.c), lineWidth: L.ring)
        let bolt: CGFloat = L.ring * 0.32
        for k in 0..<blades {
            let a: CGFloat = CGFloat(k) / CGFloat(blades) * 2 * .pi
            let p = CGPoint(x: L.c.x + mid * cos(a), y: L.c.y + mid * sin(a))
            let rect = CGRect(x: p.x - bolt / 2, y: p.y - bolt / 2, width: bolt, height: bolt)
            ctx.fill(Path(ellipseIn: rect), with: .color(Color(white: 0.2)))
        }
    }
}

/// Drives the panel's shutter.
@MainActor
final class ShutterModel: ObservableObject {
    @Published var openness: Double = 0
    @Published var granted = false
    @Published var shake: CGFloat = 0

    /// Opens the iris, then calls `done` once the animation has played.
    func open(then done: @escaping @MainActor () -> Void) {
        granted = true
        withAnimation(.easeInOut(duration: 0.75)) { openness = 1 }
        after(0.95, done)
    }

    private func after(_ seconds: Double, _ f: @escaping @MainActor () -> Void) {
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
            f()
        }
    }

    /// Stays shut with a short "clunk".
    func deny(then done: @escaping @MainActor () -> Void) {
        withAnimation(.easeOut(duration: 0.08)) { openness = 0.06 }
        after(0.08) {
            withAnimation(.spring(response: 0.18, dampingFraction: 0.35)) { self.openness = 0; self.shake += 1 }
        }
        after(0.45, done)
    }
}

/// Horizontal shake used on deny.
struct Shake: GeometryEffect {
    var animatableData: CGFloat
    func effectValue(size: CGSize) -> ProjectionTransform {
        ProjectionTransform(CGAffineTransform(translationX: 6 * sin(animatableData * .pi * 4), y: 0))
    }
}

/// Template images for the menu bar (black lines; macOS tints them).
enum IrisIcon {
    enum State { case disconnected, closed, open }

    static func image(_ state: State) -> NSImage {
        let size = NSSize(width: 18, height: 18)
        let img = NSImage(size: size, flipped: false) { rect in
            guard let cg = NSGraphicsContext.current?.cgContext else { return false }
            let c = CGPoint(x: rect.midX, y: rect.midY)
            let r: CGFloat = 7.2
            cg.setStrokeColor(NSColor.black.cgColor)
            cg.setLineWidth(1.6)
            cg.strokeEllipse(in: CGRect(x: c.x - r - 0.6, y: c.y - r - 0.6, width: 2 * r + 1.2, height: 2 * r + 1.2))

            let open: CGFloat = state == .open ? 0.55 : 0
            let g = IrisGeometry(center: c, radius: r, openness: open, count: 7)
            cg.saveGState()
            cg.addEllipse(in: CGRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r))
            cg.clip()
            if state == .open {
                // Fill the blades so the opening reads as a hole.
                cg.setFillColor(NSColor.black.withAlphaComponent(0.55).cgColor)
                for i in 0..<7 {
                    cg.addPath(g.blade(i))
                    cg.fillPath()
                }
            }
            cg.setLineWidth(1.0)
            for i in 0..<7 {
                cg.addPath(g.edge(i))
                cg.strokePath()
            }
            cg.restoreGState()

            if state == .disconnected {
                cg.setLineWidth(1.8)
                cg.move(to: CGPoint(x: rect.minX + 2, y: rect.minY + 2))
                cg.addLine(to: CGPoint(x: rect.maxX - 2, y: rect.maxY - 2))
                cg.strokePath()
            }
            return true
        }
        img.isTemplate = true
        return img
    }
}
