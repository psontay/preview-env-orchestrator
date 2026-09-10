package application

import "context"

// DockerOrchestrationService là port mà application layer phụ thuộc vào.
// Việc implement thật (bằng Docker SDK) nằm ở internal/infrastructure/docker,
// application layer không biết và không cần biết chi tiết đó — chỉ biết
// "provision" và "destroy" là hai hành vi cần có.
type DockerOrchestrationService interface {
	Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
	Destroy(ctx context.Context, containerID string) error
}

type ProvisionRequest struct {
	Image       string
	SeedSQLPath string
	EnvID       string
}

type ProvisionResult struct {
	ContainerID string
	URL         string
}

// GitHubNotifier là port để post comment ngược lại vào Pull Request.
type GitHubNotifier interface {
	CommentOnPR(ctx context.Context, repo string, prNumber int, message string) error
}
