package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func archiveFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSourceBuildsImmutableSanitizedContext(t *testing.T) {
	sha := strings.Repeat("a", 40)
	compressed := archiveFixture(t, map[string]string{"root/Dockerfile": "FROM scratch", "root/src/app": "app", "root/.env": "private-value", "root/.git/config": "private-git", "root/drop.txt": "excluded", "root/.dockerignore": "drop.txt\n"})
	client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/app/tarball/"+sha || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("wrong immutable source request")
		}
		_, _ = w.Write(compressed)
	})
	reader, err := NewSource("test-token", "https://github.example", client).Archive(context.Background(), "org/app", sha)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tr := tar.NewReader(reader)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	if !names["Dockerfile"] || !names["src/app"] || names[".env"] || names[".git/config"] || names["drop.txt"] {
		t.Fatalf("unexpected build files: %v", names)
	}
}

func TestSourceRejectsUnsafeArchivesAndMissingDockerfile(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"traversal":          {"root/../Dockerfile": "FROM scratch"},
		"absolute":           {"/root/Dockerfile": "FROM scratch"},
		"missing Dockerfile": {"root/app": "code"},
		"multiple roots":     {"one/Dockerfile": "FROM scratch", "two/app": "code"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sanitizeArchive(archiveFixture(t, files)); err == nil {
				t.Fatal("unsafe/incomplete archive accepted")
			}
		})
	}
}

func TestSourceRejectsBranchInsteadOfCommit(t *testing.T) {
	s := NewSource("token", "https://github.example", handlerClient(func(http.ResponseWriter, *http.Request) { t.Error("invalid ref must not call GitHub") }))
	if _, err := s.Archive(context.Background(), "org/app", "master"); err == nil {
		t.Fatal("must require immutable full SHA")
	}
}
