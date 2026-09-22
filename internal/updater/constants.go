package updater

// BuildVersion is replaced by the release workflow using -ldflags. Development
// builds deliberately identify themselves as dev and cannot masquerade as a
// published Nexus release.
var BuildVersion = "dev"

// Operation is the closed set of actions exposed by the privileged boundary.
type Operation string

const (
	OperationGetStatus        Operation = "GetStatus"
	OperationCheckUpdates     Operation = "CheckUpdates"
	OperationDoctor           Operation = "Doctor"
	OperationUpdate           Operation = "Update"
	OperationRollback         Operation = "Rollback"
	OperationCleanup          Operation = "Cleanup"
	OperationGetOperation     Operation = "GetOperation"
	OperationListComponents   Operation = "ListComponents"
	OperationInstallComponent Operation = "InstallComponent"
	OperationEnableComponent  Operation = "EnableComponent"
	OperationDisableComponent Operation = "DisableComponent"
	OperationRestartComponent Operation = "RestartComponent"
)

// Component is the closed set of first-party components. A caller cannot
// supply a service name or select a third-party component.
type Component string

const (
	ComponentCore    Component = "nexus-core"
	ComponentSubs    Component = "nexus-subs"
	ComponentDiscord Component = "nexus-discord"
)

// Profile is the closed set of installer topologies.
type Profile string

const (
	ProfileFull               Profile = "full"
	ProfileAppWebSubsRemoteDB Profile = "app-web-subs-remote-db"
	ProfileWebOnly            Profile = "web-only"
	ProfileDatabaseOnly       Profile = "db-only"
	ProfileSubsOnly           Profile = "subs-only"
)

const (
	ProtocolVersion       uint16 = 1
	MaxFrameSize                 = 64 * 1024
	MaxResponseSize              = 1024 * 1024
	MaxTransportFrameSize        = 4 * 1024 * 1024
)

func validOperation(operation Operation) bool {
	switch operation {
	case OperationGetStatus, OperationCheckUpdates, OperationDoctor,
		OperationUpdate, OperationRollback, OperationCleanup, OperationGetOperation,
		OperationListComponents, OperationInstallComponent, OperationEnableComponent,
		OperationDisableComponent, OperationRestartComponent:
		return true
	default:
		return false
	}
}

func validComponent(component Component) bool {
	switch component {
	case ComponentCore, ComponentSubs, ComponentDiscord:
		return true
	default:
		return false
	}
}

func validProfile(profile Profile) bool {
	switch profile {
	case ProfileFull, ProfileAppWebSubsRemoteDB, ProfileWebOnly, ProfileDatabaseOnly, ProfileSubsOnly:
		return true
	default:
		return false
	}
}

func componentOperation(operation Operation) bool {
	switch operation {
	case OperationInstallComponent, OperationEnableComponent, OperationDisableComponent,
		OperationRestartComponent:
		return true
	default:
		return false
	}
}

func mutatingOperation(operation Operation) bool {
	switch operation {
	case OperationUpdate, OperationRollback, OperationCleanup, OperationInstallComponent,
		OperationEnableComponent, OperationDisableComponent, OperationRestartComponent:
		return true
	default:
		return false
	}
}
