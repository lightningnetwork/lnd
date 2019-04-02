package build

// DeploymentType is an enum specifying the deployment to compile.
type DeploymentType byte

const (
	// Development is a deployment that includes extra testing hooks and
	// logging configurations.
	Development DeploymentType = iota

	// Production is a deployment that strips out testing logic and uses
	// Default logging.
	Production
)

// String returns a human readable name for a build type.
func (b DeploymentType) String() string {
	switch b {
	case Development:
		return "development"
	case Production:
		return "production"
	default:
		return "unknown"
	}
}

// IsProdBuild returns true if this is a production build.
func IsProdBuild() bool {
	return Deployment == Production
}

// IsDevBuild returns true if this is a development build.
func IsDevBuild() bool {
	return Deployment == Development
}
