// Package capturemode resolves whether a command captures locally or in Grafana Cloud.
package capturemode

import "github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"

// The launcher exports the resolved destination and its reason so doctor can
// report them from inside the launched agent. LaunchDisabledEnv also stops the
// child from exporting through a partial Cloud config after the local receiver
// failed to start.
const (
	LaunchDestinationEnv = "AGENTO11Y_LAUNCH_CAPTURE_DESTINATION"
	LaunchReasonEnv      = "AGENTO11Y_LAUNCH_CAPTURE_REASON"
	LaunchDisabledEnv    = "AGENTO11Y_LAUNCH_CAPTURE_DISABLED"
)

// Flag records an explicit launcher destination flag.
type Flag int

const (
	FlagUnset Flag = iota
	FlagLocal
	FlagNoLocal
)

// Source identifies the rule that selected a capture destination.
type Source int

const (
	SourceFlag Source = iota
	SourceEnv
	SourceUnsupported
	SourceCredentials
	SourceDefault
)

// Request contains the inputs used to resolve a capture destination.
type Request struct {
	Flag            Flag
	EnvValue        string
	EnvKey          string
	HasCloudCreds   bool
	DaemonSupported bool
}

// Mode is the resolved capture destination and the rule that selected it.
type Mode struct {
	Local  bool
	Source Source
	EnvKey string
}

// Resolve selects the capture destination in precedence order.
func Resolve(r Request) Mode {
	switch r.Flag {
	case FlagUnset:
	case FlagNoLocal:
		return Mode{Source: SourceFlag}
	case FlagLocal:
		return Mode{Local: true, Source: SourceFlag}
	}

	if value, ok := envconfig.ParseBoolValue(r.EnvValue); ok {
		return Mode{Local: value, Source: SourceEnv, EnvKey: r.EnvKey}
	}
	if !r.DaemonSupported {
		return Mode{Source: SourceUnsupported}
	}
	if r.HasCloudCreds {
		return Mode{Source: SourceCredentials}
	}
	return Mode{Local: true, Source: SourceDefault}
}
