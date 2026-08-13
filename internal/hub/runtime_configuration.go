package hub

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const (
	RuntimeAuthConsole = "console"
	RuntimeAuthCloud   = "cloud"
	RuntimeAuthGateway = "gateway"
)

type RuntimeAuthentication struct {
	Category string `json:"category"`
	Source   string `json:"source"`
}

type RuntimeConfiguration struct {
	Configured     bool                  `json:"configured"`
	SettingSources []string              `json:"settingSources"`
	Authentication RuntimeAuthentication `json:"authentication,omitempty"`
}

type RuntimeAuthenticationEvidence struct {
	Category   string                               `json:"category"`
	Source     string                               `json:"source"`
	Validation string                               `json:"validation"`
	Evidence   []runtimecontract.CapabilityEvidence `json:"evidence,omitempty"`
}

type RuntimeConfigurationEvidence struct {
	SettingSources []string                      `json:"settingSources"`
	Authentication RuntimeAuthenticationEvidence `json:"authentication"`
}

type runtimeConfigurationEvidenceProvider interface {
	RuntimeConfigurationEvidence() (RuntimeConfigurationEvidence, bool)
}

func legacyClaudeRuntimeConfiguration() RuntimeConfiguration {
	return RuntimeConfiguration{
		Configured:     true,
		SettingSources: []string{"user", "project", "local"},
		Authentication: RuntimeAuthentication{Category: RuntimeAuthConsole, Source: "api_key"},
	}
}

func validateRuntimeConfigurationEvidence(configuration RuntimeConfiguration, evidence RuntimeConfigurationEvidence) error {
	if !configuration.Configured {
		return fmt.Errorf("Claude Runtime owner configuration is not configured")
	}
	if evidence.Authentication.Validation != "accepted" {
		return fmt.Errorf("Claude Runtime authentication was not accepted")
	}
	if evidence.Authentication.Category != configuration.Authentication.Category || evidence.Authentication.Source != configuration.Authentication.Source {
		return fmt.Errorf("Claude Runtime authentication evidence does not match the selected configuration")
	}
	if strings.Join(evidence.SettingSources, "\x00") != strings.Join(configuration.SettingSources, "\x00") {
		return fmt.Errorf("Claude Runtime setting source evidence does not match the selected configuration")
	}
	return nil
}

func normalizeRuntimeConfiguration(runtimeKind string, configuration RuntimeConfiguration) (RuntimeConfiguration, error) {
	if runtimeKind != "claude" {
		if configuration.Configured || len(configuration.SettingSources) != 0 || configuration.Authentication.Category != "" || configuration.Authentication.Source != "" {
			return RuntimeConfiguration{}, errf(400, "this Runtime does not accept Claude setting or authentication configuration")
		}
		return RuntimeConfiguration{}, nil
	}
	if !configuration.Configured {
		return RuntimeConfiguration{}, errf(400, "Claude Runtime configuration must explicitly select settingSources and authentication")
	}
	sources := configuration.SettingSources
	if len(sources) == 0 {
		return RuntimeConfiguration{}, errf(400, "Claude settingSources must select at least one source")
	}
	seen := map[string]bool{}
	for _, source := range sources {
		source = strings.TrimSpace(source)
		if source != "user" && source != "project" && source != "local" {
			return RuntimeConfiguration{}, errf(400, "Claude settingSources must contain only user, project, or local")
		}
		if seen[source] {
			return RuntimeConfiguration{}, errf(400, "Claude settingSources contains duplicate %q", source)
		}
		seen[source] = true
	}
	sources = append([]string(nil), sources...)
	sort.SliceStable(sources, func(i, j int) bool {
		order := map[string]int{"user": 0, "project": 1, "local": 2}
		return order[sources[i]] < order[sources[j]]
	})
	auth := configuration.Authentication
	auth.Category, auth.Source = strings.TrimSpace(auth.Category), strings.TrimSpace(auth.Source)
	switch auth.Category {
	case RuntimeAuthConsole:
		if auth.Source != "api_key" {
			return RuntimeConfiguration{}, errf(400, "Claude Console authentication source must be api_key")
		}
	case RuntimeAuthCloud:
		switch auth.Source {
		case "bedrock", "vertex", "foundry", "anthropic_aws", "anthropic_google_cloud", "mantle":
		default:
			return RuntimeConfiguration{}, errf(400, "Claude cloud authentication source is unsupported")
		}
	case RuntimeAuthGateway:
		if auth.Source != "gateway" {
			return RuntimeConfiguration{}, errf(400, "Claude gateway authentication source must be gateway")
		}
	default:
		return RuntimeConfiguration{}, errf(400, "Claude authentication category must be console, cloud, or gateway")
	}
	return RuntimeConfiguration{Configured: true, SettingSources: sources, Authentication: auth}, nil
}
