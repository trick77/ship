// Package pins holds the tool versions ship runs or demands: one place for check and conform to read.
package pins

const (
	// Golangci is golangci-lint's default version (ship.yaml lint overrides it), built by go run.
	Golangci = "v2.14.0"
	// Govulncheck is the govulncheck version check runs, built by go run.
	Govulncheck = "v1.9.0"
	// Trivy is the ghcr.io/aquasecurity/trivy image tag release scans pushed images with.
	Trivy = "0.75.0"
	// Cosign is the cosign release the Release workflow installs (cosign-installer's cosign-release); a test keeps
	// the workflow on it.
	Cosign = "v3.1.3"
	// GoMin is the lowest Go (major.minor) a repo's go.mod go lines and golang builder may name. A floor, not a pin:
	// Dependabot and go get move a repo up on its own.
	GoMin = "1.27"
	// NodeMin is the lowest Node major a UI's node builder and CI caller may name, and the caller's default.
	NodeMin = "26"
)
