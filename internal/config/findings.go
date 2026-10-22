package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// The two levels of a Finding. An ERROR stops the agent starting; a WARNING is
// printed and the agent carries on.
const (
	LevelError   = "ERROR"
	LevelWarning = "WARNING"
)

// Finding is one thing wrong, or worth knowing, about a configuration file.
// Key is the dotted key it concerns (list items as server.tls.allowed_certnames[2]).
type Finding struct {
	Level   string
	Key     string
	Message string
}

// String is the line --check-config prints: "ERROR <key>: <message>".
func (f Finding) String() string { return f.Level + " " + f.Key + ": " + f.Message }

func errorf(key, format string, a ...any) Finding {
	return Finding{Level: LevelError, Key: key, Message: fmt.Sprintf(format, a...)}
}

func warnf(key, format string, a ...any) Finding {
	return Finding{Level: LevelWarning, Key: key, Message: fmt.Sprintf(format, a...)}
}

// HasErrors reports whether any finding is an ERROR.
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == LevelError {
			return true
		}
	}
	return false
}

// dedupe drops a repeated (level, key) pair, keeping the first: a decode error
// and the validation of the zero value it left behind name the same key.
func dedupe(fs []Finding) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, f := range fs {
		k := f.Level + "\x00" + f.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// Report prints the findings one per line, then "config <path>: OK" or
// "config <path>: N error(s), M warning(s)" (PD-12).
func Report(w io.Writer, path string, fs []Finding) {
	var errs, warns int
	for _, f := range fs {
		fmt.Fprintln(w, f)
		if f.Level == LevelError {
			errs++
		} else {
			warns++
		}
	}
	if errs == 0 && warns == 0 {
		fmt.Fprintf(w, "config %s: OK\n", path)
		return
	}
	fmt.Fprintf(w, "config %s: %d error(s), %d warning(s)\n", path, errs, warns)
}

// credentialsDir is where systemd puts LoadCredential= files for a unit.
const credentialsDir = "/run/credentials/"

// CheckFiles warns about each certificate, key, CA, CRL or ssh_config the
// current user cannot read. It is a WARNING, never an ERROR: --check-config
// may run outside the unit, where LoadCredential= files do not exist (PD-13).
// Under serve the TLS setup fails hard instead.
func CheckFiles(c *Config) []Finding {
	files := []struct{ key, path string }{
		{"server.tls.certificate", c.Server.TLS.Certificate},
		{"server.tls.key", c.Server.TLS.Key},
		{"server.tls.client_ca", c.Server.TLS.ClientCA},
		{"server.tls.crl", c.Server.TLS.CRL},
		{"engine.ssh_config", c.Engine.SSHConfig},
	}
	var out []Finding
	for _, f := range files {
		if f.path == "" {
			continue
		}
		fh, err := os.Open(f.path)
		if err == nil {
			_ = fh.Close()
			continue
		}
		reason := err
		var pe *fs.PathError
		if errors.As(err, &pe) {
			reason = pe.Err
		}
		if strings.HasPrefix(f.path, credentialsDir) {
			out = append(out, warnf(f.key, "%s is not readable by this user here (%v); it is a systemd credential, which only the service sees, and the service reads it at start", f.path, reason))
			continue
		}
		out = append(out, warnf(f.key, "%s is not readable by this user here (%v); the service reads it at start", f.path, reason))
	}
	return out
}
