// Package config loads actl's optional project config file, `.actl.yml`. It is the
// single home for "which debug slice am I running" — the job, the matrix combination,
// breakpoints, the runner image map, secrets/vars/env, and per-`environment:` overlays
// — so a real workflow can be debugged with a short `actl` instead of a flag soup.
//
// Secrets are deliberately NOT inlinable here: `.actl.yml` is a committable file, so a
// `secrets:` map (top-level or under any environment) is a hard error pointing the user
// at `secret-file:` (a dotenv path, kept out of git). vars/env are not sensitive and may
// be inlined.
package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the parsed `.actl.yml`. Every field is optional; a CLI flag overrides its
// config counterpart, which overrides the built-in default (see cmd/actl). Secrets are
// file-only: the `Secrets` nodes exist solely so the loader can reject an inline map
// with a friendly message rather than silently ignoring it.
type Config struct {
	Workflow     string                `yaml:"workflow"`
	Job          string                `yaml:"job"`
	Event        string                `yaml:"event"`
	Matrix       map[string]string     `yaml:"matrix"`
	WithDeps     *bool                 `yaml:"with-deps"`
	Images       map[string]string     `yaml:"images"`
	Breakpoints  []Breakpoint          `yaml:"breakpoints"`
	Workdir      string                `yaml:"workdir"`
	Source       string                `yaml:"source"`
	SecretFile   string                `yaml:"secret-file"`
	Vars         map[string]string     `yaml:"vars"`
	Env          map[string]string     `yaml:"env"`
	Secrets      yaml.Node             `yaml:"secrets"` // inline secrets are rejected (see validate)
	Environments map[string]EnvOverlay `yaml:"environments"`
	Inputs       map[string]string     `yaml:"inputs"`
	Needs        map[string]Need       `yaml:"needs"`
	Identity     Identity              `yaml:"identity"`
}

// Identity configures cloud identity handling per cloud (CLAUDE.md §4). The default path
// is bring-a-credential (File); ambient personal login is an opt-in fallback (GCP/AWS
// only — Azure has no ambient mode, so its Ambient is ignored).
type Identity struct {
	GCP   CloudIdentity `yaml:"gcp"`
	AWS   CloudIdentity `yaml:"aws"`
	Azure CloudIdentity `yaml:"azure"`
}

// CloudIdentity is one cloud's identity config: a brought-credential file (GCP SA key
// JSON / Azure SP creds JSON / AWS keys dotenv) and an opt-in ambient flag (GCP/AWS).
type CloudIdentity struct {
	File    string `yaml:"file"`    // path to the brought credential (kept out of git, like secret-file)
	Ambient *bool  `yaml:"ambient"` // opt-in ambient fallback; nil = unset (GCP/AWS only)
}

// EnvOverlay is a per-`environment:` overlay of secrets/vars on top of the flat
// defaults, applied when the debugged job targets that deployment environment. Like the
// top level, secrets come only via SecretFile; an inline `secrets:` map is rejected.
type EnvOverlay struct {
	SecretFile string            `yaml:"secret-file"`
	Vars       map[string]string `yaml:"vars"`
	Secrets    yaml.Node         `yaml:"secrets"` // rejected (see validate)
}

// Need seeds an upstream job's contribution to needs.* for isolated debugging, the
// config form of the -need flag.
type Need struct {
	Result  string            `yaml:"result"`
	Outputs map[string]string `yaml:"outputs"`
}

// Breakpoint is a config breakpoint: either a zero-based step index or a step name.
// Index is -1 when Name is set. The core resolves names to indices against the job's
// steps (cmd/actl passes both forms through).
type Breakpoint struct {
	Index int
	Name  string
}

// UnmarshalYAML accepts either an integer (step index) or a string (step name).
func (b *Breakpoint) UnmarshalYAML(n *yaml.Node) error {
	var i int
	if err := n.Decode(&i); err == nil {
		b.Index, b.Name = i, ""
		return nil
	}
	var s string
	if err := n.Decode(&s); err == nil {
		b.Index, b.Name = -1, s
		return nil
	}
	return fmt.Errorf("config: breakpoint must be a step index or name, got %q", n.Value)
}

// Load reads and validates `.actl.yml` at path. A missing file is not an error when the
// path is the default (explicit=false): it returns (nil, nil) so the caller proceeds on
// flags alone. When explicit (the user pointed -config at a file), a missing/unreadable
// file is reported. Unknown keys are rejected (KnownFields) to catch typos, and an
// inline `secrets:` map is rejected with a message pointing at secret-file:.
func Load(path string, explicit bool) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate rejects inline secrets. A `secrets:` key present in the YAML decodes to a
// non-zero node (Kind != 0); an absent key leaves the zero node. The same check applies
// to every environment overlay.
func (c *Config) validate() error {
	if c.Secrets.Kind != 0 {
		return errors.New("config: secrets can't be inlined in .actl.yml — put them in a dotenv file referenced by secret-file:")
	}
	for name, e := range c.Environments {
		if e.Secrets.Kind != 0 {
			return fmt.Errorf("config: environments.%s.secrets can't be inlined — use secret-file:", name)
		}
	}
	return nil
}
