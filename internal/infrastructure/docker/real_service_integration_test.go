//go:build integration

// Integration test cho RealService — cần Docker daemon thật (biến môi trường
// DOCKER_HOST / /var/run/docker.sock). Tách bằng build tag `integration` để
// `go test ./...` thông thường KHÔNG chạy (đúng khuyến nghị trong
// docs/DOCKER_SDK_INTEGRATION.md: unit test dùng MockService, không spin
// container thật).
//
// Chạy:  go test -tags=integration ./internal/infrastructure/docker/...
package docker_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"preview-env-orchestrator/internal/application"
	dockerinfra "preview-env-orchestrator/internal/infrastructure/docker"
)

func TestRealService_ProvisionThenDestroy(t *testing.T) {
	ctx := context.Background()

	svc, err := dockerinfra.NewRealService()
	if err != nil {
		t.Fatalf("NewRealService: %v", err)
	}

	// nginx:alpine expose sẵn cổng 80 -> PublishAllPorts sẽ map ra một port
	// host động, đúng thứ RealService cần để dựng URL preview.
	res, err := svc.Provision(ctx, application.ProvisionRequest{
		Image: "nginx:alpine",
		EnvID: "it-" + time.Now().Format("150405"),
		// Bỏ seed: nginx không có psql; luồng seed đã được review ở code.
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.ContainerID == "" {
		t.Fatal("Provision không trả ContainerID")
	}
	if !strings.HasPrefix(res.URL, "http://localhost:") {
		t.Fatalf("URL preview sai định dạng: %q", res.URL)
	}

	// Dọn dẹp dù test có fail giữa chừng.
	t.Cleanup(func() { _ = svc.Destroy(ctx, res.ContainerID) })

	// Port host vừa cấp phải thực sự phục vụ được HTTP.
	if !waitHTTP(res.URL, 15*time.Second) {
		t.Fatalf("không truy cập được %s sau khi Provision", res.URL)
	}

	// Destroy phải xoá hẳn container.
	if err := svc.Destroy(ctx, res.ContainerID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	if _, err := cli.ContainerInspect(ctx, res.ContainerID); err == nil {
		t.Fatalf("container %s vẫn còn sau Destroy", res.ContainerID)
	}
}

func TestRealService_ImplementsPort(t *testing.T) {
	var _ application.DockerOrchestrationService = (*dockerinfra.RealService)(nil)
	// Còn kiểm tra ContainerExecInspect/CopyToContainer đúng chữ ký ở compile-time
	// thông qua chính package docker (build tag thường).
	_ = container.ExecOptions{}
}

func waitHTTP(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // test đơn giản
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
