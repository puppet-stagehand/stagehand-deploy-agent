package config

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a time.Duration written as a string with a unit ("15m"). A bare
// number is refused: upstream's viper read 900 as 900 nanoseconds, so a
// deploy timeout written as 900 stopped every deploy at once (FORK-IN-01).
type Duration time.Duration

// MinDuration is the shortest duration the agent accepts for a timeout.
const MinDuration = time.Second

// ParseDurationNode reads a YAML scalar as a duration. It is shared by the
// native schema and the legacy mapper so both refuse a number the same way.
func ParseDurationNode(n *yaml.Node) (Duration, error) {
	if n.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("a duration must be a string with a unit, for example 15m")
	}
	if n.ShortTag() != "!!str" {
		return 0, fmt.Errorf("%s has no unit; use a unit, for example 15m or 90s (a bare number is not a duration)", n.Value)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration; use a unit, for example 15m or 90s", n.Value)
	}
	if v < MinDuration {
		return 0, fmt.Errorf("%q is too short; a duration must be at least 1s", n.Value)
	}
	return Duration(v), nil
}

// UnmarshalYAML parses a scalar such as 15m or 90s. The error is a
// *yaml.TypeError so decoding carries on and reports every mistake at once.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDurationNode(n)
	if err != nil {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %s", n.Line, err)}}
	}
	*d = v
	return nil
}

// MarshalYAML writes the duration back as a string with a unit.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }
