// Publish immutable binaries and a GitLab release using only CI_JOB_TOKEN.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-campbox\.[0-9]+$`)

type publisher struct {
	client              *http.Client
	api, project, token string
}

func (p publisher) request(method, endpoint string, body []byte) (int, []byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("JOB-TOKEN", p.token)
		if strings.Contains(endpoint, "/releases") {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := p.client.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("GitLab request failed: %s", method)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 128*1024*1024))
		_ = response.Body.Close()
		if readErr != nil {
			return 0, nil, readErr
		}
		if (response.StatusCode == 429 || response.StatusCode >= 500) && attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		return response.StatusCode, data, nil
	}
	return 0, nil, fmt.Errorf("GitLab request exhausted retries")
}

func (p publisher) publishFile(version, name string, data []byte) (string, error) {
	endpoint := p.api + "/projects/" + url.PathEscape(p.project) + "/packages/generic/content-cache/" + url.PathEscape(version) + "/" + name
	status, existing, err := p.request(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		if sha256.Sum256(existing) != sha256.Sum256(data) {
			return "", fmt.Errorf("package %s already exists with different contents; create a new release tag", name)
		}
		return endpoint, nil
	}
	if status != http.StatusNotFound {
		return "", fmt.Errorf("package lookup failed: HTTP %d", status)
	}
	status, _, err = p.request(http.MethodPut, endpoint, data)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("package upload failed: HTTP %d", status)
	}
	return endpoint, nil
}

func (p publisher) publish(tag, directory string) error {
	if !releaseTag.MatchString(tag) {
		return fmt.Errorf("expected a tag such as v1.4.0-campbox.1")
	}
	version := strings.TrimPrefix(tag, "v")
	checksums, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return err
	}
	wanted := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(checksums)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return fmt.Errorf("invalid checksum manifest")
		}
		wanted[fields[1]] = fields[0]
	}
	names := []string{"content-cache_linux_amd64", "content-cache_linux_arm64", "LICENSE", "BUILD-INFO.txt", "SHA256SUMS"}
	// Validate every local file before making any publication request.
	files := map[string][]byte{}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if name != "SHA256SUMS" {
			sum := sha256.Sum256(data)
			if wanted[name] != hex.EncodeToString(sum[:]) {
				return fmt.Errorf("checksum mismatch for %s", name)
			}
		}
		files[name] = data
	}
	links := make([]map[string]string, 0, len(names))
	for _, name := range names {
		endpoint, err := p.publishFile(version, name, files[name])
		if err != nil {
			return err
		}
		links = append(links, map[string]string{"name": name, "url": endpoint, "link_type": "package"})
	}
	endpoint := p.api + "/projects/" + url.PathEscape(p.project) + "/releases"
	status, _, err := p.request(http.MethodGet, endpoint+"/"+url.PathEscape(tag), nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("release lookup failed: HTTP %d", status)
	}
	body, err := json.Marshal(map[string]any{
		"tag_name": tag, "name": "content-cache " + version,
		"description": "Campbox build based on upstream v1.4.0. Configurable metadata limits (default 64MiB) and coalesced concurrent npm metadata fetches. Adaptive bbolt batching and S3-FIFO eviction with shorter lock holds. Linux amd64 and arm64 binaries, SHA256SUMS, LICENSE and BUILD-INFO.txt are attached.",
		"assets":      map[string]any{"links": links},
	})
	if err != nil {
		return err
	}
	status, _, err = p.request(http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("release creation failed: HTTP %d", status)
	}
	return nil
}

func main() {
	p := publisher{client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
			return fmt.Errorf("cross-origin redirect rejected")
		}
		return nil
	}}, api: strings.TrimRight(os.Getenv("CI_API_V4_URL"), "/"), project: os.Getenv("CI_PROJECT_ID"), token: os.Getenv("CI_JOB_TOKEN")}
	if p.api == "" || p.project == "" || p.token == "" {
		fmt.Fprintln(os.Stderr, "CI_API_V4_URL, CI_PROJECT_ID and CI_JOB_TOKEN are required")
		os.Exit(1)
	}
	if err := p.publish(os.Getenv("CI_COMMIT_TAG"), "dist"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Published content-cache packages and GitLab release")
}
