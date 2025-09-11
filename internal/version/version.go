package version

// Build metadata injected via -ldflags at build time.
// Defaults are used when building locally without the script.
var (
    // BuildNumber is a monotonically increasing string set by the build script.
    BuildNumber = "0"
    // GitCommit is the short commit hash if available; may be "unknown".
    GitCommit   = "unknown"
)

// String returns a concise version string for logs/CLI.
func String() string {
    if GitCommit == "unknown" || GitCommit == "" {
        return "build " + BuildNumber
    }
    return "build " + BuildNumber + " (" + GitCommit + ")"
}

