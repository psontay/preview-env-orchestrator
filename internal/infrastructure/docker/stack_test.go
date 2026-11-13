package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"preview-env-orchestrator/internal/application"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testSource struct{ repo, sha string }

func (s *testSource) Archive(_ context.Context, repo, sha string) (io.ReadCloser, error) {
	s.repo, s.sha = repo, sha
	return io.NopCloser(strings.NewReader("sanitized-build-context")), nil
}

type fakeEngine struct {
	t              *testing.T
	group          string
	created        map[string]container.Config
	removed        map[string]bool
	buildFailed    bool
	appStopped     bool
	managedNetwork bool
	networkRemoved bool
	imageRemoved   bool
	removeFailure  string
}

func (e *fakeEngine) roundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	path := strings.TrimPrefix(r.URL.Path, "/v1.51")
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	switch {
	case path == "/build":
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte("sanitized-build-context")) {
			e.t.Error("wrong build context")
		}
		if r.URL.Query().Get("buildargs") != "" {
			e.t.Error("credentials must not be build args")
		}
		if e.buildFailed {
			write(map[string]any{"error": "build failed"})
		} else {
			write(map[string]any{"stream": "Successfully built image\n"})
		}
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		write(map[string]any{"Id": "image-id", "Config": map[string]any{"Labels": labelsFor(e.group)}})
	case path == "/networks/create":
		write(map[string]string{"Id": e.group})
	case strings.HasSuffix(path, "/connect") || strings.HasSuffix(path, "/disconnect"):
		w.WriteHeader(200)
	case path == "/containers/create":
		var config container.Config
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			e.t.Fatal(err)
		}
		name := r.URL.Query().Get("name")
		e.created[name] = config
		write(map[string]string{"Id": name})
	case strings.HasSuffix(path, "/start"):
		w.WriteHeader(http.StatusNoContent)
	case path == "/containers/json":
		if !strings.Contains(r.URL.Query().Get("filters"), ownerLabel) || !strings.Contains(r.URL.Query().Get("filters"), e.group) {
			e.t.Error("cleanup must be scoped by owner and group")
		}
		var result []map[string]string
		for id := range e.created {
			if !e.removed[id] {
				result = append(result, map[string]string{"Id": id})
			}
		}
		write(result)
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		write(map[string]any{"Id": id, "Name": id, "Config": e.created[id], "State": map[string]any{"Running": !(e.appStopped && strings.HasSuffix(id, "-app")), "Health": map[string]string{"Status": "healthy"}}, "NetworkSettings": map[string]any{"Ports": map[string]any{"8080/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "34567"}}}}})
	case strings.HasPrefix(path, "/containers/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/containers/")
		if id == e.removeFailure {
			w.WriteHeader(500)
			write(map[string]string{"message": "temporary remove failure"})
			break
		}
		e.removed[id] = true
		w.WriteHeader(http.StatusNoContent)
	case path == "/networks/"+e.group && r.Method == http.MethodGet:
		labels := map[string]string{}
		if e.managedNetwork {
			labels = labelsFor(e.group)
		}
		write(map[string]any{"Id": e.group, "Labels": labels})
	case path == "/networks/"+e.group && r.Method == http.MethodDelete:
		e.networkRemoved = true
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/images/") && r.Method == http.MethodDelete:
		e.imageRemoved = true
		write([]any{})
	default:
		e.t.Errorf("unexpected Docker request %s %s", r.Method, path)
		w.WriteHeader(500)
	}
	return w.Result(), nil
}

func fixtureService(t *testing.T) (*RealService, *fakeEngine, *testSource, application.ProvisionRequest) {
	t.Helper()
	req := application.ProvisionRequest{EnvID: "org/app:PR_1:" + strings.Repeat("a", 40), Repo: "org/app", CommitSHA: strings.Repeat("a", 40)}
	engine := &fakeEngine{t: t, group: ResourceName(req.EnvID), created: map[string]container.Config{}, removed: map[string]bool{}, managedNetwork: true}
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.invalid"), client.WithVersion("1.51"), client.WithHTTPClient(&http.Client{Transport: testTransport(engine.roundTrip)}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	source := &testSource{}
	svc := &RealService{cli: cli, buildGate: make(chan struct{}, 1), httpClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"UP"}`)), Header: make(http.Header)}, nil
	})}}
	if err := svc.ConfigurePreview(source, StackConfig{Repository: "org/app", Secret: strings.Repeat("s", 64), ReadyTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	return svc, engine, source, req
}

func TestStackBuildsCommitAndIsolatesServices(t *testing.T) {
	svc, engine, source, req := fixtureService(t)
	result, err := svc.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if source.sha != req.CommitSHA || source.repo != req.Repo {
		t.Fatal("wrong source revision")
	}
	if len(engine.created) != 5 || result.URL != "http://localhost:34567" {
		t.Fatalf("bad preview: containers=%d URL=%s", len(engine.created), result.URL)
	}
	app := engine.created[engine.group+"-app"]
	joined := strings.Join(app.Env, "\n")
	for _, required := range []string{"jdbc:postgresql://db:5432/todolist", "REDIS_HOST=redis", "RABBITMQ_HOST=rabbitmq", "SMTP_HOST=mail", "SPRING_INIT_ADMIN_USERNAME=previewadmin"} {
		if !strings.Contains(joined, required) {
			t.Errorf("missing app config %s", required)
		}
	}
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "smtp.gmail.com") {
		t.Fatal("preview must not inherit GitHub or real SMTP credentials")
	}
	if strings.ContainsAny(engine.group, "/:") {
		t.Fatal("resource name is not Docker safe")
	}
	if err := svc.Destroy(context.Background(), result.ContainerID); err != nil {
		t.Fatal(err)
	}
	if len(engine.removed) != 5 || !engine.networkRemoved || !engine.imageRemoved {
		t.Fatal("cleanup must remove entire owned stack")
	}
}

func TestStackRollsBackWhenBackendStops(t *testing.T) {
	svc, engine, _, req := fixtureService(t)
	engine.appStopped = true
	if _, err := svc.Provision(context.Background(), req); err == nil {
		t.Fatal("stopped backend must not be reported ready")
	}
	if len(engine.removed) != 5 || !engine.networkRemoved || !engine.imageRemoved {
		t.Fatal("failed preview leaked owned resources")
	}
}

func TestBuildErrorsAndUnmanagedNetworkAreNotIgnored(t *testing.T) {
	if err := consumeDockerStream(strings.NewReader(`{"errorDetail":{"message":"pull failed"}}`)); err == nil {
		t.Fatal("Docker JSON error ignored")
	}
	if err := consumeDockerStream(strings.NewReader(`{"stream":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	svc, engine, _, _ := fixtureService(t)
	engine.managedNetwork = false
	if err := svc.destroyStack(context.Background(), engine.group); err == nil {
		t.Fatal("unmanaged network must be protected")
	}
	if engine.networkRemoved {
		t.Fatal("removed unmanaged network")
	}
}

func TestStackRespectsCancellationWhileBuildQueued(t *testing.T) {
	svc, _, _, req := fixtureService(t)
	svc.buildGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := svc.buildCommit(ctx, req, "tag", "group")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCleanupRetriesAfterAppContainerWasAlreadyRemoved(t *testing.T) {
	svc, engine, _, req := fixtureService(t)
	result, err := svc.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResourceGroup == "" {
		t.Fatal("stack must persist a stable cleanup handle")
	}
	engine.removeFailure = engine.group + "-db"
	if err := svc.Destroy(context.Background(), result.ResourceGroup); err == nil {
		t.Fatal("partial cleanup must report failure")
	}
	if !engine.removed[engine.group+"-app"] {
		t.Fatal("test must remove app before retry")
	}
	engine.removeFailure = ""
	if err := svc.Destroy(context.Background(), result.ResourceGroup); err != nil {
		t.Fatal(err)
	}
	if len(engine.removed) != 5 || !engine.networkRemoved || !engine.imageRemoved {
		t.Fatal("retry leaked dependency resources")
	}
}

func TestStackBuildFailureStartsNoContainersAndRollsBack(t *testing.T) {
	svc, engine, source, req := fixtureService(t)
	engine.buildFailed = true
	result, err := svc.Provision(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatal("Docker build failure must be reported")
	}
	if source.sha != req.CommitSHA || len(engine.created) != 0 || !engine.imageRemoved || !engine.networkRemoved {
		t.Fatal("failed build must not start dependencies and must clean owned leftovers")
	}
	if result.ContainerID != "" || result.ResourceGroup != "" {
		t.Fatal("successful rollback must not report unresolved resources")
	}
}

func TestStackRollbackFailureReturnsHandleForLaterCleanup(t *testing.T) {
	svc, engine, _, req := fixtureService(t)
	engine.appStopped = true
	engine.removeFailure = engine.group + "-db"
	result, err := svc.Provision(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatal("rollback failure must not be hidden by the original readiness failure")
	}
	if result.ResourceGroup != engine.group || result.ContainerID != engine.group || engine.removed[engine.removeFailure] {
		t.Fatal("unremoved resources must retain a stable cleanup handle")
	}
	engine.removeFailure = ""
	if err := svc.Destroy(t.Context(), result.ResourceGroup); err != nil {
		t.Fatal(err)
	}
	if len(engine.removed) != 5 || !engine.networkRemoved || !engine.imageRemoved {
		t.Fatal("later cleanup must remove every owned resource after rollback failed")
	}
}
