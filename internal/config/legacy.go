package config

// LegacyPath is the fork's configuration file (D-15).
const LegacyPath = "/etc/voxpupuli/webhook.yml"

// LegacyFallbackRemovedIn is the agent release that stops reading LegacyPath.
const LegacyFallbackRemovedIn = "1.2.0"

// parseLegacy is replaced by the legacy mapper in the next commit.
func parseLegacy(b []byte, name string) (*Config, []Finding) {
	return nil, []Finding{errorf("config", "%s: the legacy mapper is not built yet", name)}
}
