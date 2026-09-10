# Tích hợp Docker SDK thật (thay cho MockService)

`internal/infrastructure/docker/mock_service.go` hiện đang giả lập việc tạo
container để bạn chạy demo mà không cần Docker daemon. Khi triển khai thật,
tạo file `internal/infrastructure/docker/real_service.go` implement cùng
interface `application.DockerOrchestrationService`, dùng **Docker Engine SDK
chính thức** (`github.com/docker/docker/client`) — **không** dùng
`os/exec("docker", "run", ...)`, để tránh lệ thuộc vào CLI cài sẵn trên host
và dễ mock/test hơn.

## Cài dependency

```bash
go get github.com/docker/docker/client
go get github.com/docker/docker/api/types/container
```

## Khung implementation

```go
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"preview-env-orchestrator/internal/application"
)

type RealService struct {
	cli *client.Client
}

func NewRealService() (*RealService, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &RealService{cli: cli}, nil
}

func (s *RealService) Provision(ctx context.Context, req application.ProvisionRequest) (application.ProvisionResult, error) {
	created, err := s.cli.ContainerCreate(ctx,
		&container.Config{Image: req.Image, Labels: map[string]string{"preview-env-id": req.EnvID}},
		&container.HostConfig{PublishAllPorts: true},
		nil, nil, "preview-"+req.EnvID)
	if err != nil {
		return application.ProvisionResult{}, fmt.Errorf("create container: %w", err)
	}

	if err := s.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return application.ProvisionResult{}, fmt.Errorf("start container: %w", err)
	}

	if err := s.seedDatabase(ctx, created.ID, req.SeedSQLPath); err != nil {
		return application.ProvisionResult{}, fmt.Errorf("seed db: %w", err)
	}

	inspect, err := s.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return application.ProvisionResult{}, fmt.Errorf("inspect container: %w", err)
	}
	return application.ProvisionResult{
		ContainerID: created.ID,
		URL:         buildURL(inspect),
	}, nil
}

func (s *RealService) Destroy(ctx context.Context, containerID string) error {
	timeout := 10
	_ = s.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	return s.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// seedDatabase copy file .sql vào container qua CopyToContainer (Docker SDK,
// không phải CLI) rồi exec lệnh seed bên trong container.
func (s *RealService) seedDatabase(ctx context.Context, containerID, sqlPath string) error {
	data, err := os.ReadFile(sqlPath)
	if err != nil {
		return err
	}

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	_ = tw.WriteHeader(&tar.Header{Name: "seed.sql", Mode: 0644, Size: int64(len(data))})
	_, _ = tw.Write(data)
	_ = tw.Close()

	if err := s.cli.CopyToContainer(ctx, containerID, "/tmp", buf, container.CopyToContainerOptions{}); err != nil {
		return err
	}

	exec, err := s.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: []string{"sh", "-c", "psql -U postgres -d appdb -f /tmp/seed.sql"},
	})
	if err != nil {
		return err
	}
	return s.cli.ContainerExecStart(ctx, exec.ID, container.ExecStartOptions{})
}

func buildURL(inspect types.ContainerJSON) string {
	for _, bindings := range inspect.NetworkSettings.Ports {
		if len(bindings) > 0 {
			return fmt.Sprintf("http://localhost:%s", bindings[0].HostPort)
		}
	}
	return ""
}
```

## Lưu ý quan trọng

- **Interface không đổi.** `application.DockerOrchestrationService` chỉ có
  `Provision` và `Destroy` — đổi từ `MockService` sang `RealService` chỉ cần
  sửa 1 dòng ở `cmd/api/main.go` (Composition Root), không đụng vào domain
  hay application layer. Đây chính là lợi ích của Dependency Inversion.
- **Test bằng interface, không test bằng Docker thật.** Giữ `MockService`
  lại trong bộ test — đừng spin container thật trong CI/unit test.
- Field/method chính xác của Docker SDK (`ContainerExecOptions` vs
  `ExecOptions`, v.v.) thay đổi theo từng phiên bản `github.com/docker/docker`.
  Kiểm tra `go doc` sau khi `go get` để khớp đúng version bạn cài.
