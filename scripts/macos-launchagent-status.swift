import Foundation
import ServiceManagement

guard CommandLine.arguments.count == 2 else {
    FileHandle.standardError.write(Data("usage: macos-launchagent-status.swift <plist-path>\n".utf8))
    exit(64)
}

if #available(macOS 13.0, *) {
    let plistURL = URL(fileURLWithPath: CommandLine.arguments[1])
    var status = SMAppService.statusForLegacyPlist(at: plistURL)

    // Background Task Management discovers newly installed legacy agents
    // asynchronously. Give it a bounded window before declaring the state
    // unverifiable.
    for _ in 0..<20 where status == .notRegistered {
        Thread.sleep(forTimeInterval: 0.25)
        status = SMAppService.statusForLegacyPlist(at: plistURL)
    }

    switch status {
    case .enabled:
        print("enabled")
    case .requiresApproval:
        print("requires-approval")
    case .notRegistered:
        print("not-registered")
    case .notFound:
        print("not-found")
    @unknown default:
        print("unknown-\(status.rawValue)")
    }
} else {
    print("unsupported")
}
