package cli

import (
	"path/filepath"
	"sort"
	"strings"
)

const (
	evidenceBundleFileName           = "evidence.json"
	evidenceManifestFileName         = "manifest.json"
	evidenceEventsFileName           = "events.jsonl"
	evidenceLogPath                  = "logs/session.log"
	evidenceScenarioResultFileName   = "scenario-result.json"
	evidencePublishedManifestName    = "artifact-manifest.json"
	evidenceReviewCommentName        = "review-comment.md"
	evidenceScreenshotsDir           = "screenshots"
	evidenceRecordingsDir            = "recordings"
	evidenceDefaultScreenshotName    = "screenshot.png"
	evidenceDefaultRecordingName     = "recording.mp4"
	evidenceStandaloneResultName     = "scenario-result.json"
	evidenceStandaloneScenarioPrefix = "manual-"
)

type evidenceArtifact struct {
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Origin    string `json:"origin,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

func buildEvidenceBundleCatalog(bundle evidenceBundle) []evidenceArtifact {
	var artifacts []evidenceArtifact
	add := func(artifact evidenceArtifact) {
		artifact.Path = cleanEvidenceArtifactPath(artifact.Path)
		if artifact.Path == "" {
			return
		}
		if artifact.MediaType == "" {
			artifact.MediaType = mediaTypeForPath(artifact.Path)
		}
		artifacts = append(artifacts, artifact)
	}
	add(evidenceArtifact{Label: "metadata", Kind: "metadata", Path: evidenceBundleFileName, MediaType: "application/json"})
	add(evidenceArtifact{Label: "metadata", Kind: "metadata", Path: bundle.Manifest, MediaType: "application/json"})
	add(evidenceArtifact{Label: "metadata", Kind: "metadata", Path: bundle.ScenarioResult, MediaType: "application/json"})
	if bundle.Report != nil {
		add(*bundle.Report)
	}
	add(evidenceArtifact{Label: "logs", Kind: "events", Path: bundle.Events, MediaType: "application/x-ndjson"})
	add(evidenceArtifact{Label: "logs", Kind: "log", Path: bundle.Log, MediaType: "text/plain"})
	for _, artifact := range bundle.VisualEvidence {
		add(evidenceArtifact{
			Label:     "Visual Evidence",
			Kind:      artifact.Kind,
			Path:      artifact.Path,
			MediaType: artifact.MediaType,
			Origin:    artifact.Origin,
			SHA256:    artifact.SHA256,
		})
	}
	for _, output := range bundle.Outputs {
		add(evidenceArtifact{Label: "outputs", Kind: "output", Path: output.Path, MediaType: output.MediaType})
	}
	return sortEvidenceArtifactCatalog(dedupeEvidenceArtifactCatalog(artifacts))
}

func evidenceBundleCatalog(bundle evidenceBundle) []evidenceArtifact {
	var artifacts []evidenceArtifact
	if bundle.Report != nil {
		artifacts = append(artifacts, *bundle.Report)
	}
	artifacts = append(artifacts, bundle.Artifacts...)
	artifacts = append(artifacts, buildEvidenceBundleCatalog(bundle)...)
	return sortEvidenceArtifactCatalog(dedupeEvidenceArtifactCatalog(artifacts))
}

func dedupeEvidenceArtifactCatalog(artifacts []evidenceArtifact) []evidenceArtifact {
	seen := make(map[string]bool)
	catalog := make([]evidenceArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifact.Path = cleanEvidenceArtifactPath(artifact.Path)
		if artifact.Path == "" || seen[artifact.Path] {
			continue
		}
		if artifact.MediaType == "" {
			artifact.MediaType = mediaTypeForPath(artifact.Path)
		}
		seen[artifact.Path] = true
		catalog = append(catalog, artifact)
	}
	return catalog
}

func sortEvidenceArtifactCatalog(artifacts []evidenceArtifact) []evidenceArtifact {
	sort.SliceStable(artifacts, func(i, j int) bool {
		if artifacts[i].Label != artifacts[j].Label {
			return artifacts[i].Label < artifacts[j].Label
		}
		return artifacts[i].Path < artifacts[j].Path
	})
	return artifacts
}

func cleanEvidenceArtifactPath(path string) string {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func isReservedEvidenceArtifactPath(path string) bool {
	slashed := strings.ToLower(filepath.ToSlash(path))
	for _, reserved := range []string{
		evidenceBundleFileName,
		evidenceManifestFileName,
		evidenceEventsFileName,
		evidenceScenarioResultFileName,
		evidenceReportFileName,
		evidencePublishedManifestName,
		evidenceReviewCommentName,
		"logs",
		evidenceScreenshotsDir,
		evidenceRecordingsDir,
	} {
		if slashed == reserved || strings.HasPrefix(slashed, reserved+"/") {
			return true
		}
	}
	return false
}

func mediaTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "application/json"
	case ".txt", ".log":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}
