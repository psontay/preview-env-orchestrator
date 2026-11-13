package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"

	"preview-env-orchestrator/internal/application"
)

// RealService là cài đặt THẬT của application.DockerOrchestrationService,
// dùng Docker Engine SDK chính thức (github.com/docker/docker/client) — KHÔNG
// gọi CLI `docker` qua os/exec, để không lệ thuộc binary cài sẵn trên host và
// dễ kiểm thử/mở rộng hơn.
//
// MockService (mock_service.go) được giữ nguyên cho unit test; main.go chọn
// giữa hai adapter qua biến môi trường DOCKER_DRIVER (mock | real).
type RealService struct {
	cli        *client.Client
	source     application.SourceArchiveProvider
	stack      StackConfig
	buildGate  chan struct{}
	httpClient *http.Client
}

var _ application.DockerOrchestrationService = (*RealService)(nil)

// NewRealService tạo client từ môi trường (DOCKER_HOST, DOCKER_CERT_PATH, ...)
// và tự thương lượng phiên bản API với daemon.
func NewRealService() (*RealService, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: tạo client: %w", err)
	}
	return &RealService{cli: cli, buildGate: make(chan struct{}, 1)}, nil
}

func (s *RealService) Close() error { return s.cli.Close() }

// Provision: (1) tạo + chạy container từ image cấu hình sẵn với PublishAllPorts
// (Docker tự cấp port host động), (2) copy file seed .sql vào container và chạy
// nó qua exec, (3) inspect để lấy port host vừa cấp phát rồi dựng URL preview.
func (s *RealService) Provision(ctx context.Context, req application.ProvisionRequest) (application.ProvisionResult, error) {
	if req.Repo != "" {
		return s.provisionStack(ctx, req)
	}
	if req.Image == "" {
		return application.ProvisionResult{}, errors.New("docker: missing image or source repository")
	}
	// SDK không tự pull image như CLI `docker run` — chủ động đảm bảo có image.
	if err := s.ensureImage(ctx, req.Image); err != nil {
		return application.ProvisionResult{}, fmt.Errorf("docker: chuẩn bị image %q: %w", req.Image, err)
	}

	created, err := s.cli.ContainerCreate(ctx,
		&container.Config{
			Image:  req.Image,
			Labels: map[string]string{"preview-env-id": req.EnvID, ownerLabel: ownerValue},
		},
		&container.HostConfig{PublishAllPorts: true},
		nil, nil,
		ResourceName(req.EnvID),
	)
	if err != nil {
		return application.ProvisionResult{}, fmt.Errorf("docker: create container: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.cli.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	}()

	if err := s.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return application.ProvisionResult{}, fmt.Errorf("docker: start container: %w", err)
	}

	if req.SeedSQLPath != "" {
		if err := s.seedDatabase(ctx, created.ID, req.SeedSQLPath); err != nil {
			return application.ProvisionResult{}, fmt.Errorf("docker: seed db: %w", err)
		}
	}

	inspect, err := s.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return application.ProvisionResult{}, fmt.Errorf("docker: inspect container: %w", err)
	}

	url := buildURL(inspect)
	if url == "" {
		return application.ProvisionResult{}, errors.New("docker: container không expose port nào để dựng URL preview")
	}

	succeeded = true
	return application.ProvisionResult{
		ContainerID: created.ID,
		URL:         url,
	}, nil
}

// Destroy dừng (grace 10s) rồi xoá hẳn container. Bỏ qua lỗi "đã dừng" ở bước
// stop, chỉ trả lỗi nếu remove thất bại.
func (s *RealService) Destroy(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}
	if strings.ContainsAny(containerID, "/:") {
		return s.destroyStack(ctx, ResourceName(containerID))
	}
	if strings.HasPrefix(containerID, "peo-") && len(containerID) == 28 {
		return s.destroyStack(ctx, containerID)
	}
	inspect, err := s.cli.ContainerInspect(ctx, containerID)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if inspect.Config.Labels[ownerLabel] != ownerValue {
		return errors.New("refusing to delete an unmanaged container")
	}
	if group := inspect.Config.Labels[groupLabel]; group != "" {
		return s.destroyStack(ctx, group)
	}
	timeout := 10
	_ = s.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	err = s.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// ensureImage kéo image từ registry nếu chưa có sẵn cục bộ. Phải đọc hết
// stream trả về thì thao tác pull mới thực sự hoàn tất.
func (s *RealService) ensureImage(ctx context.Context, ref string) error {
	if _, err := s.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	rc, err := s.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	return consumeDockerStream(rc)
}

// seedDatabase copy file .sql vào /tmp trong container bằng CopyToContainer
// (Docker SDK, KHÔNG phải CLI), sau đó ContainerExecCreate + ExecStart để chạy
// lệnh seed và chờ tiến trình exec kết thúc, kiểm tra exit code.
func (s *RealService) seedDatabase(ctx context.Context, containerID, sqlPath string) error {
	data, err := os.ReadFile(sqlPath)
	if err != nil {
		return fmt.Errorf("đọc file seed %q: %w", sqlPath, err)
	}

	tarball, err := tarSingleFile("seed.sql", data)
	if err != nil {
		return err
	}
	if err := s.cli.CopyToContainer(ctx, containerID, "/tmp", tarball, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy seed vào container: %w", err)
	}

	exec, err := s.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"sh", "-c", "psql -U postgres -d appdb -f /tmp/seed.sql"},
	})
	if err != nil {
		return fmt.Errorf("tạo exec seed: %w", err)
	}
	if err := s.cli.ContainerExecStart(ctx, exec.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("chạy exec seed: %w", err)
	}

	code, err := s.waitExec(ctx, exec.ID)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("lệnh seed thoát với mã %d", code)
	}
	return nil
}

// waitExec chờ tiến trình exec kết thúc (bounded) rồi trả exit code.
func (s *RealService) waitExec(ctx context.Context, execID string) (int, error) {
	const (
		poll    = 150 * time.Millisecond
		maxWait = 60 * time.Second
	)
	deadline := time.Now().Add(maxWait)
	for {
		insp, err := s.cli.ContainerExecInspect(ctx, execID)
		if err != nil {
			return 0, fmt.Errorf("inspect exec seed: %w", err)
		}
		if !insp.Running {
			return insp.ExitCode, nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("lệnh seed chạy quá lâu (timeout)")
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// buildURL lấy port host đầu tiên (theo thứ tự cổng container tăng dần cho
// ổn định) trong ánh xạ port của container để dựng URL preview.
func buildURL(inspect container.InspectResponse) string {
	if inspect.NetworkSettings == nil {
		return ""
	}
	portMap := inspect.NetworkSettings.Ports
	keys := make([]string, 0, len(portMap))
	for p := range portMap {
		keys = append(keys, string(p))
	}
	sort.Strings(keys)

	for _, k := range keys {
		for _, binding := range portMap[nat.Port(k)] {
			if binding.HostPort != "" {
				return fmt.Sprintf("http://localhost:%s", binding.HostPort)
			}
		}
	}
	return ""
}

func tarSingleFile(name string, data []byte) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}
