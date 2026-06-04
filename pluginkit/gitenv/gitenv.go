// Package gitenv owns the git/SSH isolation every git-driving plugin in
// this repo uses: the instance config shape (identity, fork template,
// SSH key, env vars), its validation, and the per-run environment that
// points git - and anything git-shaped `go` spawns - at the instance's
// key instead of the host's ~/.ssh and ~/.gitconfig.
//
// Git auth is SSH-only and entirely instance-controlled. The plugin
// writes the configured key + known_hosts into a per-run temp dir and
// points git at them via GIT_SSH_COMMAND. GIT_CONFIG_GLOBAL/SYSTEM=/dev/null
// and GIT_TERMINAL_PROMPT=0 ensure no host config bleeds in and no
// command ever blocks on a TTY prompt.
package gitenv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// InstanceConfig is the shape the plugins' instance-config MFE produces.
// Opaque to the orchestrator. Adding fields here requires no
// orchestrator change.
type InstanceConfig struct {
	GitUserName  string `json:"git_user_name"`
	GitUserEmail string `json:"git_user_email"`
	// ForkURLTemplate is a string with a single {repo} placeholder that
	// the plugin substitutes with the upstream repo's basename to
	// derive the push target. Example:
	//   git@github.com:acme-bot/{repo}.git
	// produces git@github.com:acme-bot/lib-core.git for upstream
	// https://github.com/your-org/lib-core. Per-step push_url overrides
	// this. Either the template or per-step push_url must be set.
	ForkURLTemplate    string   `json:"fork_url_template"`
	DefaultMakeTargets []string `json:"default_make_targets"`
	// SSHPrivateKey is the PEM-encoded private key the plugin uses for
	// every git operation. Written to a per-run temp file at mode 0600
	// and pointed to via GIT_SSH_COMMAND. Required.
	SSHPrivateKey string `json:"ssh_private_key"`
	// SSHKnownHosts is the contents of a known_hosts file. When set,
	// host key verification is strict (StrictHostKeyChecking=yes). When
	// empty, the plugin uses accept-new, which writes any new host key
	// to a per-run scratch file (forgotten when the run ends). For
	// production-grade safety against MITM, populate this.
	SSHKnownHosts string `json:"ssh_known_hosts"`
	// EnvVars are extra environment variables set for every git / go /
	// go mod vendor / make invocation the plugin runs. Merged on top of
	// os.Environ() and the plugin's own protected overrides - user-
	// supplied entries whose keys collide with the protected set (see
	// ProtectedEnvKeys) are rejected at validate time so instance
	// authors can't silently disable the plugin's isolation.
	EnvVars map[string]string `json:"env_vars,omitempty"`
}

// ValidateInstance returns human-readable problems with cfg (empty when
// valid). The caller handles JSON decoding so it can report decode
// errors in its own words.
func ValidateInstance(cfg InstanceConfig) []string {
	var errs []string
	if cfg.GitUserName == "" {
		errs = append(errs, "git_user_name is required")
	}
	if cfg.GitUserEmail == "" {
		errs = append(errs, "git_user_email is required")
	}
	if !strings.Contains(cfg.GitUserEmail, "@") {
		errs = append(errs, "git_user_email must look like an email")
	}
	if cfg.SSHPrivateKey == "" {
		errs = append(errs, "ssh_private_key is required (host keys are intentionally not used)")
	} else if keyErr := CheckPrivateKeyShape(cfg.SSHPrivateKey); keyErr != "" {
		errs = append(errs, "ssh_private_key "+keyErr)
	}
	if cfg.ForkURLTemplate != "" && !strings.Contains(cfg.ForkURLTemplate, "{repo}") {
		errs = append(errs, "fork_url_template must contain the {repo} placeholder")
	}
	for k := range cfg.EnvVars {
		if k == "" {
			errs = append(errs, "env_vars contains an empty key")
			continue
		}
		if !IsValidEnvVarName(k) {
			errs = append(errs, fmt.Sprintf("env_vars key %q is not a valid POSIX-style identifier", k))
			continue
		}
		if _, protected := ProtectedEnvKeys[k]; protected {
			errs = append(errs, fmt.Sprintf("env_vars key %q is reserved by the plugin's isolation defaults and cannot be overridden", k))
		}
	}
	return errs
}

// CheckForExecute re-validates the parts of cfg Execute depends on. The
// orchestrator still saves an instance when the plugin's Validate call
// itself errors, so Execute can't assume the config was checked; this
// surfaces a clear error instead of git's eventual "Load key … :
// invalid format".
func CheckForExecute(instanceID string, cfg InstanceConfig) error {
	if cfg.GitUserName == "" || cfg.GitUserEmail == "" {
		return fmt.Errorf("instance %q is missing git_user_name or git_user_email", instanceID)
	}
	if cfg.SSHPrivateKey == "" {
		return fmt.Errorf("instance %q is missing ssh_private_key", instanceID)
	}
	if keyErr := CheckPrivateKeyShape(cfg.SSHPrivateKey); keyErr != "" {
		return fmt.Errorf("instance %q ssh_private_key %s", instanceID, keyErr)
	}
	return nil
}

// ProtectedEnvKeys are environment variables the plugin controls itself
// to keep its behaviour reproducible and independent of the host.
// Letting an instance override them would silently reintroduce the
// leaks they exist to prevent (host ~/.gitconfig, host ~/.ssh, module
// proxy caching of private forks, TTY prompts on `git push`, etc.).
var ProtectedEnvKeys = map[string]struct{}{
	"GIT_SSH_COMMAND":     {},
	"GIT_TERMINAL_PROMPT": {},
	"GIT_CONFIG_GLOBAL":   {},
	"GIT_CONFIG_SYSTEM":   {},
	"GIT_CONFIG_COUNT":    {},
	"GCM_INTERACTIVE":     {},
	"HOME":                {},
	"GOPATH":              {},
	"GONOSUMDB":           {},
	"GOSUMDB":             {},
	"GOFLAGS":             {},
}

// IsValidEnvVarName reports whether s looks like a POSIX shell env var
// name (leading letter or underscore, then letters/digits/underscores).
// Anything looser risks producing malformed KEY=VALUE lines when we
// exec, which would surface as opaque cmd/exec errors.
func IsValidEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// CheckModuleDir enforces the two rules that keep a dir path from
// escaping the cloned repo tree: no absolute paths, no ".." segments.
// Returns "" when the path is safe to Join under repoDir.
func CheckModuleDir(p string) string {
	if p == "" {
		return "must be non-empty (use \".\" for the repo root)"
	}
	if filepath.IsAbs(p) {
		return "must be relative to the repo root, not absolute"
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "must not contain \"..\" - dir must stay inside the repo"
	}
	return ""
}

// GitURLToModulePath translates the common git URL shapes into the
// module path Go uses to reference that repo.
//
//	https://github.com/org/repo         -> github.com/org/repo
//	https://github.com/org/repo.git     -> github.com/org/repo
//	git@github.com:org/repo.git         -> github.com/org/repo
//	ssh://git@github.com/org/repo.git   -> github.com/org/repo
func GitURLToModulePath(url string) (string, error) {
	s := strings.TrimSuffix(url, ".git")
	if strings.HasPrefix(s, "git@") {
		rest := strings.TrimPrefix(s, "git@")
		colon := strings.IndexByte(rest, ':')
		if colon < 0 || colon == len(rest)-1 {
			return "", fmt.Errorf("git URL %q missing host:path separator", url)
		}
		return rest[:colon] + "/" + rest[colon+1:], nil
	}
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		if strings.HasPrefix(s, prefix) {
			rest := strings.TrimPrefix(s, prefix)
			if at := strings.IndexByte(rest, '@'); at >= 0 {
				rest = rest[at+1:]
			}
			if slash := strings.IndexByte(rest, '/'); slash < 0 {
				return "", fmt.Errorf("git URL %q missing path", url)
			}
			return rest, nil
		}
	}
	return "", fmt.Errorf("unrecognised git URL scheme %q", url)
}

// GitHost extracts the hostname from a git URL. Empty string if the
// URL shape isn't recognised; callers should filter empties out.
func GitHost(url string) string {
	s := strings.TrimSuffix(url, ".git")
	if strings.HasPrefix(s, "git@") {
		rest := strings.TrimPrefix(s, "git@")
		if colon := strings.IndexByte(rest, ':'); colon > 0 {
			return rest[:colon]
		}
		return ""
	}
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		if strings.HasPrefix(s, prefix) {
			rest := strings.TrimPrefix(s, prefix)
			if at := strings.IndexByte(rest, '@'); at >= 0 {
				rest = rest[at+1:]
			}
			if slash := strings.IndexByte(rest, '/'); slash > 0 {
				return rest[:slash]
			}
			return rest
		}
	}
	return ""
}

// ResolvePushURL picks an explicit push_url over a derived one from the
// instance's template. Returns a clear error if neither is set.
func ResolvePushURL(pushURL, cloneURL, template string) (string, error) {
	if pushURL != "" {
		return pushURL, nil
	}
	if template == "" {
		return "", fmt.Errorf("no push_url set on the step and no fork_url_template on the instance - cannot determine where to push")
	}
	return strings.ReplaceAll(template, "{repo}", RepoBasename(cloneURL)), nil
}

// RepoBasename extracts the trailing repo name from any common git URL
// shape so it can be substituted into a fork URL template.
//
//	https://github.com/org/repo        -> repo
//	https://github.com/org/repo.git    -> repo
//	git@github.com:org/repo.git        -> repo
func RepoBasename(url string) string {
	s := strings.TrimSuffix(url, ".git")
	return path.Base(s)
}

// RepoDirFor returns where a clone of cloneURL lives inside gopathDir:
// $GOPATH/src/<module-path-of-clone-url>, falling back to
// $GOPATH/src/local/repo for URL shapes it can't parse. Plenty of
// real-world Makefiles / go:generate directives (especially in
// pre-modules or vendored repos) resolve tool and import paths by
// walking up to $GOPATH/src, and fail with confusing errors when the
// working copy isn't actually there. Go's own module tooling doesn't
// care where the clone lives, so this costs nothing for repos that
// don't need it.
func RepoDirFor(gopathDir, cloneURL string) string {
	if modulePath, err := GitURLToModulePath(cloneURL); err == nil && modulePath != "" {
		return filepath.Join(gopathDir, "src", filepath.FromSlash(modulePath))
	}
	return filepath.Join(gopathDir, "src", "local", "repo")
}

// BuildGitEnv materialises the instance's SSH credentials into the
// per-run temp dir and returns an environment that points git (and any
// git invocations made by `go get`/`go mod tidy`) at them.
//
// We deliberately bypass the host's gitconfig and ssh config so the
// plugin's behaviour is reproducible regardless of who's running it.
//
// gitHosts is the list of hostnames (github.com, gitlab.company.net,
// etc.) we need SSH-based auth for. For each host we install a
// `insteadOf` rewrite that turns `https://<host>/…` URLs into
// `git@<host>:…` so `go mod tidy`'s internal `git clone` calls use SSH
// and pick up our configured key - critical when a `replace` directive
// points at a private fork under a host the Go module proxy has
// never seen.
func BuildGitEnv(dir, gopathDir string, cfg InstanceConfig, gitHosts []string) ([]string, error) {
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ssh dir: %w", err)
	}
	keyPath := filepath.Join(sshDir, "id")
	keyBytes := NormalizePEM(cfg.SSHPrivateKey)
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write ssh key: %w", err)
	}
	knownHostsPath := filepath.Join(sshDir, "known_hosts")
	if err := os.WriteFile(knownHostsPath, []byte(cfg.SSHKnownHosts), 0o600); err != nil {
		return nil, fmt.Errorf("write known_hosts: %w", err)
	}

	hostCheck := "accept-new"
	if cfg.SSHKnownHosts != "" {
		hostCheck = "yes"
	}
	sshCmd := fmt.Sprintf(
		"ssh -i %s -o UserKnownHostsFile=%s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=%s",
		keyPath, knownHostsPath, hostCheck,
	)

	env := append([]string{}, os.Environ()...)
	// Overrides - these intentionally clobber anything inherited.
	overrides := map[string]string{
		"GIT_SSH_COMMAND":     sshCmd,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_SYSTEM":   "/dev/null",
		"GCM_INTERACTIVE":     "never", // Git Credential Manager, if installed
		"HOME":                dir,     // last-resort isolation from host ~/.gitconfig + ~/.ssh
		// Explicit rather than relying on the $HOME-derived default so
		// make targets / go:generate directives that read $GOPATH behave
		// the same regardless of how the go toolchain computes its
		// default. Also means the module cache (GOPATH/pkg/mod) lives at
		// a path we know, so cleanup can find and unlock it (see
		// RemoveAllWritable).
		"GOPATH": gopathDir,
		// Skip the checksum DB for all modules - private fork pseudo-versions
		// aren't in sum.golang.org and would cause `go mod tidy` to fail.
		// We do NOT set GOPRIVATE=* (which would also set GONOPROXY=*) because
		// that forces Go to bypass proxy.golang.org for public modules too,
		// causing direct fetches of vanity URLs (e.g. go.opencensus.io) that
		// may be unreachable from inside the pipeline container. The SSH
		// insteadOf rewrites installed above already redirect private-fork
		// https:// URLs to git@host: so the proxy and SSH can coexist.
		"GONOSUMDB": "*",
		"GOSUMDB":   "off",
		"GOFLAGS":   "-mod=mod",
	}

	// Install a HTTPS→SSH `insteadOf` rewrite for each unique host we
	// care about. Git 2.31+ accepts config via GIT_CONFIG_COUNT +
	// GIT_CONFIG_KEY_<n> + GIT_CONFIG_VALUE_<n>, which lets us do this
	// without writing anything to disk.
	seenHosts := map[string]bool{}
	idx := 0
	for _, h := range gitHosts {
		if h == "" || seenHosts[h] {
			continue
		}
		seenHosts[h] = true
		overrides["GIT_CONFIG_KEY_"+strconv.Itoa(idx)] = "url.git@" + h + ":.insteadOf"
		overrides["GIT_CONFIG_VALUE_"+strconv.Itoa(idx)] = "https://" + h + "/"
		idx++
	}
	if idx > 0 {
		overrides["GIT_CONFIG_COUNT"] = strconv.Itoa(idx)
	}

	// Apply the instance's user-supplied env vars last, so they layer on
	// top of the inherited process environment but *below* the plugin's
	// protected overrides (which are appended after this and win any
	// conflict). ValidateInstance already rejects keys in
	// ProtectedEnvKeys, so a valid config can never reach here with an
	// entry that would collide - but we double-guard by skipping
	// protected keys defensively.
	env = StripEnv(env, cfg.EnvVars)
	for k, v := range cfg.EnvVars {
		if _, protected := ProtectedEnvKeys[k]; protected {
			continue
		}
		env = append(env, k+"="+v)
	}

	env = StripEnv(env, overrides)
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env, nil
}

// RemoveAllWritable deletes root, first walking it to make every
// directory owner-writable.
//
// The go tool chmods module-cache directories under GOPATH/pkg/mod
// read-only (0555) to guard against accidental edits to cached
// modules. Since GOPATH lives inside root for the duration of the run,
// a plain os.RemoveAll silently stops the moment it hits one of those
// directories - unlinking an entry requires write permission on its
// *parent* directory, not on the entry itself - and leaves the rest of
// the module cache on disk. We own every file here (same uid that
// created it), so chmod always succeeds even though the current mode
// doesn't allow writes.
func RemoveAllWritable(root string) error {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		_ = os.Chmod(path, 0o700)
		return nil
	})
	return os.RemoveAll(root)
}

// StripEnv removes every KEY=… entry whose KEY is in keys.
func StripEnv(env []string, keys map[string]string) []string {
	out := env[:0]
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, drop := keys[kv[:eq]]; drop {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// NormalizePEM trims surrounding whitespace, strips CR characters (a
// single \r mid-body is enough to make OpenSSH's PEM parser reject the
// key with "invalid format"), and ensures a single trailing newline.
func NormalizePEM(s string) []byte {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.TrimSpace(s)
	return []byte(s + "\n")
}

// CheckPrivateKeyShape actually parses the pasted material with
// crypto/ssh so that the user gets an actionable error at instance-save
// time (and again pre-clone) instead of the opaque "Load key … : invalid
// format" that OpenSSH prints. Returns "" when the value is a usable
// unencrypted private key.
func CheckPrivateKeyShape(s string) string {
	trimmed := strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	for _, prefix := range []string{"ssh-rsa ", "ssh-ed25519 ", "ssh-dss ", "ecdsa-sha2-"} {
		if strings.HasPrefix(trimmed, prefix) {
			return "looks like a public key - paste the matching private key file (PEM/OpenSSH format)"
		}
	}
	if strings.HasPrefix(trimmed, "PuTTY-User-Key-File") {
		return "is in PuTTY (.ppk) format; convert to OpenSSH with `puttygen key.ppk -O private-openssh -o key`"
	}
	if !strings.Contains(trimmed, "-----BEGIN") || !strings.Contains(trimmed, "-----END") {
		return "does not contain PEM BEGIN/END markers - paste the entire key file including the -----BEGIN … PRIVATE KEY----- header/footer"
	}
	if _, err := ssh.ParseRawPrivateKey(NormalizePEM(s)); err != nil {
		var missingErr *ssh.PassphraseMissingError
		if errors.As(err, &missingErr) {
			return "is passphrase-protected; the plugin cannot unlock it non-interactively - generate an unencrypted key or decrypt with `ssh-keygen -p -N '' -f key`"
		}
		return "could not be parsed: " + err.Error()
	}
	return ""
}

// Slugify makes a string safe for use as a git branch path segment.
func Slugify(s string) string {
	return strings.NewReplacer("/", "-", ".", "-", "@", "-").Replace(s)
}
