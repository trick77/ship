// Package pins holds the tool versions ship runs or demands: one place for check and conform to read.
package pins

const (
	// Golangci is golangci-lint's default version (ship.yaml lint overrides it), built by go run.
	Golangci = "v2.14.0"
	// Govulncheck is the govulncheck version check runs, built by go run.
	Govulncheck = "v1.9.0"
	// Prettier is the exact devDependencies.prettier a UI's package.json must pin: one formatter version, one
	// formatting, in every repo.
	Prettier = "3.9.9"
)
