package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCommentMarkdownRendersFromPublishedManifest(t *testing.T) {
	manifest := publishedArtifactManifest{
		RunID:         "20260704T120000.000000000Z",
		Scenario:      "smoke",
		Status:        "passed",
		TargetProfile: "ci",
		Host:          "fake-local",
		BaseURL:       "https://evidence.example.test/maya",
		Artifacts: []publishedArtifact{
			{Label: "Visual Evidence", Kind: "screenshot", Path: "screenshots/smoke.png", URL: "https://evidence.example.test/maya/20260704T120000.000000000Z/screenshots/smoke.png", MediaType: "image/png"},
			{Label: "logs", Kind: "log", Path: "logs/session.log", URL: "https://evidence.example.test/maya/20260704T120000.000000000Z/logs/session.log", MediaType: "text/plain"},
		},
	}

	markdown := renderReviewMarkdownFromManifest(manifest)

	for _, want := range []string{
		"<!-- maya-stall:evidence-comment -->",
		"status: passed",
		"run: 20260704T120000.000000000Z",
		"scenario: smoke",
		"targetProfile: ci",
		"host: fake-local",
		"[screenshots/smoke.png](<https://evidence.example.test/maya/20260704T120000.000000000Z/screenshots/smoke.png>)",
		"[logs/session.log](<https://evidence.example.test/maya/20260704T120000.000000000Z/logs/session.log>)",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("review markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestGitHubReviewCommentShapesCreateAndUpdate(t *testing.T) {
	markdown := "<!-- maya-stall:evidence-comment -->\n## Maya Stall Evidence\n"
	options := githubReviewCommentOptions{
		Repo:        "BramVR/maya_stall",
		PullRequest: 12,
		Token:       "fake-github-token",
	}
	marker := reviewCommentMarkerForTarget("github", "BramVR/maya_stall#12")
	postedMarkdown := reviewMarkdownForPost(markdown, marker)

	t.Run("create", func(t *testing.T) {
		api := &fakeReviewCommentAPI{
			responses: []reviewCommentAPIResponse{
				{StatusCode: 403, Body: []byte(`{"message":"Resource not accessible by integration"}`)},
				{StatusCode: 200, Body: []byte(`[{"id":100,"body":"` + marker + `\nforged","user":{"login":"someone-else"}}]`)},
				{StatusCode: 201, Body: []byte(`{"id":101}`)},
			},
		}

		result, err := postGitHubReviewComment(options, markdown, api)
		if err != nil {
			t.Fatalf("postGitHubReviewComment returned error: %v", err)
		}
		if result.Operation != "create" || result.CommentID != "101" {
			t.Fatalf("result = %+v, want create comment 101", result)
		}
		if len(api.requests) != 3 {
			t.Fatalf("request count = %d, want 3", len(api.requests))
		}
		assertReviewAPIRequest(t, api.requests[0], "GET", "https://api.github.com/user", "")
		assertReviewAPIRequest(t, api.requests[1], "GET", "https://api.github.com/repos/BramVR/maya_stall/issues/12/comments?per_page=100", "")
		assertReviewAPIRequest(t, api.requests[2], "POST", "https://api.github.com/repos/BramVR/maya_stall/issues/12/comments", postedMarkdown)
		if got := api.requests[2].Headers["Authorization"]; got != "Bearer fake-github-token" {
			t.Fatalf("GitHub auth header = %q", got)
		}
	})

	t.Run("update", func(t *testing.T) {
		api := &fakeReviewCommentAPI{
			responses: []reviewCommentAPIResponse{
				{StatusCode: 200, Body: []byte(`{"login":"maya-stall-bot"}`)},
				{StatusCode: 200, Body: []byte(`[{"id":202,"body":"` + marker + `\nold","user":{"login":"maya-stall-bot"}}]`)},
				{StatusCode: 200, Body: []byte(`{"id":202}`)},
			},
		}

		result, err := postGitHubReviewComment(options, markdown, api)
		if err != nil {
			t.Fatalf("postGitHubReviewComment returned error: %v", err)
		}
		if result.Operation != "update" || result.CommentID != "202" {
			t.Fatalf("result = %+v, want update comment 202", result)
		}
		if len(api.requests) != 3 {
			t.Fatalf("request count = %d, want 3", len(api.requests))
		}
		assertReviewAPIRequest(t, api.requests[2], "PATCH", "https://api.github.com/repos/BramVR/maya_stall/issues/comments/202", postedMarkdown)
	})

	t.Run("update from paginated lookup", func(t *testing.T) {
		api := &fakeReviewCommentAPI{
			responses: []reviewCommentAPIResponse{
				{StatusCode: 200, Body: []byte(`{"login":"maya-stall-bot"}`)},
				{
					StatusCode: 200,
					Headers:    map[string]string{"Link": `<https://api.github.com/repos/BramVR/maya_stall/issues/12/comments?per_page=100&page=2>; rel="next"`},
					Body:       []byte(`[{"id":201,"body":"not ours","user":{"login":"someone-else"}}]`),
				},
				{StatusCode: 200, Body: []byte(`[{"id":203,"body":"` + marker + `\nold","user":{"login":"maya-stall-bot"}}]`)},
				{StatusCode: 200, Body: []byte(`{"id":203}`)},
			},
		}

		result, err := postGitHubReviewComment(options, markdown, api)
		if err != nil {
			t.Fatalf("postGitHubReviewComment returned error: %v", err)
		}
		if result.Operation != "update" || result.CommentID != "203" {
			t.Fatalf("result = %+v, want update comment 203", result)
		}
		if len(api.requests) != 4 {
			t.Fatalf("request count = %d, want 4", len(api.requests))
		}
		assertReviewAPIRequest(t, api.requests[2], "GET", "https://api.github.com/repos/BramVR/maya_stall/issues/12/comments?per_page=100&page=2", "")
		assertReviewAPIRequest(t, api.requests[3], "PATCH", "https://api.github.com/repos/BramVR/maya_stall/issues/comments/203", postedMarkdown)
	})
}

func TestGitLabReviewCommentShapesMergeRequestNote(t *testing.T) {
	markdown := "<!-- maya-stall:evidence-comment -->\n## Maya Stall Evidence\n"
	options := gitLabReviewCommentOptions{
		Project:      "BramVR/maya_stall",
		MergeRequest: 12,
		Token:        "fake-gitlab-token",
		BaseURL:      "https://gitlab.example.test",
	}
	marker := reviewCommentMarkerForTarget("gitlab", "merge-request:12")
	postedMarkdown := reviewMarkdownForPost(markdown, marker)
	api := &fakeReviewCommentAPI{
		responses: []reviewCommentAPIResponse{
			{StatusCode: 200, Body: []byte(`{"username":"maya-stall-bot"}`)},
			{StatusCode: 200, Body: []byte(`[]`)},
			{StatusCode: 201, Body: []byte(`{"id":303}`)},
		},
	}

	result, err := postGitLabReviewComment(options, markdown, api)
	if err != nil {
		t.Fatalf("postGitLabReviewComment returned error: %v", err)
	}
	if result.Operation != "create" || result.CommentID != "303" {
		t.Fatalf("result = %+v, want create note 303", result)
	}
	if len(api.requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(api.requests))
	}
	assertReviewAPIRequest(t, api.requests[0], "GET", "https://gitlab.example.test/api/v4/user", "")
	assertReviewAPIRequest(t, api.requests[1], "GET", "https://gitlab.example.test/api/v4/projects/BramVR%2Fmaya_stall/merge_requests/12/notes?per_page=100", "")
	assertReviewAPIRequest(t, api.requests[2], "POST", "https://gitlab.example.test/api/v4/projects/BramVR%2Fmaya_stall/merge_requests/12/notes", postedMarkdown)
	if got := api.requests[2].Headers["Private-Token"]; got != "fake-gitlab-token" {
		t.Fatalf("GitLab token header = %q", got)
	}

	api = &fakeReviewCommentAPI{
		responses: []reviewCommentAPIResponse{
			{StatusCode: 200, Body: []byte(`{"username":"maya-stall-bot"}`)},
			{StatusCode: 200, Headers: map[string]string{"X-Next-Page": "2"}, Body: []byte(`[{"id":301,"body":"not ours","author":{"username":"someone-else"}}]`)},
			{StatusCode: 200, Body: []byte(`[{"id":304,"body":"` + marker + `\nold","author":{"username":"maya-stall-bot"}}]`)},
			{StatusCode: 200, Body: []byte(`{"id":304}`)},
		},
	}
	result, err = postGitLabReviewComment(options, markdown, api)
	if err != nil {
		t.Fatalf("postGitLabReviewComment paginated update returned error: %v", err)
	}
	if result.Operation != "update" || result.CommentID != "304" {
		t.Fatalf("result = %+v, want update note 304", result)
	}
	if len(api.requests) != 4 {
		t.Fatalf("request count = %d, want 4", len(api.requests))
	}
	assertReviewAPIRequest(t, api.requests[2], "GET", "https://gitlab.example.test/api/v4/projects/BramVR%2Fmaya_stall/merge_requests/12/notes?page=2&per_page=100", "")
	assertReviewAPIRequest(t, api.requests[3], "PUT", "https://gitlab.example.test/api/v4/projects/BramVR%2Fmaya_stall/merge_requests/12/notes/304", postedMarkdown)
}

func TestReviewCommentCommandDryRunUsesPublishedManifest(t *testing.T) {
	dir := t.TempDir()
	published := writePublishedManifestFixture(t, dir)
	var stdout, stderr strings.Builder

	code := Run([]string{"review-comment", "github", "--repo", "BramVR/maya_stall", "--pr", "12", "--dry-run", published}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("review-comment dry-run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"platform: github",
		"operation: dry-run",
		"target: BramVR/maya_stall#12",
		"reviewComment:",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, stdout.String())
		}
	}
	content, err := os.ReadFile(filepath.Join(published, "review-comment.md"))
	if err != nil {
		t.Fatalf("read rendered review comment: %v", err)
	}
	if !strings.Contains(string(content), "status: passed") || !strings.Contains(string(content), reviewCommentMarker) {
		t.Fatalf("rendered review comment missing manifest content:\n%s", string(content))
	}
}

func TestReviewCommentCommandMissingCredentialsAreClear(t *testing.T) {
	dir := t.TempDir()
	published := writePublishedManifestFixture(t, dir)
	var stdout, stderr strings.Builder

	code := Run([]string{"review-comment", "github", "--repo", "BramVR/maya_stall", "--pr", "12", "--token-env", "MAYA_STALL_TEST_MISSING_TOKEN", published}, &stdout, &stderr, dir, "test-version")
	if code != 2 {
		t.Fatalf("review-comment exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "MAYA_STALL_TEST_MISSING_TOKEN") || !strings.Contains(stderr.String(), "GitHub Review Comment") {
		t.Fatalf("missing credential error not clear: %s", stderr.String())
	}
}

func TestReviewCommentCommandMissingPlatformContextIsClear(t *testing.T) {
	dir := t.TempDir()
	published := writePublishedManifestFixture(t, dir)
	var stdout, stderr strings.Builder

	code := Run([]string{"review-comment", "gitlab", "--merge-request", "12", "--dry-run", published}, &stdout, &stderr, dir, "test-version")
	if code != 2 {
		t.Fatalf("review-comment exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "GitLab Review Comment needs --project") {
		t.Fatalf("missing context error not clear: %s", stderr.String())
	}
}

func TestReviewCommentCommandRejectsWrongPlatformURLFlags(t *testing.T) {
	dir := t.TempDir()
	published := writePublishedManifestFixture(t, dir)
	var stdout, stderr strings.Builder

	code := Run([]string{"review-comment", "github", "--repo", "BramVR/maya_stall", "--pr", "12", "--base-url", "https://gitlab.example.test", "--dry-run", published}, &stdout, &stderr, dir, "test-version")
	if code != 2 {
		t.Fatalf("github wrong URL flag exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--base-url only applies to GitLab") {
		t.Fatalf("github wrong URL flag error not clear: %s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"review-comment", "gitlab", "--project", "BramVR/maya_stall", "--merge-request", "12", "--api-url", "https://github.example.test", "--dry-run", published}, &stdout, &stderr, dir, "test-version")
	if code != 2 {
		t.Fatalf("gitlab wrong URL flag exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--api-url only applies to GitHub") {
		t.Fatalf("gitlab wrong URL flag error not clear: %s", stderr.String())
	}
}

type fakeReviewCommentAPI struct {
	responses []reviewCommentAPIResponse
	requests  []reviewCommentAPIRequest
}

func (api *fakeReviewCommentAPI) Do(request reviewCommentAPIRequest) (reviewCommentAPIResponse, error) {
	api.requests = append(api.requests, request)
	if len(api.responses) == 0 {
		return reviewCommentAPIResponse{}, nil
	}
	response := api.responses[0]
	api.responses = api.responses[1:]
	return response, nil
}

func assertReviewAPIRequest(t *testing.T, request reviewCommentAPIRequest, method string, url string, markdown string) {
	t.Helper()
	if request.Method != method || request.URL != url {
		t.Fatalf("request = %s %s, want %s %s", request.Method, request.URL, method, url)
	}
	if markdown == "" {
		if len(request.Body) != 0 {
			t.Fatalf("request body = %s, want empty", string(request.Body))
		}
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatalf("parse request body: %v; body: %s", err, string(request.Body))
	}
	if body.Body != markdown {
		t.Fatalf("request markdown = %q, want %q", body.Body, markdown)
	}
}

func writePublishedManifestFixture(t *testing.T, dir string) string {
	t.Helper()
	published := filepath.Join(dir, "published")
	if err := os.MkdirAll(published, 0o755); err != nil {
		t.Fatalf("create published fixture: %v", err)
	}
	manifest := publishedArtifactManifest{
		RunID:         "20260704T120000.000000000Z",
		Scenario:      "smoke",
		Status:        "passed",
		TargetProfile: "ci",
		Host:          "fake-local",
		BaseURL:       "https://evidence.example.test/maya",
		Artifacts: []publishedArtifact{
			{Label: "logs", Kind: "log", Path: "logs/session.log", URL: "https://evidence.example.test/maya/20260704T120000.000000000Z/logs/session.log", MediaType: "text/plain"},
		},
	}
	if err := writeJSONFile(filepath.Join(published, "artifact-manifest.json"), manifest); err != nil {
		t.Fatalf("write artifact manifest: %v", err)
	}
	return published
}
