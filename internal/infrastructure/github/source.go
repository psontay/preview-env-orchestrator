package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

const maxSourceBytes = 64 << 20

type Source struct {
	token   string
	baseURL string
	client  *http.Client
}

func NewSource(token, baseURL string, client *http.Client) *Source {
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many source redirects")
			}
			if req.URL.Scheme != "https" || req.URL.Hostname() != "codeload.github.com" {
				return fmt.Errorf("unexpected source archive redirect")
			}
			req.Header.Del("Authorization")
			return nil
		}}
	}
	return &Source{token: token, baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (s *Source) Archive(ctx context.Context, repo, sha string) (io.ReadCloser, error) {
	if !repositoryPattern.MatchString(repo) || !commitPattern.MatchString(sha) {
		return nil, fmt.Errorf("source requires owner/repo and a full commit SHA")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/repos/"+repo+"/tarball/"+sha, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download commit archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download source: GitHub HTTP %d", resp.StatusCode)
	}
	compressed, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(compressed) > maxSourceBytes {
		return nil, fmt.Errorf("source archive too large")
	}
	contextTar, err := sanitizeArchive(compressed)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(contextTar)), nil
}

type sourceFile struct {
	header tar.Header
	data   []byte
}

func sanitizeArchive(compressed []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("read source gzip: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, maxSourceBytes+1))
	var files []sourceFile
	var ignore []byte
	var total int64
	root := ""
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read source tar: %w", err)
		}
		if strings.HasPrefix(header.Name, "/") || strings.Contains(header.Name, "\\") {
			return nil, fmt.Errorf("unsafe archive path")
		}
		for _, part := range strings.Split(header.Name, "/") {
			if part == ".." {
				return nil, fmt.Errorf("unsafe archive traversal")
			}
		}
		prefix, name, found := strings.Cut(header.Name, "/")
		if !found || name == "" {
			continue
		}
		if root == "" {
			root = prefix
		}
		if root != prefix {
			return nil, fmt.Errorf("multiple archive roots")
		}
		name = path.Clean(name)
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unsupported archive entry: %s", name)
		}
		if header.Size < 0 || header.Size > maxSourceBytes || len(files) >= 20000 {
			return nil, fmt.Errorf("source archive limit exceeded")
		}
		total += header.Size
		if total > maxSourceBytes {
			return nil, fmt.Errorf("expanded source too large")
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		copyHeader := *header
		copyHeader.Name, copyHeader.Uid, copyHeader.Gid = name, 0, 0
		copyHeader.Uname, copyHeader.Gname = "", ""
		copyHeader.PAXRecords = nil
		files = append(files, sourceFile{copyHeader, data})
		if name == ".dockerignore" {
			ignore = data
		}
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(ignore))
	if err != nil {
		return nil, err
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	foundDockerfile := false
	for _, file := range files {
		name := file.header.Name
		// Never upload local credentials or Git metadata, even when a PR changes
		// .dockerignore. Example env files remain available to the Dockerfile.
		sensitive := false
		for _, part := range strings.Split(name, "/") {
			if part == ".git" || part == ".env" || part == ".evn" || (strings.HasPrefix(part, ".env.") && part != ".env.example") {
				sensitive = true
			}
		}
		if sensitive {
			continue
		}
		excluded, err := matcher.MatchesOrParentMatches(name)
		if err != nil {
			return nil, err
		}
		if excluded && name != "Dockerfile" && name != ".dockerignore" {
			continue
		}
		if name == "Dockerfile" {
			foundDockerfile = file.header.Typeflag == tar.TypeReg
		}
		if err := writer.WriteHeader(&file.header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.data); err != nil {
			return nil, err
		}
	}
	if !foundDockerfile {
		return nil, fmt.Errorf("commit has no root Dockerfile")
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if output.Len() > maxSourceBytes {
		return nil, fmt.Errorf("build context too large")
	}
	return output.Bytes(), nil
}
