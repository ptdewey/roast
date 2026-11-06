package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const maxProvisionBytes = 1 << 20

// LegacyUserPath returns the XDG-style path used by the pre-public Roast.
func LegacyUserPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "roast", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "roast", "config.yaml"), nil
}

// NeedsProvision reports whether an organization template could initialize a
// legacy config or authorize a native-protocol update. A Claude model served
// through a compatibility protocol is only a candidate, not necessarily wrong.
func NeedsProvision(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(raw, &header, false); err != nil {
		return false, err
	}
	if header.Version == nil {
		return true, nil
	}
	cfg, err := decode(raw)
	if err != nil {
		return false, err
	}
	if len(compatibilityProfiles(cfg)) == 0 {
		return false, nil
	}
	acknowledged, err := os.ReadFile(path + ".protocol-update-ack")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return string(acknowledged) != fmt.Sprintf("%x\n", sha256.Sum256(raw)), nil
}

// AcknowledgeProtocolUpdate records a choice for these exact config bytes,
// without storing credentials or modifying the config. A later config edit
// invalidates the choice; explicit config init never consults this marker.
func AcknowledgeProtocolUpdate(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(raw, &header, false); err != nil || header.Version == nil {
		return err
	}
	cfg, err := decode(raw)
	if err != nil || len(compatibilityProfiles(cfg)) == 0 {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".protocol-update-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := fmt.Fprintf(file, "%x\n", sha256.Sum256(raw)); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path+".protocol-update-ack")
}

func compatibilityProfiles(cfg Config) []string {
	var names []string
	for name, profile := range cfg.InferenceProfiles {
		if InferFamily(profile.Model) == "anthropic" && (profile.Protocol == "" || profile.Protocol == "chat" || profile.Protocol == "responses") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Provision downloads a complete versioned configuration, optionally carries
// forward the credential from a legacy gateway config, and atomically installs
// it at path. The remote document may contain endpoint and model policy but no
// inline secrets.
func Provision(ctx context.Context, sourceURL, path, legacyPath string) error {
	plan, err := PlanProvision(ctx, sourceURL, path, legacyPath)
	if err != nil {
		return err
	}
	return plan.Install()
}

// ProvisionPlan is a downloaded, validated configuration template and the
// exact configuration it would install. Nothing is written until Install, so
// a caller can show the plan and ask for confirmation first.
type ProvisionPlan struct {
	Path string
	// Config is the normalized configuration in effect after Install.
	Config Config
	// ProtocolUpdates names existing profiles the template switches to native
	// Anthropic Messages. It is non-empty only when Install updates an existing
	// versioned config in place; such an update changes no other setting.
	ProtocolUpdates []string
	// ImportedLegacyCredential reports that profiles without a credential
	// source received the credential from a legacy config.
	ImportedLegacyCredential bool

	raw          []byte
	backupSuffix string
	// existing holds the bytes at Path the plan was computed from; nil when
	// Path did not exist.
	existing []byte
}

// TemplateUpload reports whether Install applies the template's upload
// settings with uploads enabled. A protocol update never changes uploads.
func (p *ProvisionPlan) TemplateUpload() bool {
	return len(p.ProtocolUpdates) == 0 && p.Config.Upload.Enabled
}

// PlanProvision downloads and validates a configuration template and computes
// what Install would write at path, without modifying any file.
func PlanProvision(ctx context.Context, sourceURL, path, legacyPath string) (*ProvisionPlan, error) {
	if path == "" {
		var err error
		path, err = UserPath()
		if err != nil {
			return nil, err
		}
	}
	raw, cfg, err := fetchProvision(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	plan := &ProvisionPlan{Path: path}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		plan.existing = existing
		var header struct {
			Version *int `yaml:"version"`
		}
		if err := decodeKnown(existing, &header, false); err != nil {
			return nil, err
		}
		if header.Version != nil {
			updated, err := updateProtocols(existing, cfg)
			if err != nil {
				return nil, err
			}
			before, err := decode(existing)
			if err != nil {
				return nil, err
			}
			after, err := decode(updated)
			if err != nil {
				return nil, err
			}
			for name, profile := range after.InferenceProfiles {
				if before.InferenceProfiles[name].Protocol != profile.Protocol {
					plan.ProtocolUpdates = append(plan.ProtocolUpdates, name)
				}
			}
			sort.Strings(plan.ProtocolUpdates)
			if plan.Config, err = normalize(after); err != nil {
				return nil, err
			}
			plan.raw, plan.backupSuffix = updated, ".pre-protocol-update"
			return plan, nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if legacyPath != "" {
		legacyRaw, readErr := os.ReadFile(legacyPath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return nil, fmt.Errorf("read legacy config: %w", readErr)
		}
		if readErr == nil {
			var legacy legacyConfig
			if err := decodeKnown(legacyRaw, &legacy, true); err != nil {
				return nil, fmt.Errorf("parse legacy config: %w", err)
			}
			if legacy.Gateway.APIKey != "" && cfg.Provisioning != nil && cfg.Provisioning.ImportLegacyCredential {
				for name, profile := range cfg.InferenceProfiles {
					if !IsOAuth(profile.Auth) && profile.APIKey == "" && profile.APIKeyEnv == "" {
						profile.APIKey = legacy.Gateway.APIKey
						cfg.InferenceProfiles[name] = profile
					}
				}
				if cfg.Upload.Backend == "presigned" && cfg.Upload.Token == "" && cfg.Upload.TokenEnv == "" {
					cfg.Upload.Token = legacy.Gateway.APIKey
				}
				cfg.Provisioning = nil
				raw, err = yaml.Marshal(cfg)
				if err != nil {
					return nil, fmt.Errorf("encode migrated config: %w", err)
				}
				plan.ImportedLegacyCredential = true
			}
		}
	}
	if legacyPath != "" && filepath.Clean(path) == filepath.Clean(legacyPath) {
		plan.backupSuffix = ".pre-v1"
	}
	if plan.existing != nil && plan.backupSuffix == "" {
		return nil, fmt.Errorf("config already exists at %s", path)
	}
	plan.Config, plan.raw = cfg, raw
	return plan, nil
}

// Install writes the planned configuration. It refuses to proceed when the
// file at Path changed after the plan was computed.
func (p *ProvisionPlan) Install() error {
	current, err := os.ReadFile(p.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if p.existing != nil {
			return fmt.Errorf("config at %s was removed while installing; run the command again", p.Path)
		}
	case err != nil:
		return err
	case p.existing == nil || !bytes.Equal(current, p.existing):
		return fmt.Errorf("config at %s changed while installing; run the command again", p.Path)
	}
	if err := writeAtomic(p.Path, p.raw, p.backupSuffix); err != nil {
		return err
	}
	if len(p.ProtocolUpdates) > 0 {
		return AcknowledgeProtocolUpdate(p.Path)
	}
	return nil
}

// updateProtocols uses an explicitly supplied template as the authority for
// endpoint support. It never infers gateway capabilities from model names alone
// and never replaces a versioned config with the template's unrelated settings.
func updateProtocols(raw []byte, template Config) ([]byte, error) {
	cfg, err := decode(raw)
	if err != nil {
		return nil, err
	}
	var updates []string
	for _, name := range compatibilityProfiles(cfg) {
		profile := cfg.InferenceProfiles[name]
		for _, target := range template.InferenceProfiles {
			if profile.BaseURL != "" && strings.TrimRight(profile.BaseURL, "/") == strings.TrimRight(target.Endpoint(), "/") &&
				InferFamily(target.Model) == "anthropic" && target.Protocol == "anthropic" {
				updates = append(updates, name)
				profile.Protocol = "anthropic"
				cfg.InferenceProfiles[name] = profile
				break
			}
		}
	}
	if len(updates) == 0 {
		return nil, errors.New("config already exists; the template authorizes no matching Claude protocol updates (use --output for a new config)")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	profiles := yamlMappingValue(document.Content[0], "inference_profiles")
	for _, name := range updates {
		profile := yamlMappingValue(profiles, name)
		if profile == nil || profile.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("profile %q uses a YAML alias; update its protocol manually", name)
		}
		if node := yamlMappingValue(profile, "protocol"); node != nil {
			node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!str", "anthropic"
			node.Alias, node.Content = nil, nil
		} else {
			profile.Content = append(profile.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "protocol"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "anthropic"})
		}
	}
	updated, err := yaml.Marshal(&document)
	if err != nil {
		return nil, err
	}
	actual, err := decode(updated)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(actual, cfg) {
		return nil, errors.New("protocol update would change other settings through YAML aliases; update protocols manually")
	}
	if _, err := normalize(actual); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(updated), nil
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// FetchError reports that a configuration URL could not be retrieved because
// of a transport failure or a non-success HTTP status, as opposed to an
// invalid URL or template.
type FetchError struct{ Err error }

func (e *FetchError) Error() string { return e.Err.Error() }
func (e *FetchError) Unwrap() error { return e.Err }

var errCrossOriginRedirect = errors.New("configuration URL redirected to another origin")

func fetchProvision(ctx context.Context, sourceURL string) ([]byte, Config, error) {
	u, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && IsLoopbackHost(u.Hostname()))) || u.User != nil || u.Fragment != "" {
		return nil, Config{}, errors.New("configuration URL must be HTTPS, or HTTP on loopback, without credentials or a fragment")
	}
	originScheme, originHost := u.Scheme, u.Host
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("configuration URL redirected too many times")
			}
			if next.URL.Scheme != originScheme || next.URL.Host != originHost || next.URL.User != nil || next.URL.Fragment != "" {
				return errCrossOriginRedirect
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, Config{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("fetch configuration: %w", err)
		if errors.Is(err, errCrossOriginRedirect) {
			return nil, Config{}, err
		}
		return nil, Config{}, &FetchError{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Config{}, &FetchError{Err: fmt.Errorf("fetch configuration: HTTP %d", resp.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProvisionBytes+1))
	if err != nil {
		return nil, Config{}, &FetchError{Err: fmt.Errorf("read configuration: %w", err)}
	}
	if len(raw) > maxProvisionBytes {
		return nil, Config{}, fmt.Errorf("configuration exceeds %d bytes", maxProvisionBytes)
	}
	cfg, err := ValidateProvisionDocument(raw)
	if err != nil {
		return nil, Config{}, err
	}
	return bytes.TrimSpace(raw), cfg, nil
}

// ValidateProvisionDocument validates an organization-provided configuration
// template. Templates may reference credential environment variables, but may
// not contain secret values, control characters, or prompt overrides, which
// name local files whose contents would be sent to the template's endpoints.
func ValidateProvisionDocument(raw []byte) (Config, error) {
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(raw, &header, false); err != nil {
		return Config{}, fmt.Errorf("parse downloaded configuration: %w", err)
	}
	if header.Version == nil {
		return Config{}, errors.New("downloaded configuration must declare a version")
	}
	cfg, err := decode(raw)
	if err != nil {
		return Config{}, fmt.Errorf("parse downloaded configuration: %w", err)
	}
	if err := rejectControlCharacters(cfg); err != nil {
		return Config{}, err
	}
	cfg, err = normalize(cfg)
	if err != nil {
		return Config{}, fmt.Errorf("validate downloaded configuration: %w", err)
	}
	for name, profile := range cfg.InferenceProfiles {
		if profile.APIKey != "" {
			return Config{}, fmt.Errorf("downloaded configuration must not contain inference_profiles.%s.api_key", name)
		}
	}
	if cfg.Upload.Token != "" {
		return Config{}, errors.New("downloaded configuration must not contain upload.token")
	}
	for _, prompt := range []struct{ field, value string }{
		{"finder_code", cfg.Prompts.FinderCode}, {"finder_prose", cfg.Prompts.FinderProse}, {"verifier", cfg.Prompts.Verifier},
	} {
		if prompt.value != "" {
			return Config{}, fmt.Errorf("downloaded configuration must not contain prompts.%s; prompt overrides belong only in the user's own config", prompt.field)
		}
	}
	return cfg, nil
}

// rejectControlCharacters refuses template text containing C0 or C1 control
// characters, DEL, or invalid UTF-8. The install summary and the upload
// confirmation print these fields, and an escape sequence in one could repaint
// what the user is asked to approve. Secret fields and prompt overrides are
// not checked here because ValidateProvisionDocument rejects them outright.
func rejectControlCharacters(cfg Config) error {
	reject := func(where string) error {
		return fmt.Errorf("downloaded configuration must not contain control characters (in %s)", where)
	}
	type field struct{ path, value string }
	check := func(fields ...field) error {
		for _, f := range fields {
			if hasControlCharacter(f.value) {
				return reject(f.path)
			}
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.InferenceProfiles)) {
		if hasControlCharacter(name) {
			return reject(fmt.Sprintf("inference profile name %q", name))
		}
		profile, prefix := cfg.InferenceProfiles[name], "inference_profiles."+name+"."
		if err := check(
			field{prefix + "endpoint", profile.BaseURL}, field{prefix + "model", profile.Model},
			field{prefix + "protocol", profile.Protocol}, field{prefix + "api_key_env", profile.APIKeyEnv},
		); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Roles)) {
		if hasControlCharacter(name) {
			return reject(fmt.Sprintf("role name %q", name))
		}
		role := cfg.Roles[name]
		if err := check(field{"roles." + name + ".profile", role.Profile}); err != nil {
			return err
		}
		for _, route := range slices.Sorted(maps.Keys(role.AuthorRoutes)) {
			if hasControlCharacter(route) {
				return reject(fmt.Sprintf("roles.%s.author_routes key %q", name, route))
			}
			if err := check(field{"roles." + name + ".author_routes." + route, role.AuthorRoutes[route]}); err != nil {
				return err
			}
		}
	}
	for i, exclude := range cfg.Review.Excludes {
		if err := check(field{fmt.Sprintf("review.excludes[%d]", i), exclude}); err != nil {
			return err
		}
	}
	upload := cfg.Upload
	return check(
		field{"upload.backend", upload.Backend}, field{"upload.endpoint", upload.Endpoint},
		field{"upload.bucket", upload.Bucket}, field{"upload.prefix", upload.Prefix},
		field{"upload.region", upload.Region}, field{"upload.access_key_env", upload.AccessKeyEnv},
		field{"upload.secret_key_env", upload.SecretKeyEnv}, field{"upload.session_token_env", upload.SessionTokenEnv},
		field{"upload.token_env", upload.TokenEnv},
	)
}

// hasControlCharacter reports whether s holds a C0 control character
// (including tab and newline), DEL, a C1 control character, or invalid UTF-8.
func hasControlCharacter(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

func writeAtomic(path string, raw []byte, backupSuffix string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if backupSuffix == "" {
			return fmt.Errorf("config already exists at %s", path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular config at %s", path)
		}
		backup := path + backupSuffix
		if err := ensureLegacyBackup(path, backup, 0o600); err != nil {
			return fmt.Errorf("back up config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	return nil
}

func ensureLegacyBackup(source, destination string, mode os.FileMode) error {
	sourceRaw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("existing backup is not a regular file: %s", destination)
		}
		backupRaw, readErr := os.ReadFile(destination)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(sourceRaw, backupRaw) {
			return fmt.Errorf("existing backup does not match legacy config: %s", destination)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyExclusive(destination, sourceRaw, mode, persistExclusiveCopy)
}

func copyExclusive(destination string, raw []byte, mode os.FileMode, persist func(*os.File, []byte) error) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = os.Remove(destination)
		}
	}()
	if err := persist(file, raw); err != nil {
		return err
	}
	complete = true
	return nil
}

func persistExclusiveCopy(file *os.File, raw []byte) error {
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}
