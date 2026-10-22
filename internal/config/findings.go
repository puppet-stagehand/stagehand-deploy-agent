package config

import "io"

// RED-stage stubs: signatures only, so the tests compile and fail on their
// assertions. Replaced in the GREEN commit.

const (
	LevelError   = "ERROR"
	LevelWarning = "WARNING"
)

type Finding struct{ Level, Key, Message string }

func HasErrors(fs []Finding) bool { return false }

func Report(w io.Writer, path string, fs []Finding) {}

func Load(path string, legacy bool) (*Config, []Finding) {
	c, err := loadOld(path)
	if err != nil {
		return nil, []Finding{{LevelError, "config", err.Error()}}
	}
	return c, nil
}

func Parse(b []byte, name string, legacy bool) (*Config, []Finding) { return nil, nil }

func Locate(explicit, defaultPath, legacyPath string) (string, bool, error) { return "", false, nil }

func CheckFiles(c *Config) []Finding { return nil }

func SchemaKeys() []string { return nil }
