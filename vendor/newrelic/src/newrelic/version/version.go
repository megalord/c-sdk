package version

var (
	// Populated at build time. See Makefile for details.

	Number = "unreleased"
	Commit = ""
)

func Full() string {
	switch {
	case len(Commit) > 12:
		return Number + "-" + Commit[:12]
	case len(Commit) > 0:
		return Number + "-" + Commit
	default:
		return Number
	}
}
