// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Iris",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "Iris",
            path: "Sources/Iris",
            linkerSettings: [.linkedFramework("LocalAuthentication"), .linkedFramework("ServiceManagement")]
        )
    ]
)
