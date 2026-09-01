package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type evidencePublishOptions struct {
	Destination string
	BaseURL     string
	BundleDir   string
}

type publishedEvidence struct {
	RunID        string
	PublishedDir string
	ManifestPath string
	MarkdownPath string
	URL          string
}

type publishedArtifactManifest struct {
	RunID         string              `json:"runId"`
	Scenario      string              `json:"scenario"`
	Status        string              `json:"status"`
	TargetProfile string              `json:"targetProfile"`
	Host          string              `json:"host"`
	BaseURL       string              `json:"baseUrl"`
	ReportPath    string              `json:"reportPath,omitempty"`
	Artifacts     []publishedArtifact `json:"artifacts"`
}

type publishedArtifact struct {
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	URL       string `json:"url"`
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Origin    string `json:"origin,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

func parseEvidencePublishArgs(args []string) (evidencePublishOptions, error) {
	var options evidencePublishOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--destination":
			i++
			if i >= len(args) || args[i] == "" {
				return evidencePublishOptions{}, newUsageError("--destination needs a filesystem Evidence Store path")
			}
			options.Destination = args[i]
		case "--base-url":
			i++
			if i >= len(args) || args[i] == "" {
				return evidencePublishOptions{}, newUsageError("--base-url needs a URL")
			}
			options.BaseURL = args[i]
		default:
			if strings.HasPrefix(arg, "-") {
				return evidencePublishOptions{}, newUsageError("unknown evidence publish option %q", arg)
			}
			if options.BundleDir != "" {
				return evidencePublishOptions{}, newUsageError("evidence publish needs one Evidence Bundle directory")
			}
			options.BundleDir = arg
		}
	}
	if options.Destination == "" {
		return evidencePublishOptions{}, newUsageError("evidence publish needs --destination")
	}
	if options.BaseURL == "" {
		return evidencePublishOptions{}, newUsageError("evidence publish needs --base-url")
	}
	if _, err := parseBaseURL(options.BaseURL); err != nil {
		return evidencePublishOptions{}, err
	}
	if options.BundleDir == "" {
		return evidencePublishOptions{}, newUsageError("evidence publish needs an Evidence Bundle directory")
	}
	return options, nil
}

func publishEvidenceBundle(repoDir string, options evidencePublishOptions) (publishedEvidence, error) {
	bundleDir := resolveFromRepo(repoDir, options.BundleDir)
	destinationRoot := resolveFromRepo(repoDir, options.Destination)
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		return publishedEvidence{}, err
	}
	if bundle.RunID == "" {
		bundle.RunID = filepath.Base(bundleDir)
	}
	if err := validateRunID(bundle.RunID); err != nil {
		return publishedEvidence{}, err
	}

	publishedDir := filepath.Join(destinationRoot, bundle.RunID)
	overlap, err := pathsOverlap(bundleDir, publishedDir)
	if err != nil {
		return publishedEvidence{}, err
	}
	if overlap {
		return publishedEvidence{}, fmt.Errorf("Evidence Store destination must not overlap the source Evidence Bundle")
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if err := replacePublishedDir(bundleDir, publishedDir, func(stagingDir string) error {
		manifest, err := buildPublishedArtifactManifest(stagingDir, bundle, baseURL)
		if err != nil {
			return err
		}
		if err := writeJSONFile(filepath.Join(stagingDir, "artifact-manifest.json"), manifest); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(stagingDir, "review-comment.md"), []byte(renderReviewMarkdown(bundle, manifest)), 0o644)
	}); err != nil {
		return publishedEvidence{}, err
	}
	detailPath := "review-comment.md"
	if bundle.Report != nil {
		detailPath = bundle.Report.Path
	}
	publishedURL, err := artifactURL(baseURL, bundle.RunID, detailPath)
	if err != nil {
		return publishedEvidence{}, err
	}
	return publishedEvidence{
		RunID:        bundle.RunID,
		PublishedDir: publishedDir,
		ManifestPath: filepath.Join(publishedDir, "artifact-manifest.json"),
		MarkdownPath: filepath.Join(publishedDir, "review-comment.md"),
		URL:          publishedURL,
	}, nil
}

func readEvidenceBundleFile(bundleDir string) (evidenceBundle, error) {
	return readEvidenceBundleFileWithStaleReport(bundleDir, false)
}

func readEvidenceBundleFileWithStaleReport(bundleDir string, allowStaleReport bool) (evidenceBundle, error) {
	evidencePath, ok := safeBundlePath(bundleDir, evidenceBundleFileName)
	if !ok {
		return evidenceBundle{}, fmt.Errorf("evidence bundle metadata path is unsafe")
	}
	info, err := os.Lstat(evidencePath)
	if err != nil || !info.Mode().IsRegular() {
		return evidenceBundle{}, errors.Join(fmt.Errorf("evidence bundle metadata must be a regular file"), err)
	}
	content, err := os.ReadFile(evidencePath)
	if err != nil {
		return evidenceBundle{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return evidenceBundle{}, fmt.Errorf("parse Evidence Bundle: %w", err)
	}
	if bundle.RunID != "" {
		if err := validateRunID(bundle.RunID); err != nil {
			return evidenceBundle{}, err
		}
	}
	if bundle.Manifest != evidenceManifestFileName {
		return evidenceBundle{}, fmt.Errorf("evidence bundle must use canonical manifest path %s", evidenceManifestFileName)
	}
	manifestPath, ok := safeBundlePath(bundleDir, bundle.Manifest)
	if !ok {
		return evidenceBundle{}, fmt.Errorf("evidence bundle manifest path is missing or unsafe")
	}
	info, err = os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() {
		return evidenceBundle{}, errors.Join(fmt.Errorf("evidence bundle manifest must be a regular file"), err)
	}
	manifestContent, err := os.ReadFile(manifestPath)
	if err != nil {
		return evidenceBundle{}, err
	}
	var manifest runManifest
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		return evidenceBundle{}, fmt.Errorf("parse Evidence Bundle manifest: %w", err)
	}
	reportAuthorized := manifest.Report != nil
	// Manifest-authorized reports always require a bound run identity. Recovery
	// may ignore only a physical report leaf that has no manifest authority.
	requireIdentity := reportAuthorized
	reportExists := false
	if reportInfo, reportErr := os.Lstat(filepath.Join(bundleDir, evidenceReportFileName)); reportErr == nil {
		reportExists = true
		if !reportInfo.Mode().IsRegular() {
			return evidenceBundle{}, fmt.Errorf("evidence bundle report must be a regular file")
		}
	} else if !errors.Is(reportErr, os.ErrNotExist) {
		return evidenceBundle{}, reportErr
	}
	if reportExists && !reportAuthorized && !allowStaleReport {
		return evidenceBundle{}, fmt.Errorf("evidence bundle report exists without manifest authority")
	}
	if err := validateEvidenceManifestIdentity(bundle, manifest, requireIdentity); err != nil {
		return evidenceBundle{}, err
	}
	bundle.Report = manifest.Report
	return bundle, nil
}

func validateEvidenceManifestIdentity(bundle evidenceBundle, manifest runManifest, required bool) error {
	if required && (manifest.RunID == "" || manifest.Scenario == "") {
		return fmt.Errorf("evidence bundle manifest identifies another run")
	}
	if manifest.RunID != "" && manifest.RunID != bundle.RunID ||
		manifest.Scenario != "" && manifest.Scenario != bundle.Scenario ||
		manifest.TargetProfile != "" && manifest.TargetProfile != bundle.TargetProfile ||
		manifest.Host != "" && manifest.Host != bundle.Host {
		return fmt.Errorf("evidence bundle manifest identifies another run")
	}
	return nil
}

func replacePublishedDir(bundleDir string, publishedDir string, populate func(stagingDir string) error) error {
	info, err := os.Lstat(publishedDir)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("published Evidence Bundle destination %s must not be a symlink", publishedDir)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	publishedExists := err == nil
	parent := filepath.Dir(publishedDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stagingDir, err := os.MkdirTemp(parent, "."+filepath.Base(publishedDir)+".tmp-")
	if err != nil {
		return err
	}
	if err := copyPath(bundleDir, stagingDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		return fmt.Errorf("copy Evidence Bundle into staging directory: %w", err)
	}
	if err := populate(stagingDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		return err
	}
	if !publishedExists {
		return os.Rename(stagingDir, publishedDir)
	}
	backupDir, err := os.MkdirTemp(parent, "."+filepath.Base(publishedDir)+".backup-")
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		return err
	}
	if err := os.Remove(backupDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		return err
	}
	if err := os.Rename(publishedDir, backupDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		return err
	}
	if err := os.Rename(stagingDir, publishedDir); err != nil {
		_ = os.Rename(backupDir, publishedDir)
		_ = os.RemoveAll(stagingDir)
		return err
	}
	return os.RemoveAll(backupDir)
}

func buildPublishedArtifactManifest(publishedDir string, bundle evidenceBundle, baseURL string) (publishedArtifactManifest, error) {
	if bundle.Report != nil {
		if bundle.Report.Path != evidenceReportFileName || bundle.Report.Kind != "report" || bundle.Report.MediaType != reportMediaType {
			return publishedArtifactManifest{}, fmt.Errorf("evidence bundle report metadata is invalid")
		}
		if err := verifyReportArtifact(filepath.Join(publishedDir, evidenceReportFileName), *bundle.Report); err != nil {
			return publishedArtifactManifest{}, err
		}
		view, err := buildEvidenceReportView(publishedDir, bundle)
		if err != nil {
			return publishedArtifactManifest{}, fmt.Errorf("rebuild Evidence Bundle report for publication: %w", err)
		}
		expected, err := renderEvidenceReportHTML(view)
		if err != nil {
			return publishedArtifactManifest{}, fmt.Errorf("render Evidence Bundle report for publication: %w", err)
		}
		actual, err := os.ReadFile(filepath.Join(publishedDir, evidenceReportFileName))
		if err != nil {
			return publishedArtifactManifest{}, err
		}
		if !bytes.Equal(actual, expected) {
			return publishedArtifactManifest{}, fmt.Errorf("evidence bundle report does not project current canonical evidence")
		}
	}
	var artifacts []publishedArtifact
	add := func(artifact evidenceArtifact) error {
		label := artifact.Label
		kind := artifact.Kind
		path := artifact.Path
		mediaType := artifact.MediaType
		if path == "" {
			return nil
		}
		if artifact.Kind == "report" && (bundle.Report == nil || artifact.Path != bundle.Report.Path) {
			return fmt.Errorf("evidence bundle contains an unverified report artifact %s", artifact.Path)
		}
		clean, err := cleanPublishedRelativePath(path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(publishedDir, clean)); err != nil {
			return err
		}
		if bundle.Report != nil && artifact.Path == bundle.Report.Path {
			if err := verifyReportArtifact(filepath.Join(publishedDir, clean), artifact); err != nil {
				return err
			}
		}
		artifactURL, err := artifactURL(baseURL, bundle.RunID, clean)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, publishedArtifact{
			Label:     label,
			Kind:      kind,
			Path:      filepath.ToSlash(clean),
			URL:       artifactURL,
			MediaType: mediaType,
			Size:      artifact.Size,
			Origin:    artifact.Origin,
			SHA256:    artifact.SHA256,
		})
		return nil
	}
	for _, artifact := range evidenceBundleCatalog(bundle) {
		if err := add(artifact); err != nil {
			return publishedArtifactManifest{}, err
		}
	}
	sort.SliceStable(artifacts, func(i, j int) bool {
		if artifacts[i].Label != artifacts[j].Label {
			return artifacts[i].Label < artifacts[j].Label
		}
		return artifacts[i].Path < artifacts[j].Path
	})
	manifest := publishedArtifactManifest{
		RunID:         bundle.RunID,
		Scenario:      bundle.Scenario,
		Status:        bundle.Status,
		TargetProfile: bundle.TargetProfile,
		Host:          bundle.Host,
		BaseURL:       baseURL,
		Artifacts:     artifacts,
	}
	if bundle.Report != nil {
		manifest.ReportPath = bundle.Report.Path
	}
	return manifest, nil
}

func renderReviewMarkdown(bundle evidenceBundle, manifest publishedArtifactManifest) string {
	if manifest.TargetProfile == "" {
		manifest.TargetProfile = bundle.TargetProfile
	}
	if manifest.Host == "" {
		manifest.Host = bundle.Host
	}
	return renderReviewMarkdownFromManifest(manifest)
}

func renderReviewMarkdownFromManifest(manifest publishedArtifactManifest) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "<!-- maya-stall:evidence-comment -->\n")
	fmt.Fprintf(&builder, "## Maya Stall Evidence\n\n")
	fmt.Fprintf(&builder, "status: %s\n", markdownText(manifest.Status))
	fmt.Fprintf(&builder, "run: %s\n", markdownText(manifest.RunID))
	fmt.Fprintf(&builder, "scenario: %s\n", markdownText(manifest.Scenario))
	fmt.Fprintf(&builder, "targetProfile: %s\n", markdownText(manifest.TargetProfile))
	fmt.Fprintf(&builder, "host: %s\n\n", markdownText(manifest.Host))
	for _, artifact := range manifest.Artifacts {
		if manifest.ReportPath != "" && artifact.Path == manifest.ReportPath {
			fmt.Fprintf(&builder, "details: [%s](<%s>)\n\n", markdownLinkText(artifact.Path), markdownLinkDestination(artifact.URL))
			break
		}
	}
	for _, artifact := range manifest.Artifacts {
		fmt.Fprintf(&builder, "- %s: [%s](<%s>)\n", markdownText(artifact.Label), markdownLinkText(artifact.Path), markdownLinkDestination(artifact.URL))
	}
	return builder.String()
}

func verifyReportArtifact(path string, artifact evidenceArtifact) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("report artifact %s must be a regular file", artifact.Path)
	}
	if artifact.Size <= 0 || info.Size() != artifact.Size {
		return fmt.Errorf("report artifact %s size is %d; manifest records %d", artifact.Path, info.Size(), artifact.Size)
	}
	hash, err := fileSHA256Hex(path)
	if err != nil {
		return err
	}
	if artifact.SHA256 == "" || hash != artifact.SHA256 {
		return fmt.Errorf("report artifact %s SHA-256 does not match the manifest", artifact.Path)
	}
	return nil
}

func artifactURL(baseURL string, runID string, relativePath string) (string, error) {
	joined, err := url.JoinPath(baseURL, runID, filepath.ToSlash(relativePath))
	if err != nil {
		return "", err
	}
	return strings.NewReplacer(
		"[", "%5B",
		"]", "%5D",
		"(", "%28",
		")", "%29",
		"<", "%3C",
		">", "%3E",
	).Replace(joined), nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, newUsageError("invalid --base-url %q: %v", raw, err)
	}
	if parsed.Scheme == "" {
		return nil, newUsageError("--base-url must include a URL scheme")
	}
	return parsed, nil
}

func resolveFromRepo(repoDir string, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(repoDir, path)
}

func pathsOverlap(left string, right string) (bool, error) {
	leftPath, err := canonicalPathForOverlap(left)
	if err != nil {
		return false, err
	}
	rightPath, err := canonicalPathForOverlap(right)
	if err != nil {
		return false, err
	}
	return pathContainsOrSame(leftPath, rightPath) || pathContainsOrSame(rightPath, leftPath), nil
}

func canonicalPathForOverlap(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return filepath.Clean(absolute), nil
	}
	resolvedParent, err := canonicalPathForOverlap(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(absolute)), nil
}

func pathContainsOrSame(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func cleanPublishedRelativePath(path string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("published artifact path %q must be relative", path)
	}
	return clean, nil
}

func markdownText(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"]", "\\]",
		"[", "\\[",
		")", "\\)",
		"(", "\\(",
		"\n", " ",
		"\r", " ",
	).Replace(value)
}

func markdownLinkText(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"]", "\\]",
		"[", "\\[",
		")", "\\)",
		"(", "\\(",
		"\n", " ",
		"\r", " ",
	).Replace(value)
}

func markdownLinkDestination(value string) string {
	return strings.NewReplacer(
		">", "%3E",
		"\n", "%0A",
		"\r", "%0D",
		" ", "%20",
	).Replace(value)
}
