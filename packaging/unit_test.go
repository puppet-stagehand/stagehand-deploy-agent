// Package packaging holds the files the deb and rpm install (the systemd unit,
// the package scripts, the example configuration). It has no code; the tests
// here read those files so a change that weakens the unit or makes a package
// script start the service fails in `go test`, before anyone builds a package.
package packaging

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unit is a parsed systemd unit: section -> key -> every value, in file order.
// systemd allows a key to repeat, so each key keeps a slice.
type unit map[string]map[string][]string

func parseUnit(t *testing.T, path string) unit {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open unit: %v", err)
	}
	defer f.Close()
	u := unit{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if u[section] == nil {
				u[section] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unit line is not key=value: %q", line)
		}
		if u[section] == nil {
			u[section] = map[string][]string{}
		}
		u[section][strings.TrimSpace(k)] = append(u[section][strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	return u
}

const unitPath = "stagehand-deploy-agent.service"

// one returns the single value of a [Service] key, failing when the key is
// absent or repeated.
func (u unit) one(t *testing.T, section, key string) string {
	t.Helper()
	vs, ok := u[section][key]
	if !ok {
		t.Fatalf("[%s] %s= is missing", section, key)
	}
	if len(vs) != 1 {
		t.Fatalf("[%s] %s= appears %d times, want 1", section, key, len(vs))
	}
	return vs[0]
}

func TestUnitRunsAsTheAgentUserInGroupPuppetWithUmask0027(t *testing.T) {
	u := parseUnit(t, unitPath)
	for key, want := range map[string]string{
		"User":  "stagehand-deploy-agent",
		"Group": "puppet",
		// 0027 makes new environment directories 0750 and files 0640, which
		// Puppet Server reads through the puppet group (RESEARCH Pitfall 9).
		"UMask": "0027",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
}

func TestUnitHardening(t *testing.T) {
	u := parseUnit(t, unitPath)
	for key, want := range map[string]string{
		"NoNewPrivileges":         "yes",
		"CapabilityBoundingSet":   "",
		"AmbientCapabilities":     "",
		"ProtectSystem":           "strict",
		"RestrictAddressFamilies": "AF_UNIX AF_INET AF_INET6",
		"KillMode":                "mixed",
		"TimeoutStopSec":          "60",
		"StateDirectory":          "stagehand-deploy-agent",
		"StateDirectoryMode":      "0700",
		"SystemCallFilter":        "@system-service",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
}

func TestUnitReadWritePathsNameTheEnvironmentsAndTheR10kCache(t *testing.T) {
	u := parseUnit(t, unitPath)
	paths := strings.Fields(u.one(t, "Service", "ReadWritePaths"))
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for _, want := range []string{"/etc/puppetlabs/code/environments", "/opt/puppetlabs/puppet/cache/r10k"} {
		if !have[want] {
			t.Errorf("ReadWritePaths=%v does not name %s", paths, want)
		}
	}
	if len(paths) != 2 {
		t.Errorf("ReadWritePaths=%v, want exactly the two default write paths (the state directory comes from StateDirectory=)", paths)
	}
}

func TestUnitStartsTheServeSubcommandWithTheDefaultConfig(t *testing.T) {
	u := parseUnit(t, unitPath)
	const want = "/usr/bin/stagehand-deploy-agent serve --config /etc/stagehand-deploy-agent/config.yml"
	if got := u.one(t, "Service", "ExecStart"); got != want {
		t.Errorf("ExecStart=%q, want %q", got, want)
	}
	if got := u.one(t, "Service", "Type"); got != "exec" {
		t.Errorf("Type=%q, want exec", got)
	}
}

func TestUnitEnvironmentSetsHomeAndForbidsGitPrompts(t *testing.T) {
	u := parseUnit(t, unitPath)
	env := strings.Join(u["Service"]["Environment"], "\n")
	for _, want := range []string{"HOME=/var/lib/stagehand-deploy-agent", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want) {
			t.Errorf("Environment= lacks %s (have %q)", want, env)
		}
	}
}

func TestUnitIsWantedByMultiUser(t *testing.T) {
	u := parseUnit(t, unitPath)
	if got := u.one(t, "Install", "WantedBy"); got != "multi-user.target" {
		t.Errorf("WantedBy=%q, want multi-user.target", got)
	}
}

// What must be absent matters as much as what is present: each of these was
// ruled out for a stated reason (RESEARCH A14, A15, Pitfall 4; PD-24).
func TestUnitOmitsWhatMustNotBeThere(t *testing.T) {
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, banned := range []struct{ token, why string }{
		{"ExecReload", "the agent has no reload; the fork's HUP reload is gone"},
		{"MemoryDenyWriteExecute", "the engine is Ruby (r10k), which needs writable-executable memory (A14)"},
		{"GIT_SSH_COMMAND", "the agent sets it for the engine child from engine.ssh_config"},
		{"LoadCredential", "the certname is per node; the module's drop-in adds the credentials"},
	} {
		if strings.Contains(text, banned.token) {
			t.Errorf("the unit mentions %s: %s", banned.token, banned.why)
		}
	}
}

// runScript runs a package script with stub getent, useradd and systemctl
// first on PATH. Each stub appends its argv to a log file; getent answers from
// the GETENT_GROUP_PUPPET and GETENT_PASSWD_AGENT variables.
func runScript(t *testing.T, script string, env []string, args ...string) (out string, code int, calls []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	stub := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("getent", `echo "getent $*" >> "$CALLS_LOG"
case "$1 $2" in
  "group puppet") [ "${GETENT_GROUP_PUPPET:-yes}" = yes ] && echo "puppet:x:999:" && exit 0 || exit 2 ;;
  "passwd stagehand-deploy-agent") [ "${GETENT_PASSWD_AGENT:-no}" = yes ] && echo "stagehand-deploy-agent:x:998:999::/var/lib/stagehand-deploy-agent:/usr/sbin/nologin" && exit 0 || exit 2 ;;
esac
exit 2
`)
	stub("useradd", `echo "useradd $*" >> "$CALLS_LOG"`+"\n")
	stub("systemctl", `echo "systemctl $*" >> "$CALLS_LOG"`+"\n")

	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "CALLS_LOG=" + log}, env...)
	b, err := cmd.CombinedOutput()
	out = string(b)
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", script, err)
		}
		code = ee.ExitCode()
	}
	if raw, err := os.ReadFile(log); err == nil {
		calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return out, code, calls
}

func contains(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func TestPreinstallCreatesTheUserInGroupPuppet(t *testing.T) {
	_, code, calls := runScript(t, "scripts/preinstall.sh", nil, "install")
	if code != 0 {
		t.Fatalf("exit %d, want 0; calls %v", code, calls)
	}
	want := "useradd --system --gid puppet --home-dir /var/lib/stagehand-deploy-agent --no-create-home --shell /usr/sbin/nologin stagehand-deploy-agent"
	if !contains(calls, want) {
		t.Errorf("useradd was not run as specified.\nwant: %s\ncalls: %v", want, calls)
	}
}

func TestPreinstallLeavesAnExistingUserAlone(t *testing.T) {
	_, code, calls := runScript(t, "scripts/preinstall.sh", []string{"GETENT_PASSWD_AGENT=yes"}, "install")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "useradd") {
			t.Errorf("useradd ran although the user exists: %s", c)
		}
	}
}

func TestPreinstallFailsClearlyWithoutGroupPuppet(t *testing.T) {
	out, code, calls := runScript(t, "scripts/preinstall.sh", []string{"GETENT_GROUP_PUPPET=no"}, "install")
	if code == 0 {
		t.Fatal("preinstall succeeded without the puppet group")
	}
	if !strings.Contains(out, "stagehand-deploy-agent needs the puppet group (install puppetserver first)") {
		t.Errorf("message does not say what to do: %q", out)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "useradd") {
			t.Errorf("a user was created although the group is missing: %s", c)
		}
	}
}

func TestInstallingNeverStartsOrEnablesTheService(t *testing.T) {
	for _, script := range []string{"preinstall", "postinstall", "preremove", "postremove"} {
		raw, err := os.ReadFile("scripts/" + script + ".sh")
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "#") {
				continue
			}
			for _, verb := range []string{"systemctl start", "systemctl enable", "systemctl restart", "systemctl preset", "systemctl reload-or-restart", "systemctl try-restart"} {
				if strings.Contains(l, verb) {
					t.Errorf("%s.sh line %d runs %q: the module (or the operator) starts the service after writing the config", script, i+1, verb)
				}
			}
		}
	}
}

func TestPostinstallOnlyReloadsSystemd(t *testing.T) {
	for _, arg := range []string{"configure", "1", "2"} {
		_, code, calls := runScript(t, "scripts/postinstall.sh", nil, arg)
		if code != 0 {
			t.Fatalf("exit %d for %q", code, arg)
		}
		if len(calls) != 1 || calls[0] != "systemctl daemon-reload" {
			t.Errorf("postinstall %q made calls %v, want only systemctl daemon-reload", arg, calls)
		}
	}
}

func TestPreremoveStopsOnlyOnRemovalNotOnUpgrade(t *testing.T) {
	// deb: "remove" / "upgrade"; rpm: "0" (last package version erased) / "1" (upgrade).
	for _, arg := range []string{"remove", "0"} {
		_, code, calls := runScript(t, "scripts/preremove.sh", nil, arg)
		if code != 0 {
			t.Fatalf("exit %d for %q", code, arg)
		}
		if !contains(calls, "systemctl disable --now stagehand-deploy-agent") {
			t.Errorf("removal (%q) did not disable and stop the service; calls %v", arg, calls)
		}
	}
	for _, arg := range []string{"upgrade", "1", "2", "failed-upgrade", "deconfigure"} {
		_, code, calls := runScript(t, "scripts/preremove.sh", nil, arg)
		if code != 0 {
			t.Fatalf("exit %d for %q", code, arg)
		}
		if len(calls) != 0 {
			t.Errorf("preremove %q touched the service: %v", arg, calls)
		}
	}
}

func TestPostremoveOnlyReloadsSystemd(t *testing.T) {
	_, code, calls := runScript(t, "scripts/postremove.sh", nil, "remove")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(calls) != 1 || calls[0] != "systemctl daemon-reload" {
		t.Errorf("postremove made calls %v, want only systemctl daemon-reload", calls)
	}
}

func TestPackageScriptsHaveNoColourCodes(t *testing.T) {
	for _, script := range []string{"preinstall", "postinstall", "preremove", "postremove"} {
		raw, err := os.ReadFile("scripts/" + script + ".sh")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `\033`) || strings.Contains(string(raw), "\x1b") {
			t.Errorf("%s.sh prints colour escape codes", script)
		}
		if !strings.HasPrefix(string(raw), "#!/bin/sh\n") {
			t.Errorf("%s.sh does not start with #!/bin/sh", script)
		}
	}
}
