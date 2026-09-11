package version

const (
	// Name for this.
	Name = "ddflagd"
	// Version for this. tagpr keeps it in step with the git tag.
	Version = "0.0.2" //nostyle:repetition
)

// Revision is the commit the binary was built from, set at build time.
var Revision = "HEAD"
