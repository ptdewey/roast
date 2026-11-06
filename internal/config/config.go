// Package config loads user-controlled model and review configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// CurrentVersion is the durable public configuration schema version.
	CurrentVersion = 1

	defaultOpenAIBaseURL    = "https://api.openai.com/v1"
	defaultAnthropicBaseURL = "https://api.anthropic.com/v1"
)

// DefaultBaseURL returns the public default endpoint for a wire protocol.
func DefaultBaseURL(protocol string) string {
	if protocol == "anthropic" {
		return defaultAnthropicBaseURL
	}
	return defaultOpenAIBaseURL
}

// InferenceProfile is one named model, endpoint, protocol, and credential
// source. Profiles live only in user-owned config, never repository config.
type InferenceProfile struct {
	// Auth is empty/api_key for existing profiles, or chatgpt/codex for subscription OAuth.
	Auth            string `yaml:"auth,omitempty"`
	Account         string `yaml:"account,omitempty"`
	BaseURL         string `yaml:"endpoint"`
	APIKey          string `yaml:"api_key"`
	APIKeyEnv       string `yaml:"api_key_env"`
	Model           string `yaml:"model"`
	Protocol        string `yaml:"protocol"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`
}

// Role selects an inference profile. The finder may route away from the
// author's model family through another named profile.
type Role struct {
	Profile string `yaml:"profile"`
	// Pin keeps this role selected even when it belongs to the same model
	// family as the author. Only the finder uses this setting.
	Pin bool `yaml:"pin"`
	// AuthorRoutes maps an exact author model or model family to a profile.
	AuthorRoutes map[string]string `yaml:"author_routes"`
}

// ResolvedRole is the complete, runtime-ready role. APIKey is secret and must
// never be copied into run metadata.
type ResolvedRole struct {
	Auth            string
	Account         string
	Profile         string
	BaseURL         string
	APIKey          string
	Model           string
	Protocol        string
	MaxOutputTokens int
	Pin             bool
	AuthorRoutes    map[string]string
	// Credential names where APIKey came from. It never holds a secret.
	Credential CredentialSource
	// ExplicitEndpoint reports that the profile or a role override chose
	// BaseURL. Otherwise BaseURL is the protocol default or the ambient
	// provider base URL, and follows a protocol change.
	ExplicitEndpoint bool
}

// Credential source kinds, in resolution order.
const (
	CredentialRoleEnv     = "role_env"
	CredentialProfileEnv  = "profile_env"
	CredentialProfileKey  = "profile_api_key"
	CredentialProviderEnv = "provider_env"
	CredentialNone        = "none"
	CredentialChatGPT     = "chatgpt_oauth"
	CredentialCodex       = "codex_oauth"
)

// CredentialSource describes where a role's credential came from without
// carrying its value.
type CredentialSource struct {
	Kind string
	// Account is a local account label, never an OAuth token.
	Account string
	// Env is the environment variable read for an environment-backed kind.
	Env string
	// Withheld names an ambient provider key that was set but not sent,
	// because the role's endpoint is not that provider's own endpoint.
	Withheld string
}

func (s CredentialSource) String() string {
	switch s.Kind {
	case CredentialChatGPT:
		return "ChatGPT OAuth (account " + s.Account + ")"
	case CredentialCodex:
		return "Codex OAuth (account " + s.Account + ")"
	case CredentialRoleEnv:
		return s.Env + " (role override)"
	case CredentialProfileEnv:
		return s.Env + " (profile api_key_env)"
	case CredentialProfileKey:
		return "api_key in the config file"
	case CredentialProviderEnv:
		return s.Env + " (ambient provider key)"
	}
	if s.Withheld != "" {
		return fmt.Sprintf("none; %s is set but is sent only to its provider's default endpoint or %s", s.Withheld, pairedBaseURLEnv(s.Withheld))
	}
	return "none"
}

// Review contains local review defaults.
type Review struct {
	ConfidenceFloor      float64  `yaml:"confidence_floor"`
	QuickConfidenceFloor float64  `yaml:"quick_confidence_floor"`
	Excludes             []string `yaml:"excludes"`
}

// Prompts optionally replaces embedded prompt templates with user-owned files.
type Prompts struct {
	FinderCode  string `yaml:"finder_code"`
	FinderProse string `yaml:"finder_prose"`
	Verifier    string `yaml:"verifier"`
}

// Upload configures optional direct upload of deterministic run bundles to an
// S3-compatible object store. Credentials are named by environment variable;
// they are never accepted inline or copied into run metadata.
type Upload struct {
	Enabled           bool   `yaml:"enabled"`
	Backend           string `yaml:"backend"`
	Endpoint          string `yaml:"endpoint"`
	Bucket            string `yaml:"bucket"`
	Prefix            string `yaml:"prefix"`
	Region            string `yaml:"region"`
	AccessKeyEnv      string `yaml:"access_key_env"`
	SecretKeyEnv      string `yaml:"secret_key_env"`
	SessionTokenEnv   string `yaml:"session_token_env"`
	Token             string `yaml:"token"`
	TokenEnv          string `yaml:"token_env"`
	PathStyle         bool   `yaml:"path_style"`
	AllowInsecureHTTP bool   `yaml:"allow_insecure_http"`
}

// ResolvedUpload is the runtime object-store configuration. Secret values are
// resolved only when an upload is attempted.
type ResolvedUpload struct {
	Backend                            string
	Endpoint, Bucket, Prefix, Region   string
	AccessKey, SecretKey, SessionToken string
	Token                              string
	PathStyle                          bool
	// AllowInsecureHTTP permits plaintext HTTP to non-loopback hosts,
	// including presigned upload URLs returned by a broker.
	AllowInsecureHTTP bool
}

// Config is Roast's user configuration.
type Config struct {
	Version           int                         `yaml:"version"`
	Provisioning      *Provisioning               `yaml:"provisioning,omitempty"`
	InferenceProfiles map[string]InferenceProfile `yaml:"inference_profiles"`
	Roles             map[string]Role             `yaml:"roles"`
	Review            Review                      `yaml:"review"`
	Prompts           Prompts                     `yaml:"prompts"`
	Upload            Upload                      `yaml:"upload"`
}

// Provisioning contains one-time instructions honored only while installing a
// remote configuration template. It has no effect during normal config loads.
type Provisioning struct {
	ImportLegacyCredential bool `yaml:"import_legacy_credential"`
}

// CredentialEnvNames returns every environment variable referenced as a
// profile credential source so artifact capture can always redact it.
func (c Config) CredentialEnvNames() []string {
	out := make([]string, 0)
	for _, profile := range c.InferenceProfiles {
		if profile.APIKeyEnv != "" {
			out = append(out, profile.APIKeyEnv)
		}
	}
	for _, name := range []string{c.Upload.AccessKeyEnv, c.Upload.SecretKeyEnv, c.Upload.SessionTokenEnv, c.Upload.TokenEnv} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Defaults returns provider-neutral defaults. Model names are intentionally
// empty: users choose the models available at their endpoints. The endpoint is
// left unset so the protocol default, or the user's own provider base URL,
// applies.
func Defaults() Config {
	profile := InferenceProfile{
		Protocol:        "responses",
		MaxOutputTokens: 32768,
	}
	return Config{
		Version: CurrentVersion,
		InferenceProfiles: map[string]InferenceProfile{
			"finder": profile, "verifier": profile,
		},
		Roles: map[string]Role{
			"finder": {Profile: "finder"}, "verifier": {Profile: "verifier"},
		},
		Review: Review{
			ConfidenceFloor:      0.6,
			QuickConfidenceFloor: 0.8,
		},
	}
}

// UserPath returns the default config file path.
func UserPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "roast", "config.yaml"), nil
}

// Load reads and validates user configuration. An empty path selects UserPath;
// a missing default file is allowed, while a missing explicit file is an error.
func Load(path string) (Config, error) {
	explicit := path != ""
	if path == "" {
		var err error
		path, err = UserPath()
		if err != nil {
			return Config{}, err
		}
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return normalize(Defaults())
	}
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}
	cfg, err := decode(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return normalize(cfg)
}

func decode(data []byte) (Config, error) {
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(data, &header, false); err != nil {
		return Config{}, err
	}
	if header.Version != nil {
		if *header.Version != CurrentVersion {
			return Config{}, fmt.Errorf("unsupported config version %d (this build supports version %d)", *header.Version, CurrentVersion)
		}
		var cfg Config
		if err := decodeKnown(data, &cfg, true); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	shape, err := unversionedShape(data)
	if err != nil {
		return Config{}, err
	}
	switch shape {
	case shapeLegacyRoles:
		var legacy legacyConfig
		if err := decodeKnown(data, &legacy, true); err != nil {
			return Config{}, err
		}
		return migrateLegacy(legacy), nil
	case shapeDraftProfiles:
		var draft draftConfig
		if err := decodeKnown(data, &draft, true); err != nil {
			return Config{}, err
		}
		return migrateDraft(draft), nil
	default:
		var cfg Config
		if err := decodeKnown(data, &cfg, true); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
}

func decodeKnown(data []byte, out any, knownFields bool) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(knownFields)
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config must contain exactly one YAML document")
		}
		return err
	}
	return nil
}

func normalize(cfg Config) (Config, error) {
	if cfg.Version == 0 {
		cfg.Version = CurrentVersion
	}
	if cfg.Version != CurrentVersion {
		return Config{}, fmt.Errorf("unsupported config version %d (this build supports version %d)", cfg.Version, CurrentVersion)
	}

	defaults := Defaults()
	if cfg.InferenceProfiles == nil {
		cfg.InferenceProfiles = defaults.InferenceProfiles
	}
	if cfg.Roles == nil {
		cfg.Roles = defaults.Roles
	}
	for name, profile := range cfg.InferenceProfiles {
		normalized, err := normalizeProfile(name, profile)
		if err != nil {
			return Config{}, err
		}
		cfg.InferenceProfiles[name] = normalized
	}
	for _, name := range []string{"finder", "verifier"} {
		role, ok := cfg.Roles[name]
		if !ok || role.Profile == "" {
			role = defaults.Roles[name]
			cfg.Roles[name] = role
			if _, exists := cfg.InferenceProfiles[role.Profile]; !exists {
				cfg.InferenceProfiles[role.Profile] = defaults.InferenceProfiles[role.Profile]
			}
		}
		if _, ok := cfg.InferenceProfiles[role.Profile]; !ok {
			return Config{}, fmt.Errorf("roles.%s.profile references unknown inference profile %q", name, role.Profile)
		}
		normalizedRoutes := make(map[string]string, len(role.AuthorRoutes))
		for family, profileName := range role.AuthorRoutes {
			family = canonicalFamily(strings.ToLower(strings.TrimSpace(family)))
			if family == "" {
				return Config{}, fmt.Errorf("roles.%s.author_routes contains empty model family", name)
			}
			if _, ok := cfg.InferenceProfiles[profileName]; !ok {
				return Config{}, fmt.Errorf("roles.%s.author_routes.%s references unknown inference profile %q", name, family, profileName)
			}
			if _, duplicate := normalizedRoutes[family]; duplicate {
				return Config{}, fmt.Errorf("roles.%s.author_routes contains duplicate family %q", name, family)
			}
			normalizedRoutes[family] = profileName
		}
		role.AuthorRoutes = normalizedRoutes
		cfg.Roles[name] = role
	}
	if err := normalizeUpload(&cfg.Upload); err != nil {
		return Config{}, err
	}
	if cfg.Review.ConfidenceFloor == 0 {
		cfg.Review.ConfidenceFloor = defaults.Review.ConfidenceFloor
	}
	if cfg.Review.QuickConfidenceFloor == 0 {
		cfg.Review.QuickConfidenceFloor = defaults.Review.QuickConfidenceFloor
	}
	return cfg, nil
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func normalizeUpload(upload *Upload) error {
	upload.Backend = strings.ToLower(strings.TrimSpace(upload.Backend))
	if upload.Backend == "" {
		upload.Backend = "s3"
	}
	if upload.Backend != "s3" && upload.Backend != "presigned" {
		return errors.New("upload.backend must be s3 or presigned")
	}
	if upload.AccessKeyEnv == "" {
		upload.AccessKeyEnv = "ROAST_UPLOAD_ACCESS_KEY"
	}
	if upload.SecretKeyEnv == "" {
		upload.SecretKeyEnv = "ROAST_UPLOAD_SECRET_KEY"
	}
	for field, name := range map[string]string{
		"access_key_env":    upload.AccessKeyEnv,
		"secret_key_env":    upload.SecretKeyEnv,
		"session_token_env": upload.SessionTokenEnv,
		"token_env":         upload.TokenEnv,
	} {
		if name != "" && !envNamePattern.MatchString(name) {
			return fmt.Errorf("upload.%s is not a valid environment variable name", field)
		}
	}
	upload.Endpoint = strings.TrimSpace(upload.Endpoint)
	upload.Bucket = strings.TrimSpace(upload.Bucket)
	upload.Prefix = strings.Trim(strings.TrimSpace(upload.Prefix), "/")
	if upload.Endpoint != "" {
		u, err := url.Parse(upload.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("upload.endpoint must be an http(s) origin without credentials, a path, query, or fragment")
		}
		if u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) && !upload.AllowInsecureHTTP {
			return errors.New("upload.endpoint must use https outside loopback; set upload.allow_insecure_http only for a trusted local network")
		}
		u.Path = ""
		upload.Endpoint = strings.TrimSuffix(u.String(), "/")
	}
	if upload.Token != "" && upload.TokenEnv != "" {
		return errors.New("upload cannot set both token and token_env")
	}
	if upload.Enabled && upload.Endpoint == "" {
		return errors.New("upload.endpoint is required when upload.enabled is true")
	}
	if upload.Backend == "s3" {
		if upload.Prefix == "" {
			upload.Prefix = "roast-runs"
		}
		for _, segment := range strings.Split(upload.Prefix, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return errors.New("upload.prefix must be a relative object-key prefix without empty, '.' or '..' segments")
			}
		}
		if upload.Enabled && upload.Bucket == "" {
			return errors.New("upload.bucket is required for the s3 backend")
		}
		if upload.Token != "" || upload.TokenEnv != "" {
			return errors.New("upload.token and upload.token_env are only valid for the presigned backend")
		}
	} else {
		if upload.Bucket != "" || upload.Prefix != "" || upload.Region != "" || upload.PathStyle {
			return errors.New("upload.bucket, upload.prefix, upload.region, and upload.path_style are only valid for the s3 backend")
		}
	}
	return nil
}

// IsLoopbackHost reports whether host is localhost or a loopback IP address.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ResolveUpload resolves object-store credentials from the configured
// environment variables. It intentionally does not consult AWS profiles,
// instance metadata, SSO, or any provider-specific credential chain.
func (c Config) ResolveUpload() (ResolvedUpload, error) {
	if c.Upload.Endpoint == "" {
		return ResolvedUpload{}, errors.New("upload endpoint is not configured")
	}
	if c.Upload.Backend == "presigned" {
		token := c.Upload.Token
		if token == "" && c.Upload.TokenEnv != "" {
			token = os.Getenv(c.Upload.TokenEnv)
		}
		if token == "" {
			if c.Upload.TokenEnv == "" {
				return ResolvedUpload{}, errors.New("presigned upload token is not configured")
			}
			return ResolvedUpload{}, fmt.Errorf("presigned upload token is not configured (set %s)", c.Upload.TokenEnv)
		}
		return ResolvedUpload{Backend: "presigned", Endpoint: c.Upload.Endpoint, Token: token, AllowInsecureHTTP: c.Upload.AllowInsecureHTTP}, nil
	}
	if c.Upload.Bucket == "" {
		return ResolvedUpload{}, errors.New("upload bucket is not configured")
	}
	access := os.Getenv(c.Upload.AccessKeyEnv)
	secret := os.Getenv(c.Upload.SecretKeyEnv)
	if access == "" || secret == "" {
		return ResolvedUpload{}, fmt.Errorf("upload credentials are not configured (set %s and %s)", c.Upload.AccessKeyEnv, c.Upload.SecretKeyEnv)
	}
	return ResolvedUpload{
		Backend:  c.Upload.Backend,
		Endpoint: c.Upload.Endpoint, Bucket: c.Upload.Bucket,
		Prefix: c.Upload.Prefix, Region: c.Upload.Region,
		AccessKey: access, SecretKey: secret,
		SessionToken: os.Getenv(c.Upload.SessionTokenEnv), PathStyle: c.Upload.PathStyle,
		AllowInsecureHTTP: c.Upload.AllowInsecureHTTP,
	}, nil
}

func normalizeProfile(name string, profile InferenceProfile) (InferenceProfile, error) {
	if strings.TrimSpace(name) == "" {
		return InferenceProfile{}, errors.New("inference profile names cannot be empty")
	}
	if profile.Protocol == "" {
		profile.Protocol = "responses"
	}
	if profile.MaxOutputTokens == 0 {
		profile.MaxOutputTokens = 32768
	}
	if err := validateProfileAuth(name, &profile); err != nil {
		return InferenceProfile{}, err
	}
	if profile.APIKey != "" && profile.APIKeyEnv != "" {
		return InferenceProfile{}, fmt.Errorf("inference_profiles.%s cannot set both api_key and api_key_env", name)
	}
	if profile.APIKeyEnv != "" && !envNamePattern.MatchString(profile.APIKeyEnv) {
		return InferenceProfile{}, fmt.Errorf("inference_profiles.%s.api_key_env is not a valid environment variable name", name)
	}
	if err := validateEndpoint("inference_profiles."+name, profile.Endpoint(), profile.Protocol, profile.MaxOutputTokens); err != nil {
		return InferenceProfile{}, err
	}
	return profile, nil
}

// Endpoint returns the profile's configured endpoint, or its protocol's public
// default when the profile does not set one.
func (p InferenceProfile) Endpoint() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	if IsOAuth(p.Auth) {
		return OAuthBaseURL(p.Auth)
	}
	return DefaultBaseURL(p.Protocol)
}

// DescribeEndpoint states where the profile sends requests, including the
// ambient base URL that replaces an unset endpoint.
func (p InferenceProfile) DescribeEndpoint() string {
	if IsOAuth(p.Auth) {
		return OAuthBaseURL(p.Auth) + " (" + p.Auth + " subscription)"
	}
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return fmt.Sprintf("%s (default, or %s)", DefaultBaseURL(p.Protocol), providerBaseURLEnv(p.Protocol))
}

// DescribeCredential states, without resolving any secret, where the profile
// obtains its credential when selected for role.
func (p InferenceProfile) DescribeCredential(role string) string {
	if IsOAuth(p.Auth) {
		return oauthCredential(p.Auth, p.Account).String()
	}
	roleVar := roleEnvPrefix(role) + "API_KEY"
	keyVar := providerAPIKeyEnv(p.Protocol)
	switch {
	case p.APIKeyEnv != "":
		return "env " + p.APIKeyEnv
	case p.APIKey != "":
		return "api_key in the config file"
	case p.BaseURL == "":
		return fmt.Sprintf("none named; %s with the default endpoint or %s, unless %s is set", keyVar, pairedBaseURLEnv(keyVar), roleVar)
	case sameEndpoint(p.BaseURL, DefaultBaseURL(p.Protocol)):
		return fmt.Sprintf("none named; %s unless %s is set", keyVar, roleVar)
	default:
		return "none named; set " + roleVar
	}
}

// ResolveRole expands a role selection into the complete runtime role and
// applies role/provider environment overrides.
//
// Role-specific ROAST_<ROLE>_* variables always apply. The ambient provider
// base URL (OPENAI_BASE_URL or ANTHROPIC_BASE_URL) applies only to a profile
// that sets no endpoint. The credential is, in order: ROAST_<ROLE>_API_KEY;
// the profile's api_key_env or api_key; otherwise the ambient provider key,
// but only when the role's endpoint is that provider's default endpoint or its
// ambient base URL. A profile with any other endpoint must name its own
// credential.
func (c Config) ResolveRole(name string) (ResolvedRole, error) {
	selection, ok := c.Roles[name]
	if !ok || selection.Profile == "" {
		return ResolvedRole{}, fmt.Errorf("role %q is not configured", name)
	}
	return c.resolveProfile(name, selection.Profile, selection)
}

func (c Config) resolveProfile(roleName, profileName string, selection Role) (ResolvedRole, error) {
	profile, ok := c.InferenceProfiles[profileName]
	if !ok {
		return ResolvedRole{}, fmt.Errorf("role %s references unknown inference profile %q", roleName, profileName)
	}
	role := ResolvedRole{
		Profile: profileName, BaseURL: profile.BaseURL, ExplicitEndpoint: profile.BaseURL != "",
		Auth: profile.Auth, Account: profile.Account,
		Model: profile.Model, Protocol: profile.Protocol, MaxOutputTokens: profile.MaxOutputTokens,
		Pin: selection.Pin, AuthorRoutes: selection.AuthorRoutes,
	}
	if !role.ExplicitEndpoint {
		if IsOAuth(role.Auth) {
			role.BaseURL = OAuthBaseURL(role.Auth)
		} else {
			role.BaseURL = implicitEndpoint(role.Protocol)
		}
	}
	crossedProvider, err := applyRoleEnv(roleName, &role)
	if err != nil {
		return ResolvedRole{}, err
	}
	if IsOAuth(role.Auth) {
		role.Account = chatGPTAccount(role.Account)
		role.Credential = oauthCredential(role.Auth, role.Account)
		if os.Getenv(roleEnvPrefix(roleName)+"API_KEY") != "" {
			return ResolvedRole{}, fmt.Errorf("%sAPI_KEY cannot be combined with auth: %s", roleEnvPrefix(roleName), role.Auth)
		}
		if err := validateOAuthRoute(role); err != nil {
			return ResolvedRole{}, err
		}
	} else {
		role.APIKey, role.Credential = resolveCredential(roleName, profile, role, crossedProvider)
	}
	return role, nil
}

// applyRoleEnv applies role-specific overrides. It reports whether a protocol
// override moved the role between providers' endpoints.
func applyRoleEnv(name string, role *ResolvedRole) (bool, error) {
	prefix := roleEnvPrefix(name)
	var protocol, baseURL *string
	if value := os.Getenv(prefix + "PROTOCOL"); value != "" {
		protocol = &value
	}
	if value := os.Getenv(prefix + "BASE_URL"); value != "" {
		baseURL = &value
	}
	crossedProvider := applyEndpointOverrides(role, baseURL, protocol)
	if value := os.Getenv(prefix + "MODEL"); value != "" {
		role.Model = value
	}
	if value := os.Getenv(prefix + "MAX_OUTPUT_TOKENS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return false, fmt.Errorf("%sMAX_OUTPUT_TOKENS must be an integer >= 1 (got %q)", prefix, value)
		}
		role.MaxOutputTokens = n
	}
	return crossedProvider, nil
}

// resolveCredential selects the role's credential. A profile credential does
// not follow a protocol override onto another provider's default endpoint.
func resolveCredential(roleName string, profile InferenceProfile, role ResolvedRole, crossedProvider bool) (string, CredentialSource) {
	roleVar := roleEnvPrefix(roleName) + "API_KEY"
	if value := os.Getenv(roleVar); value != "" {
		return value, CredentialSource{Kind: CredentialRoleEnv, Env: roleVar}
	}
	if !crossedProvider {
		if profile.APIKeyEnv != "" {
			return os.Getenv(profile.APIKeyEnv), CredentialSource{Kind: CredentialProfileEnv, Env: profile.APIKeyEnv}
		}
		if profile.APIKey != "" {
			return profile.APIKey, CredentialSource{Kind: CredentialProfileKey}
		}
	}
	return ambientCredential(role.Protocol, role.BaseURL)
}

// ambientCredential returns the ambient provider key for protocol only when
// endpoint is that provider's default endpoint or the base URL the same
// environment pairs with the key.
func ambientCredential(protocol, endpoint string) (string, CredentialSource) {
	keyVar := providerAPIKeyEnv(protocol)
	value := os.Getenv(keyVar)
	if value == "" {
		return "", CredentialSource{Kind: CredentialNone}
	}
	if sameEndpoint(endpoint, DefaultBaseURL(protocol)) {
		return value, CredentialSource{Kind: CredentialProviderEnv, Env: keyVar}
	}
	if base := os.Getenv(providerBaseURLEnv(protocol)); base != "" && sameEndpoint(endpoint, base) {
		return value, CredentialSource{Kind: CredentialProviderEnv, Env: keyVar}
	}
	return "", CredentialSource{Kind: CredentialNone, Withheld: keyVar}
}

// implicitEndpoint is the endpoint of a role that chose none: the ambient
// provider base URL for protocol, or the protocol's public default.
func implicitEndpoint(protocol string) string {
	if value := os.Getenv(providerBaseURLEnv(protocol)); value != "" {
		return value
	}
	return DefaultBaseURL(protocol)
}

// applyEndpointOverrides applies a role's protocol and endpoint overrides, in
// that order, and reports whether the protocol override moved the role from
// one provider's endpoint to another's. An implicit endpoint follows the new
// protocol to its default or ambient base URL; an explicit endpoint moves only
// when it is the previous protocol's public default. An endpoint override
// makes the endpoint explicit, so the role is no longer on a provider's
// endpoint that it did not choose.
func applyEndpointOverrides(role *ResolvedRole, baseURL, protocol *string) bool {
	crossedProvider := false
	if protocol != nil {
		previous := DefaultBaseURL(role.Protocol)
		role.Protocol = *protocol
		if next := DefaultBaseURL(*protocol); next != previous {
			switch {
			case !role.ExplicitEndpoint:
				role.BaseURL = implicitEndpoint(*protocol)
				crossedProvider = true
			case sameEndpoint(role.BaseURL, previous):
				role.BaseURL = next
				crossedProvider = true
			}
		}
	}
	if baseURL != nil {
		role.BaseURL, role.ExplicitEndpoint = *baseURL, true
		crossedProvider = false
	}
	return crossedProvider
}

// OverrideEndpoint applies command-line endpoint and protocol overrides with
// the same rules as ROAST_<ROLE>_BASE_URL and ROAST_<ROLE>_PROTOCOL, then
// re-resolves the credential. A nil override is unset.
func OverrideEndpoint(roleName string, role ResolvedRole, baseURL, protocol *string) ResolvedRole {
	return rebindCredential(roleName, role, applyEndpointOverrides(&role, baseURL, protocol))
}

// rebindCredential re-resolves a role's credential after its endpoint or
// protocol changed. A role-scoped key or the profile's own credential follows
// an explicit endpoint override. An ambient provider key is checked again
// against the new endpoint and protocol. When crossedProvider is true, the
// profile's credential stays with the provider it was configured for.
func rebindCredential(roleName string, role ResolvedRole, crossedProvider bool) ResolvedRole {
	// Subscription credentials can never become ambient API keys after an
	// endpoint/protocol flag. ValidateRole rejects the incompatible route.
	if IsOAuth(role.Auth) {
		role.APIKey = ""
		return role
	}
	switch role.Credential.Kind {
	case CredentialRoleEnv:
		return role
	case CredentialProfileEnv, CredentialProfileKey:
		if !crossedProvider {
			return role
		}
	}
	if value := os.Getenv(roleEnvPrefix(roleName) + "API_KEY"); value != "" {
		role.APIKey, role.Credential = value, CredentialSource{Kind: CredentialRoleEnv, Env: roleEnvPrefix(roleName) + "API_KEY"}
		return role
	}
	role.APIKey, role.Credential = ambientCredential(role.Protocol, role.BaseURL)
	return role
}

func roleEnvPrefix(roleName string) string {
	return "ROAST_" + strings.ToUpper(roleName) + "_"
}

func providerAPIKeyEnv(protocol string) string {
	if protocol == "anthropic" {
		return "ANTHROPIC_API_KEY"
	}
	return "OPENAI_API_KEY"
}

func providerBaseURLEnv(protocol string) string {
	if protocol == "anthropic" {
		return "ANTHROPIC_BASE_URL"
	}
	return "OPENAI_BASE_URL"
}

func pairedBaseURLEnv(keyVar string) string {
	return strings.TrimSuffix(keyVar, "API_KEY") + "BASE_URL"
}

// sameEndpoint compares endpoints by scheme, host, path without trailing
// slashes, and query. Unparseable values never match.
func sameEndpoint(a, b string) bool {
	left, errLeft := url.Parse(strings.TrimSpace(a))
	right, errRight := url.Parse(strings.TrimSpace(b))
	if errLeft != nil || errRight != nil || left.Host == "" || right.Host == "" {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host) &&
		strings.TrimRight(left.Path, "/") == strings.TrimRight(right.Path, "/") &&
		left.RawQuery == right.RawQuery && left.User.String() == right.User.String()
}

// AuthorModel returns the author model or family supplied by the invoking
// agent. A command-line override takes precedence in the CLI.
func AuthorModel() string {
	return os.Getenv("ROAST_AUTHOR_MODEL")
}

// InferFamily returns a provider-neutral model family when one can be
// determined from either a bare family or a common provider/model spelling.
func InferFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	if before, after, ok := strings.Cut(m, "/"); ok {
		switch before {
		case "anthropic", "openai", "google", "gemini", "mistral", "xai", "cohere", "deepseek":
			return canonicalFamily(before)
		}
		m = after
	}
	m = strings.TrimSuffix(strings.TrimSuffix(m, ":api"), "[1m]")
	switch {
	case m == "anthropic" || strings.HasPrefix(m, "claude-"):
		return "anthropic"
	case m == "openai" || strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "chatgpt-") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "openai"
	case m == "google" || m == "gemini" || strings.HasPrefix(m, "gemini-"):
		return "google"
	case m == "mistral" || strings.HasPrefix(m, "mistral-") || strings.HasPrefix(m, "codestral-"):
		return "mistral"
	case m == "xai" || strings.HasPrefix(m, "grok-"):
		return "xai"
	case m == "cohere" || strings.HasPrefix(m, "command-"):
		return "cohere"
	case m == "deepseek" || strings.HasPrefix(m, "deepseek-"):
		return "deepseek"
	default:
		return ""
	}
}

func canonicalFamily(family string) string {
	if family == "gemini" {
		return "google"
	}
	return family
}

// FlipResult describes author-aware finder selection.
type FlipResult struct {
	Role ResolvedRole
	Note string
	Warn string
}

// ResolveFinderFlip keeps finder generation independent from the author when
// the configured finder supplies a route for the author's model family.
func (c Config) ResolveFinderFlip(finder ResolvedRole, authorModel string) (FlipResult, error) {
	result := FlipResult{Role: finder}
	authorKey := strings.ToLower(strings.TrimSpace(authorModel))
	if authorKey == "" {
		return result, nil
	}
	authorFamily := InferFamily(authorModel)
	if finder.Pin {
		if authorFamily != "" && InferFamily(finder.Model) == authorFamily {
			result.Warn = fmt.Sprintf("author-model %s shares the finder family %s, but the finder is pinned; generation independence is reduced", authorModel, authorFamily)
		}
		return result, nil
	}
	targetName := finder.AuthorRoutes[authorKey]
	if targetName == "" && authorFamily != "" {
		targetName = finder.AuthorRoutes[authorFamily]
	}
	if targetName == "" {
		if authorFamily == "" {
			result.Warn = fmt.Sprintf("could not determine a model family for author-model %q and no exact author route is configured; keeping finder %s", authorModel, finder.Model)
		} else if InferFamily(finder.Model) == authorFamily {
			result.Warn = fmt.Sprintf("author-model %s shares the finder family %s and no author route is configured; generation independence is reduced", authorModel, authorFamily)
		}
		return result, nil
	}
	resolved, err := c.resolveProfile("finder", targetName, Role{})
	if err != nil {
		return result, err
	}
	targetFamily := InferFamily(resolved.Model)
	if (authorFamily != "" && (targetFamily == "" || targetFamily == authorFamily)) || strings.EqualFold(resolved.Model, authorModel) {
		result.Warn = fmt.Sprintf("author-model %s shares the finder family %s and profile %q uses non-independent model %q; keeping finder %s", authorModel, authorFamily, targetName, resolved.Model, finder.Model)
		return result, nil
	}
	result.Role = resolved
	if authorFamily == "" {
		result.Note = fmt.Sprintf("author-model %s matched an exact route; using finder profile %s (%s)", authorModel, targetName, resolved.Model)
	} else {
		result.Note = fmt.Sprintf("author-model %s maps to family %s; using finder profile %s (%s)", authorModel, authorFamily, targetName, resolved.Model)
	}
	return result, nil
}

// ValidateRole checks one fully-resolved role before any model call.
func ValidateRole(name string, role ResolvedRole) error {
	if strings.TrimSpace(role.Model) == "" {
		return fmt.Errorf("%s model is not configured (set inference_profiles.%s.model or ROAST_%s_MODEL)", name, role.Profile, strings.ToUpper(name))
	}
	if IsOAuth(role.Auth) {
		if err := validateOAuthRoute(role); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return validateEndpoint(name, role.BaseURL, role.Protocol, role.MaxOutputTokens)
	}
	if role.Auth != "" && role.Auth != AuthAPIKey {
		return fmt.Errorf("%s auth must be api_key, chatgpt, or codex", name)
	}
	if strings.TrimSpace(role.APIKey) == "" {
		roleVar := roleEnvPrefix(name) + "API_KEY"
		switch {
		case role.Credential.Kind == CredentialProfileEnv:
			return fmt.Errorf("%s API key is not configured (set %s, named by inference_profiles.%s.api_key_env, or %s)", name, role.Credential.Env, role.Profile, roleVar)
		case role.Credential.Withheld != "":
			return fmt.Errorf("%s API key is not configured: %s is not sent to %s because it is neither that provider's default endpoint nor %s (set inference_profiles.%s.api_key_env or %s)",
				name, role.Credential.Withheld, role.BaseURL, pairedBaseURLEnv(role.Credential.Withheld), role.Profile, roleVar)
		}
		return fmt.Errorf("%s API key is not configured (set inference_profiles.%s.api_key_env or %s)", name, role.Profile, roleVar)
	}
	return validateEndpoint(name, role.BaseURL, role.Protocol, role.MaxOutputTokens)
}

func validateEndpoint(name, baseURL, protocol string, maxOutputTokens int) error {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s endpoint must be an http(s) URL without credentials, a query, or a fragment", name)
	}
	switch protocol {
	case "chat", "responses", "anthropic":
	default:
		return fmt.Errorf("%s protocol must be chat, responses, or anthropic", name)
	}
	if maxOutputTokens <= 0 {
		return fmt.Errorf("%s max_output_tokens must be positive", name)
	}
	return nil
}
