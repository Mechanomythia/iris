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
            let side = min(size.width, size.height)
            let ring = side * 0.07
            let r = side / 2 - ring
            let c = CGPoint(x: size.width / 2, y: size.height / 2)
            let g = IrisGeometry(center: c, radius: r, openness: CGFloat(openness), count: blades)
            let disc = Path(ellipseIn: CGRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r))

            // What's behind the shutter.
            ctx.drawLayer { l in
                l.clip(to: disc)
                let glow: Color = granted ? Color(red: 0.25, green: 0.85, blue: 0.55) : Color(white: 0.25)
                l.fill(disc, with: .radialGradient(Gradient(colors: [glow.opacity(0.9), Color.black]),
                                                   center: c, startRadius: 0, endRadius: r))
            }

            // Blades.
            ctx.drawLayer { l in
                l.clip(to: disc)
                for i in 0..<blades {
                    let shade = 0.50 + 0.18 * sin(Double(i) * 1.7)
                    l.fill(Path(g.blade(i)), with: .linearGradient(
                        Gradient(colors: [Color(white: shade + 0.2), Color(white: shade - 0.12)]),
                        startPoint: CGPoint(x: c.x - r, y: c.y + r), endPoint: CGPoint(x: c.x + r, y: c.y - r)))
                }
                for i in 0..<blades {
                    l.stroke(Path(g.edge(i)), with: .color(.black.opacity(0.6)), lineWidth: max(1, side / 140))
                }
                // Subtle inner shadow.
                l.stroke(disc, with: .color(.black.opacity(0.5)), lineWidth: side / 40)
            }

            // Ring with bolts.
            let ringPath = Path(ellipseIn: CGRect(x: c.x - r - ring / 2, y: c.y - r - ring / 2, width: 2 * r + ring, height: 2 * r + ring))
            ctx.stroke(ringPath, with: .conicGradient(
                Gradient(colors: [Color(white: 0.75), Color(white: 0.35), Color(white: 0.8), Color(white: 0.3), Color(white: 0.75)]),
                center: c), lineWidth: ring)
            for k in 0..<blades {
                let a = Double(k) / Double(blades) * 2 * .pi
                let p = CGPoint(x: c.x + (r + ring / 2) * cos(a), y: c.y + (r + ring / 2) * sin(a))
                let s = ring * 0.32
                ctx.fill(Path(ellipseIn: CGRect(x: p.x - s / 2, y: p.y - s / 2, width: s, height: s)), with: .color(Color(white: 0.2)))
            }
        }
        .accessibilityLabel(openness > 0.5 ? "Iris open" : "Iris closed")
    }
}

/// Drives the panel's shutter.
@MainActor
final class ShutterModel: ObservableObject {
    @Published var openness: Double = 0
    @Published var granted = false
    @Published var shake: CGFloat = 0

    /// Opens the iris, then calls `done` once the animation has played.
    func open(then done: @escaping () -> Void) {
        granted = true
        withAnimation(.easeInOut(duration: 0.75)) { openness = 1 }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.95, execute: done)
    }

    /// Stays shut with a short "clunk".
    func deny(then done: @escaping () -> Void) {
        withAnimation(.easeOut(duration: 0.08)) { openness = 0.06 }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.08) {
            withAnimation(.spring(response: 0.18, dampingFraction: 0.35)) { self.openness = 0; self.shake += 1 }
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.45, execute: done)
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
