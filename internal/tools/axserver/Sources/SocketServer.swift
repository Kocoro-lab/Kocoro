import Darwin
import Dispatch
import Foundation

/// Owns signal delivery and socket teardown. POSIX signals are ignored at the
/// raw handler boundary and delivered by DispatchSourceSignal on an ordinary
/// Swift execution context, where locking, CoreGraphics cleanup, unlink, and
/// process termination are permitted.
final class SocketServerShutdownController {
    private let lock = NSLock()
    private let socketPath: String?
    private let inputGate: InputCommitGateV1
    private let terminate: (Int32) -> Void
    private let queue: DispatchQueue
    private var serverFD: Int32 = -1
    private var clientFD: Int32 = -1
    private var stopped = false
    private var signalSources: [DispatchSourceSignal] = []

    init(
        socketPath: String?,
        inputGate: InputCommitGateV1 = processInputCommitGateV1,
        queue: DispatchQueue = DispatchQueue(
            label: "run.shannon.kocoro.ax-server.signal-cleanup",
            qos: .userInitiated),
        terminate: @escaping (Int32) -> Void = { Darwin.exit($0 == 0 ? 0 : 128 + $0) }
    ) {
        self.socketPath = socketPath
        self.inputGate = inputGate
        self.queue = queue
        self.terminate = terminate
    }

    func installSignalSources() {
        // No Swift closure is installed as a raw POSIX handler. SIG_IGN itself
        // is async-signal-safe and lets Dispatch own delivery of both signals.
        Darwin.signal(SIGINT, SIG_IGN)
        Darwin.signal(SIGTERM, SIG_IGN)
        signalSources = [SIGINT, SIGTERM].map { number in
            let source = DispatchSource.makeSignalSource(signal: number, queue: queue)
            source.setEventHandler { [weak self] in self?.stop(signal: number, terminate: true) }
            source.resume()
            return source
        }
    }

    func setServerFD(_ fd: Int32) {
        lock.lock()
        serverFD = fd
        lock.unlock()
    }

    func setClientFD(_ fd: Int32) {
        lock.lock()
        clientFD = fd
        lock.unlock()
    }

    func stop(signal: Int32, terminate shouldTerminate: Bool) {
        lock.lock()
        guard !stopped else { lock.unlock(); return }
        stopped = true
        let exactClientFD = clientFD
        let exactServerFD = serverFD
        clientFD = -1
        serverFD = -1
        lock.unlock()

        // Flip the input gate before touching transport state. This waits for
        // an in-flight sample, posts registered releases, and prevents any
        // later down/sample from committing in this process.
        _ = inputGate.shutdownForSignal(signal)
        if exactClientFD >= 0 {
            _ = Darwin.shutdown(exactClientFD, SHUT_RDWR)
            _ = Darwin.close(exactClientFD)
        }
        if exactServerFD >= 0 {
            _ = Darwin.shutdown(exactServerFD, SHUT_RDWR)
            _ = Darwin.close(exactServerFD)
        }
        if let socketPath { _ = Darwin.unlink(socketPath) }
        if shouldTerminate { terminate(signal) }
    }
}

/// Runs ax_server as a persistent Unix socket server.
/// Same NDJSON protocol as the stdin/stdout mode — one JSON request per line,
/// one JSON response per line. Accepts one client and exits on disconnect.
func runSocketServer(path socketPath: String) {
    _ = unlink(socketPath)

    // This is the restart recovery boundary for the stable TCC helper. It runs
    // before the socket is made ready; malformed or unconfirmed recovery keeps
    // the input gate fail-closed while read-only diagnostics remain available.
    _ = processInputCommitGateV1.recoverAtStartup()
    let shutdownController = SocketServerShutdownController(socketPath: socketPath)
    shutdownController.installSignalSources()

    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else {
        FileHandle.standardError.write("ax_server: failed to create socket\n".data(using: .utf8)!)
        exit(1)
    }
    shutdownController.setServerFD(fd)

    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let pathBytes = socketPath.utf8CString
    guard pathBytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else {
        FileHandle.standardError.write("ax_server: socket path too long\n".data(using: .utf8)!)
        shutdownController.stop(signal: 0, terminate: false)
        exit(1)
    }
    withUnsafeMutablePointer(to: &addr.sun_path) { sunPath in
        sunPath.withMemoryRebound(to: CChar.self, capacity: pathBytes.count) { dst in
            for i in 0..<pathBytes.count { dst[i] = pathBytes[i] }
        }
    }

    let bindResult = withUnsafePointer(to: &addr) { ptr in
        ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockPtr in
            bind(fd, sockPtr, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    guard bindResult == 0 else {
        FileHandle.standardError.write(
            "ax_server: bind failed: \(String(cString: strerror(errno)))\n".data(using: .utf8)!)
        shutdownController.stop(signal: 0, terminate: false)
        exit(1)
    }
    guard listen(fd, 1) == 0 else {
        FileHandle.standardError.write("ax_server: listen failed\n".data(using: .utf8)!)
        shutdownController.stop(signal: 0, terminate: false)
        exit(1)
    }

    print("ready")
    fflush(stdout)

    let enc = JSONEncoder()
    enc.outputFormatting = [.sortedKeys]
    let dec = JSONDecoder()
    let clientFD = accept(fd, nil, nil)
    guard clientFD >= 0 else {
        shutdownController.stop(signal: 0, terminate: false)
        return
    }
    shutdownController.setClientFD(clientFD)

    let input = FileHandle(fileDescriptor: clientFD, closeOnDealloc: false)
    let output = FileHandle(fileDescriptor: clientFD, closeOnDealloc: false)
    handleClient(input: input, output: output, encoder: enc, decoder: dec)
    shutdownController.stop(signal: 0, terminate: false)
}

private func handleClient(
    input: FileHandle,
    output: FileHandle,
    encoder: JSONEncoder,
    decoder: JSONDecoder
) {
    var buffer = Data()
    while true {
        let chunk = input.availableData
        if chunk.isEmpty { break }
        buffer.append(chunk)
        while let newlineIndex = buffer.firstIndex(of: UInt8(ascii: "\n")) {
            let lineData = buffer[buffer.startIndex..<newlineIndex]
            buffer = buffer[buffer.index(after: newlineIndex)...]
            guard !lineData.isEmpty else { continue }
            writeToHandle(
                dispatchWireRequest(Data(lineData), decoder: decoder),
                encoder: encoder,
                output: output)
        }
    }
}

private func writeToHandle(_ resp: Response, encoder: JSONEncoder, output: FileHandle) {
    guard let data = try? encoder.encode(resp),
          var str = String(data: data, encoding: .utf8) else { return }
    str += "\n"
    if let bytes = str.data(using: .utf8) { output.write(bytes) }
}
