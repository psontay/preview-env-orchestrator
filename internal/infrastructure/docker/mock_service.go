// Package docker chứa cài đặt của application.DockerOrchestrationService.
// File này (mock_service.go) là bản giả lập, không cần Docker daemon thật,
// dùng để chạy demo / unit test. Bản thật dùng Docker SDK chính thức được
// mô tả chi tiết ở docs/DOCKER_SDK_INTEGRATION.md (không compile kèm theo
// vì cần thêm dependency github.com/docker/docker).
package docker

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"preview-env-orchestrator/internal/application"
)

// MockService triển khai application.DockerOrchestrationService bằng cách
// giả lập độ trễ tạo container, tuân theo cùng interface mà bản thật
// (Docker SDK) sẽ dùng — nhờ vậy WebhookService không cần biết/thay đổi
// gì khi ta chuyển từ mock sang thật.
type MockService struct{}

func NewMockService() *MockService {
	return &MockService{}
}

func (m *MockService) Provision(ctx context.Context, req application.ProvisionRequest) (application.ProvisionResult, error) {
	select {
	case <-time.After(300 * time.Millisecond): // giả lập thời gian pull image + start container
	case <-ctx.Done():
		return application.ProvisionResult{}, ctx.Err()
	}

	containerID := fmt.Sprintf("mock-%s-%d", req.EnvID, rand.Intn(9999))
	port := 30000 + rand.Intn(9999)
	url := fmt.Sprintf("http://preview-%s.yourdomain.com:%d", req.EnvID, port)

	return application.ProvisionResult{
		ContainerID: containerID,
		URL:         url,
	}, nil
}

func (m *MockService) Destroy(_ context.Context, containerID string) error {
	fmt.Printf("[mock-docker] destroyed container %s\n", containerID)
	return nil
}
