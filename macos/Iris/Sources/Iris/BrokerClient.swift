import Foundation

// MARK: - Wire types (mirror internal/protocol in Go)

struct OptionInfo: Codable, Hashable {
    let name: String
    let display: String
}

struct CommandSpan: Codable, Hashable {
    let text: String
    let kind: String
}

struct BrokerEvent: Codable {
    let type: String
    var id: String?
    var tool: String?
    var target: String?
    var scope: String?
    var label: String?
    var profile: String?
    var passthrough: Bool?
    var command: String?
    var commandSpans: [CommandSpan]?
    var pid: Int?
    var cwd: String?
    var argv: [String]?
    var options: [OptionInfo]?
    var touchID: Bool?
    var deadline: Date?
    var recent: Int?
}

struct GrantInfo: Codable, Identifiable, Hashable {
    let id: String
    let tool: String
    let target: String
    let scope: String
    var profile: String?
    let option: String
    let grantedAt: Date
    let expiresAt: Date
    let uses: Int
}

struct StatusInfo: Codable {
    let version: String
    let startedAt: Date
    let uiConnected: Bool
    let identities: [String]?
    let proxyListen: String?
    let pendingPrompts: Int
    var warnings: [String]?
}

struct AuditEntry: Codable, Hashable {
    let time: Date
    let event: String
    var tool: String?
    var target: String?
    var profile: String?
    var option: String?
    var command: String?
    var detail: String?
}

struct BrokerResponse: Codable {
    let ok: Bool
    var error: String?
    var grants: [GrantInfo]?
    var status: StatusInfo?
    var audit: [AuditEntry]?
    var revoked: Int?
}

enum Wire {
    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        // Never throw: one odd date must not drop a whole event (e.g. a cancel).
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            return parseDate(s) ?? .distantPast
        }
        return d
    }()

    /// RFC 3339 with or without fractional seconds (Go's time.Time encoding).
    static func parseDate(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}

// MARK: - Paths (mirror internal/paths)

enum IrisPaths {
    static var home: String { ProcessInfo.processInfo.environment["IRIS_HOME"] ?? NSHomeDirectory() }
    static var stateDir: String { home + "/.local/state/iris" }
    static var controlSocket: String {
        ProcessInfo.processInfo.environment["IRIS_CONTROL_SOCKET"] ?? stateDir + "/control.sock"
    }
    static var configFile: String { home + "/.config/iris/config.yaml" }
    static var auditLog: String { stateDir + "/audit.jsonl" }
    static var bundledBin: String { Bundle.main.resourcePath.map { $0 + "/bin" } ?? "" }
}

// MARK: - Line-oriented Unix socket

enum SocketError: Error, LocalizedError {
    case path, connect(Int32), write, closed

    var errorDescription: String? {
        switch self {
        case .path: return "socket path too long"
        case .connect(let e): return "connect failed: \(String(cString: strerror(e)))"
        case .write: return "write failed"
        case .closed: return "connection closed"
        }
    }
}

final class LineSocket {
    private let fd: Int32
    private var buffer = Data()
    private let lock = NSLock()

    init(path: String) throws {
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw SocketError.connect(errno) }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        let cap = MemoryLayout.size(ofValue: addr.sun_path)
        guard bytes.count < cap else { close(fd); throw SocketError.path }
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: bytes)
            raw[bytes.count] = 0
        }
        let len = socklen_t(MemoryLayout<sockaddr_un>.size)
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.connect(fd, $0, len) }
        }
        guard rc == 0 else {
            let e = errno
            close(fd)
            throw SocketError.connect(e)
        }
        var on: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
    }

    deinit { close(fd) }

    func shutdown() { Darwin.shutdown(fd, SHUT_RDWR) }

    func send(_ object: [String: Any]) throws {
        var data = try JSONSerialization.data(withJSONObject: object)
        data.append(0x0A)
        lock.lock(); defer { lock.unlock() }
        try data.withUnsafeBytes { raw in
            var off = 0
            while off < raw.count {
                let n = Darwin.write(fd, raw.baseAddress!.advanced(by: off), raw.count - off)
                if n <= 0 { throw SocketError.write }
                off += n
            }
        }
    }

    /// Blocks until a full line arrives. nil = closed.
    func readLine() -> Data? {
        while true {
            if let i = buffer.firstIndex(of: 0x0A) {
                let line = buffer[buffer.startIndex..<i]
                buffer.removeSubrange(buffer.startIndex...i)
                return Data(line)
            }
            var chunk = [UInt8](repeating: 0, count: 16 * 1024)
            let n = Darwin.read(fd, &chunk, chunk.count)
            if n <= 0 { return nil }
            buffer.append(contentsOf: chunk[0..<n])
        }
    }
}

// MARK: - Client

/// One-shot request/response on the control socket.
enum Control {
    static func call(_ req: [String: Any]) throws -> BrokerResponse {
        let s = try LineSocket(path: IrisPaths.controlSocket)
        try s.send(req)
        guard let line = s.readLine() else { throw SocketError.closed }
        return try Wire.decoder.decode(BrokerResponse.self, from: line)
    }
}

/// Long-lived subscription that receives approval requests. Reconnects forever.
final class Subscription {
    var onEvent: (BrokerEvent) -> Void = { _ in }
    var onConnection: (Bool) -> Void = { _ in }
    private var socket: LineSocket?
    private let queue = DispatchQueue(label: "iris.subscription")
    private var stopped = false

    func start() { queue.async { self.loop() } }

    func stop() {
        stopped = true
        socket?.shutdown()
    }

    func send(_ req: [String: Any]) {
        do { try socket?.send(req) } catch { NSLog("iris: send failed: \(error)") }
    }

    private func loop() {
        while !stopped {
            do {
                let s = try LineSocket(path: IrisPaths.controlSocket)
                try s.send(["op": "subscribe"])
                guard let first = s.readLine(),
                      let resp = try? Wire.decoder.decode(BrokerResponse.self, from: first),
                      resp.ok else {
                    throw SocketError.closed
                }
                socket = s
                DispatchQueue.main.async { self.onConnection(true) }
                while let line = s.readLine() {
                    if let ev = try? Wire.decoder.decode(BrokerEvent.self, from: line) {
                        DispatchQueue.main.async { self.onEvent(ev) }
                    }
                }
            } catch {
                // daemon not running yet; retry
            }
            socket = nil
            DispatchQueue.main.async { self.onConnection(false) }
            Thread.sleep(forTimeInterval: 2)
        }
    }
}
