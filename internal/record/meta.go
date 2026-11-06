package record

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Meta is the secret-free replay contract for one review run. Prompts,
// reviewed input, and results live beside it as compressed artifacts.
type Meta struct {
	RunID    string   `json:"run_id"`
	ReplayOf string   `json:"replay_of,omitempty"`
	TS       string   `json:"ts"`
	Repo     string   `json:"repo"`
	InvokeIn string   `json:"invoke_dir,omitempty"`
	Argv     []string `json:"argv,omitempty"`

	Roast    BuildMeta            `json:"roast"`
	Scope    ScopeMeta            `json:"scope"`
	Derived  DerivedMeta          `json:"derived"`
	Roles    map[string]RoleMeta  `json:"roles"`
	Snapshot SnapshotMeta         `json:"snapshot"`
	Digests  DigestMeta           `json:"digests"`
	Budgets  map[string]any       `json:"budgets,omitempty"`
	Usage    map[string]UsageMeta `json:"usage,omitempty"`

	Env          map[string]string `json:"env,omitempty"`
	EnvNamesOnly []string          `json:"env_names_only,omitempty"`
	Caveats      []string          `json:"caveats,omitempty"`
}

type BuildMeta struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type ScopeMeta struct {
	Kind            string   `json:"kind"`
	Base            string   `json:"base,omitempty"`
	Revision        string   `json:"revision,omitempty"`
	UncommittedOnly bool     `json:"uncommitted_only,omitempty"`
	StagedOnly      bool     `json:"staged_only,omitempty"`
	IncludeVendored bool     `json:"include_vendored,omitempty"`
	Paths           []string `json:"paths,omitempty"`
	Excludes        []string `json:"excludes,omitempty"`
	BaseOID         string   `json:"base_oid,omitempty"`
	MergeBaseOID    string   `json:"merge_base_oid,omitempty"`
	HeadOID         string   `json:"head_oid,omitempty"`
	Survey          bool     `json:"survey,omitempty"`
	Staged          bool     `json:"staged,omitempty"`
	Files           []string `json:"files,omitempty"`
}

type DerivedMeta struct {
	Mode            string   `json:"mode"`
	Langs           []string `json:"langs,omitempty"`
	Quick           bool     `json:"quick,omitempty"`
	Interactive     bool     `json:"interactive"`
	MaxTurns        int      `json:"max_turns"`
	ConfidenceFloor float64  `json:"confidence_floor"`
	DoVerify        bool     `json:"do_verify"`
	Task            string   `json:"task,omitempty"`
	MaxSeverity     string   `json:"max_severity,omitempty"`
	MaxDiffBytes    int      `json:"max_diff_bytes,omitempty"`
	MaxFileCtxBytes int      `json:"max_file_ctx_bytes,omitempty"`
}

// RoleMeta records the resolved non-secret inference profile.
type RoleMeta struct {
	Profile         string `json:"profile,omitempty"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	Protocol        string `json:"protocol"`
	API             string `json:"api,omitempty"` // pre-profile run compatibility
	MaxOutputTokens int    `json:"max_output_tokens"`
	PromptDigest    string `json:"prompt_digest,omitempty"`
}

// WireProtocol returns the current protocol field or the legacy api field.
func (r RoleMeta) WireProtocol() string {
	if r.Protocol != "" {
		return r.Protocol
	}
	return r.API
}

type SnapshotMeta struct {
	Captured      bool     `json:"captured"`
	Reason        string   `json:"reason,omitempty"`
	Ref           string   `json:"ref,omitempty"`
	Commit        string   `json:"commit,omitempty"`
	IndexTree     string   `json:"index_tree,omitempty"`
	WorktreeTree  string   `json:"worktree_tree,omitempty"`
	BaseTree      string   `json:"base_tree,omitempty"`
	Head          string   `json:"head,omitempty"`
	Base          string   `json:"base,omitempty"`
	DiffPair      string   `json:"diff_pair,omitempty"`
	FileModes     string   `json:"file_modes,omitempty"`
	ExcludedPaths []string `json:"excluded_paths,omitempty"`
}

type DigestMeta struct {
	InputKind  string `json:"input_kind"`
	Input      string `json:"input"`
	InputBytes int    `json:"input_bytes"`
	System     string `json:"system_prompt,omitempty"`
	User       string `json:"user_prompt,omitempty"`
}

type UsageMeta struct {
	UncachedInputTokens int `json:"uncached_input_tokens"`
	CachedInputTokens   int `json:"cached_input_tokens"`
	CacheWriteTokens    int `json:"cache_write_tokens"`
	OutputTokens        int `json:"output_tokens"`
	Calls               int `json:"calls"`
}

const MetaFile = "meta.json"

func Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RunDir resolves a full run id or unique prefix.
func RunDir(id string) (string, error) {
	if id == "" || filepath.Base(id) != id || id == "." {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	dir := DefaultDir()
	if dir == "" {
		return "", fmt.Errorf("cannot resolve record dir; set ROAST_RECORD_DIR")
	}
	root := filepath.Join(dir, "runs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("no run artifacts in %s: %w", dir, err)
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), id) {
			matches = append(matches, entry.Name())
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no run matches %q under %s", id, root)
	case 1:
		return filepath.Join(root, matches[0]), nil
	default:
		return "", fmt.Errorf("%q is ambiguous: matches %s", id, strings.Join(matches, ", "))
	}
}

func WriteMetaRequired(meta Meta) (string, error) {
	dir := DefaultDir()
	if dir == "" {
		return "", fmt.Errorf("resolve record directory; set ROAST_RECORD_DIR to a writable directory")
	}
	if meta.RunID == "" || filepath.Base(meta.RunID) != meta.RunID || meta.RunID == "." {
		return "", fmt.Errorf("write meta: invalid run id %q", meta.RunID)
	}
	runDir, err := makeRunDir(dir, meta.RunID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	path := filepath.Join(runDir, MetaFile)
	tmp := path + ".tmp"
	if err := writePrivateFile(tmp, append(data, '\n')); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("publish %s: %w", path, err)
	}
	return path, nil
}

// WriteMeta is the best-effort compatibility form used by fixture builders.
func WriteMeta(meta Meta) string {
	path, _ := WriteMetaRequired(meta)
	return path
}

func ReadMeta(id string) (Meta, string, error) {
	runDir, err := RunDir(id)
	if err != nil {
		return Meta{}, "", err
	}
	path := filepath.Join(runDir, MetaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, runDir, fmt.Errorf("run %s has no %s: %w", filepath.Base(runDir), MetaFile, err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, runDir, fmt.Errorf("parse %s: %w", path, err)
	}
	return meta, runDir, nil
}

// RemoveRun removes an incomplete run directory before inference starts.
func RemoveRun(runID string) error {
	if runID == "" || filepath.Base(runID) != runID || runID == "." {
		return fmt.Errorf("remove partial run: invalid run id %q", runID)
	}
	dir := DefaultDir()
	if dir == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(dir, "runs", runID))
}

func WriteArtifactRequired(runID, name, content string) (string, error) {
	if runID == "" || filepath.Base(runID) != runID || runID == "." {
		return "", fmt.Errorf("write artifact: invalid run id %q", runID)
	}
	if name == "" || filepath.Base(name) != name || name == "." {
		return "", fmt.Errorf("write artifact: invalid name %q", name)
	}
	dir := DefaultDir()
	if dir == "" {
		return "", fmt.Errorf("resolve record directory; set ROAST_RECORD_DIR to a writable directory")
	}
	runDir, err := makeRunDir(dir, runID)
	if err != nil {
		return "", err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(content)); err != nil {
		return "", fmt.Errorf("compress artifact %q: %w", name, err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("finish artifact %q: %w", name, err)
	}
	path := filepath.Join(runDir, name+".gz")
	if err := writePrivateFile(path, compressed.Bytes()); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// WriteArtifact is the best-effort compatibility form used by local tools.
func WriteArtifact(runID, name, content string) string {
	if content == "" {
		return ""
	}
	path, _ := WriteArtifactRequired(runID, name, content)
	return path
}

func ReadArtifact(runDir, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || name == "." {
		return "", fmt.Errorf("read artifact: invalid name %q", name)
	}
	path := filepath.Join(runDir, name+".gz")
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

var envSecretNames = map[string]struct{}{
	"ROAST_FINDER_API_KEY":   {},
	"ROAST_VERIFIER_API_KEY": {},
}

var envAllowExact = map[string]struct{}{
	"ROAST_RECORD_DIR": {},
	"ROAST_DEADLINE":   {},
	"ROAST_SNAPSHOT":   {},
}

// CaptureEnv records only non-secret tuning variables. Unknown ROAST_ names
// are visible without their values so new settings cannot leak by accident.
func CaptureEnv(additionalSecrets ...string) (map[string]string, []string) {
	secrets := make(map[string]struct{}, len(envSecretNames)+len(additionalSecrets))
	for name := range envSecretNames {
		secrets[name] = struct{}{}
	}
	for _, name := range additionalSecrets {
		secrets[name] = struct{}{}
	}
	values := map[string]string{}
	var namesOnly []string
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if !ok || !strings.HasPrefix(name, "ROAST_") {
			continue
		}
		if _, secret := secrets[name]; secret || strings.HasSuffix(name, "_API_KEY") {
			continue
		}
		_, exact := envAllowExact[name]
		if exact || strings.HasPrefix(name, "ROAST_MAX_") || strings.HasPrefix(name, "ROAST_INTERACTIVE_MIN_") {
			values[name] = value
		} else {
			namesOnly = append(namesOnly, name)
		}
	}
	sort.Strings(namesOnly)
	if len(values) == 0 {
		values = nil
	}
	return values, namesOnly
}
