// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Iris",
    platforms: [.macOS(.v13)],
    dependencies: [
        // Auto-update (EdDSA-signed appcast on the GitHub release).
        .package(url: "https://github.com/sparkle-project/Sparkle", .upToNextMajor(from: "2.6.0")),
    ],
    targets: [
        .executableTarget(
            name: "Iris",
            dependencies: [.product(name: "Sparkle", package: "Sparkle")],
            path: "Sources/Iris",
            linkerSettings: [
                .linkedFramework("LocalAuthentication"),
                .linkedFramework("LocalAuthenticationEmbeddedUI"),
                .linkedFramework("ServiceManagement"),
                // Sparkle.framework ships in Iris.app/Contents/Frameworks.
                .unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"]),
            ]
        )
    ]
)
