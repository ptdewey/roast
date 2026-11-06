package config

import (
	"fmt"
	"sort"

	"go.yaml.in/yaml/v3"
)

type configShape int

const (
	shapeCurrent configShape = iota
	shapeLegacyRoles
	shapeDraftProfiles
)

// draftConfig is the unversioned named-profile schema used while the public
// configuration contract was being designed. Version 1 renamed base_url to
// endpoint so the service location is distinct from its wire protocol.
type draftConfig struct {
	InferenceProfiles map[string]draftProfile `yaml:"inference_profiles"`
	Roles             map[string]Role         `yaml:"roles"`
	Review            Review                  `yaml:"review"`
	Prompts           Prompts                 `yaml:"prompts"`
	Upload            Upload                  `yaml:"upload"`
}

type draftProfile struct {
	BaseURL         string `yaml:"base_url"`
	APIKey          string `yaml:"api_key"`
	APIKeyEnv       string `yaml:"api_key_env"`
	Model           string `yaml:"model"`
	Protocol        string `yaml:"protocol"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`
}

// legacyConfig is the role-inline schema from the original local Roast. It is
// accepted only when no version is declared, then converted to complete named
// profiles before ordinary version 1 validation runs.
type legacyConfig struct {
	Gateway legacyGateway         `yaml:"gateway"`
	Upload  legacyUpload          `yaml:"upload"`
	Roles   map[string]legacyRole `yaml:"roles"`
	Review  Review                `yaml:"review"`
	Prompts Prompts               `yaml:"prompts"`
	// Recognize the retired archive schema without carrying these values into
	// public configuration or restoring the associated integrations.
	Gate      legacyGate      `yaml:"gate"`
	Sourcebot legacySourcebot `yaml:"sourcebot"`
}

type legacyGate struct {
	AutoInstall *bool `yaml:"auto_install"`
}

type legacySourcebot struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type legacyGateway struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type legacyUpload struct {
	Enabled bool `yaml:"enabled"`
}

type legacyRole struct {
	BaseURL         string                `yaml:"base_url"`
	APIKey          string                `yaml:"api_key"`
	Model           string                `yaml:"model"`
	API             string                `yaml:"api"`
	MaxOutputTokens int                   `yaml:"max_output_tokens"`
	Pin             bool                  `yaml:"pin"`
	AuthorFlip      map[string]legacyRole `yaml:"author_flip"`
}

func unversionedShape(data []byte) (configShape, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return shapeCurrent, err
	}
	if len(document.Content) == 0 {
		return shapeCurrent, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return shapeCurrent, errorsForRoot(root)
	}
	if profiles := mappingValue(root, "inference_profiles"); profiles != nil {
		if nestedMappingHasKey(profiles, "base_url") {
			return shapeDraftProfiles, nil
		}
		return shapeCurrent, nil
	}
	// Archive installs could contain only a gateway credential and retired
	// settings, with role details supplied by their old embedded defaults.
	for _, key := range []string{"gateway", "gate", "sourcebot"} {
		if mappingValue(root, key) != nil {
			return shapeLegacyRoles, nil
		}
	}
	if roles := mappingValue(root, "roles"); roles != nil && nestedMappingHasAnyKey(roles,
		"base_url", "api_key", "model", "api", "max_output_tokens", "author_flip",
	) {
		return shapeLegacyRoles, nil
	}
	return shapeCurrent, nil
}

func errorsForRoot(root *yaml.Node) error {
	return fmt.Errorf("config root must be a mapping, got YAML kind %d", root.Kind)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func nestedMappingHasKey(mapping *yaml.Node, key string) bool {
	return nestedMappingHasAnyKey(mapping, key)
}

func nestedMappingHasAnyKey(mapping *yaml.Node, keys ...string) bool {
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for i := 1; i < len(mapping.Content); i += 2 {
		entry := mapping.Content[i]
		if entry.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(entry.Content); j += 2 {
			if _, ok := wanted[entry.Content[j].Value]; ok {
				return true
			}
		}
	}
	return false
}

func migrateDraft(draft draftConfig) Config {
	profiles := make(map[string]InferenceProfile, len(draft.InferenceProfiles))
	for name, profile := range draft.InferenceProfiles {
		profiles[name] = InferenceProfile{
			BaseURL: profile.BaseURL, APIKey: profile.APIKey, APIKeyEnv: profile.APIKeyEnv,
			Model: profile.Model, Protocol: profile.Protocol, MaxOutputTokens: profile.MaxOutputTokens,
		}
	}
	return Config{
		Version: CurrentVersion, InferenceProfiles: profiles, Roles: draft.Roles,
		Review: draft.Review, Prompts: draft.Prompts, Upload: draft.Upload,
	}
}

func migrateLegacy(legacy legacyConfig) Config {
	defaults := Defaults()
	cfg := Config{
		Version: CurrentVersion, InferenceProfiles: make(map[string]InferenceProfile),
		Roles: make(map[string]Role), Review: legacy.Review, Prompts: legacy.Prompts,
	}
	names := make([]string, 0, len(legacy.Roles))
	for name := range legacy.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		legacyRole := legacy.Roles[name]
		if legacyRole.BaseURL == "" {
			legacyRole.BaseURL = legacy.Gateway.BaseURL
		}
		if legacyRole.APIKey == "" {
			legacyRole.APIKey = legacy.Gateway.APIKey
		}
		cfg.InferenceProfiles[name] = profileFromLegacy(legacyRole)
		role := Role{Profile: name, Pin: legacyRole.Pin}
		routeKeys := make([]string, 0, len(legacyRole.AuthorFlip))
		for key := range legacyRole.AuthorFlip {
			routeKeys = append(routeKeys, key)
		}
		sort.Strings(routeKeys)
		if len(routeKeys) > 0 {
			role.AuthorRoutes = make(map[string]string, len(routeKeys))
		}
		for i, key := range routeKeys {
			profileName := fmt.Sprintf("%s-author-route-%d", name, i+1)
			cfg.InferenceProfiles[profileName] = profileFromLegacy(inheritLegacyRole(legacyRole, legacyRole.AuthorFlip[key]))
			role.AuthorRoutes[key] = profileName
		}
		cfg.Roles[name] = role
	}
	for _, name := range []string{"finder", "verifier"} {
		if _, ok := cfg.Roles[name]; ok {
			continue
		}
		cfg.InferenceProfiles[name] = defaults.InferenceProfiles[name]
		cfg.Roles[name] = defaults.Roles[name]
	}
	return cfg
}

func profileFromLegacy(role legacyRole) InferenceProfile {
	return InferenceProfile{
		BaseURL: role.BaseURL, APIKey: role.APIKey, Model: role.Model,
		Protocol: role.API, MaxOutputTokens: role.MaxOutputTokens,
	}
}

func inheritLegacyRole(base, override legacyRole) legacyRole {
	if override.BaseURL == "" {
		override.BaseURL = base.BaseURL
	}
	if override.APIKey == "" {
		override.APIKey = base.APIKey
	}
	if override.API == "" {
		override.API = base.API
	}
	if override.MaxOutputTokens == 0 {
		override.MaxOutputTokens = base.MaxOutputTokens
	}
	return override
}
