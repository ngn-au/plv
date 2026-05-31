package main

// Build identity surfaced in the UI — running version + canonical links. Mirrors the
// PowerDNS-AuthAdmin web UI's lib/app-meta.ts: appVersion is the single source of truth
// for the semver, so cutting a release is a one-constant bump (plus the matching tag).
//
// Three build kinds, distinguished by the ldflags the binary carries:
//
//   - release — built from a `vX.Y.Z` tag (CI injects -X main.releaseBuild=true).
//     Chip shows "1.0.2"; links point at the GitHub release + the tag's docs.
//   - commit  — non-release build past the last tag (every push to main / edge image).
//     CI injects the short SHA (-X main.gitSha=abc1234), so the chip shows "1.0.2+abc1234"
//     and the links resolve to that exact commit — never claiming to be a release.
//   - dev     — no VCS info at all: a plain `go build`/`go run`, or a Docker build with no
//     build-args. The running code isn't on the remote at any known ref, so the chip is
//     "1.0.2-dev" and links point at `main` rather than invent a 404-ing commit URL.

// appVersion is the canonical semver of this codebase — the single source of truth, bumped
// when cutting a release. The matching git tag is "v" + appVersion.
const appVersion = "1.0.2"

// repoHomepage is the canonical GitHub repo the chip links into.
const repoHomepage = "https://github.com/ngn-au/plv"

// Build provenance, injected via -ldflags at build time (both empty for a local dev build):
//
//	-X main.gitSha=<short-sha>     on every non-release image (commit builds)
//	-X main.releaseBuild=true      only on a build from a vX.Y.Z tag
var (
	gitSha       = ""
	releaseBuild = ""
)

// buildKind is one of "release", "commit", "dev".
func buildKind() string {
	switch {
	case releaseBuild == "true":
		return "release"
	case gitSha != "":
		return "commit"
	default:
		return "dev"
	}
}

// versionLabel is the display label, without a leading "v":
//
//	release → "1.0.2"; commit → "1.0.2+abc1234"; dev → "1.0.2-dev".
func versionLabel() string {
	switch buildKind() {
	case "release":
		return appVersion
	case "commit":
		return appVersion + "+" + gitSha
	default:
		return appVersion + "-dev"
	}
}

// sourceURL is where the version chip links: the release page for a release, the exact
// commit for a commit build, or `main` for a dev build (which isn't at any known ref).
func sourceURL() string {
	switch buildKind() {
	case "release":
		return repoHomepage + "/releases/tag/v" + appVersion
	case "commit":
		return repoHomepage + "/commit/" + gitSha
	default:
		return repoHomepage + "/tree/main"
	}
}

// docsURL points at the docs that match THIS build, so the link always resolves to docs
// that exist for the running code (the tag's docs for a release, the commit's for a commit
// build, main's for a dev build).
func docsURL() string {
	switch buildKind() {
	case "release":
		return repoHomepage + "/tree/v" + appVersion + "/docs"
	case "commit":
		return repoHomepage + "/tree/" + gitSha + "/docs"
	default:
		return repoHomepage + "/tree/main/docs"
	}
}

// sourceTitle is the chip's hover title, phrased to match its link target.
func sourceTitle() string {
	switch buildKind() {
	case "release":
		return "PLV v" + versionLabel() + " — view this release on GitHub"
	case "commit":
		return "PLV v" + versionLabel() + " — view this commit on GitHub"
	default:
		return "PLV v" + versionLabel() + " — local build, view the main branch on GitHub"
	}
}
