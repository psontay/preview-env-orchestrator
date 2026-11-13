package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"

	"preview-env-orchestrator/internal/application"
)

const (
	ownerLabel = "io.peo.managed"
	ownerValue = "preview-env-orchestrator"
	groupLabel = "io.peo.group"
)

type StackConfig struct {
	Repository       string
	Secret           string
	ControlContainer string
	ReadyTimeout     time.Duration
}

func (s *RealService) ConfigurePreview(source application.SourceArchiveProvider, config StackConfig) error {
	if source == nil || config.Repository == "" || len(config.Secret) < 32 {
		return errors.New("preview requires a repository, source provider and a secret of at least 32 characters")
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 3 * time.Minute
	}
	s.source, s.stack = source, config
	if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: 3 * time.Second}
	}
	return nil
}

func ResourceName(envID string) string {
	sum := sha256.Sum256([]byte(envID))
	return "peo-" + hex.EncodeToString(sum[:12])
}

func labelsFor(group string) map[string]string {
	return map[string]string{ownerLabel: ownerValue, groupLabel: group}
}

func (s *RealService) provisionStack(ctx context.Context, req application.ProvisionRequest) (result application.ProvisionResult, err error) {
	if s.source == nil || req.Repo != s.stack.Repository {
		return result, errors.New("preview repository is not allowed")
	}
	group := ResourceName(req.EnvID)
	imageRef := "peo-preview:" + strings.TrimPrefix(group, "peo-")
	log.Printf("[docker] env=%s step=build commit=%s", group, req.CommitSHA)
	succeeded := false
	defer func() {
		if !succeeded {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
			defer cancel()
			if cleanupErr := s.destroyStack(cleanupCtx, group); cleanupErr != nil {
				result.ContainerID = group // Track leftovers for grace-period cleanup.
				result.ResourceGroup = group
				err = errors.Join(err, fmt.Errorf("preview rollback: %w", cleanupErr))
			}
		}
	}()
	if err = s.buildCommit(ctx, req, imageRef, group); err != nil {
		return result, err
	}
	if _, err = s.cli.NetworkCreate(ctx, group, network.CreateOptions{Driver: "bridge", Labels: labelsFor(group)}); err != nil {
		return result, fmt.Errorf("create preview network: %w", err)
	}
	if s.stack.ControlContainer != "" {
		if err = s.cli.NetworkConnect(ctx, group, s.stack.ControlContainer, nil); err != nil {
			return result, fmt.Errorf("connect orchestrator to preview: %w", err)
		}
	}
	secret := func(purpose string) string { return application.PreviewSecret(s.stack.Secret, req.EnvID, purpose)[:32] }
	dbPassword, redisPassword, rabbitPassword := secret("database"), secret("redis"), secret("rabbitmq")
	deps := []struct {
		name, image      string
		env, cmd, health []string
		memory           int64
	}{
		{"db", "postgres:15-alpine", []string{"POSTGRES_DB=todolist", "POSTGRES_USER=preview", "POSTGRES_PASSWORD=" + dbPassword}, nil, []string{"CMD", "pg_isready", "-h", "127.0.0.1", "-U", "preview", "-d", "todolist"}, 256 << 20},
		{"redis", "redis:7-alpine", []string{"REDISCLI_AUTH=" + redisPassword}, []string{"redis-server", "--requirepass", redisPassword, "--save", "", "--appendonly", "no"}, []string{"CMD", "redis-cli", "ping"}, 128 << 20},
		{"rabbitmq", "rabbitmq:3-alpine", []string{"RABBITMQ_DEFAULT_USER=preview", "RABBITMQ_DEFAULT_PASS=" + rabbitPassword}, nil, []string{"CMD", "rabbitmq-diagnostics", "-q", "ping"}, 512 << 20},
		{"mail", "axllent/mailpit:latest", nil, nil, nil, 128 << 20},
	}
	var healthIDs []string
	for _, dep := range deps {
		log.Printf("[docker] env=%s step=start-%s", group, dep.name)
		if err = s.ensureImage(ctx, dep.image); err != nil {
			return result, fmt.Errorf("prepare %s image: %w", dep.name, err)
		}
		config := &container.Config{Image: dep.image, Env: dep.env, Cmd: dep.cmd, Labels: labelsFor(group)}
		if dep.health != nil {
			config.Healthcheck = &container.HealthConfig{Test: dep.health, Interval: 2 * time.Second, Timeout: 5 * time.Second, Retries: 60, StartPeriod: 10 * time.Second}
		}
		id, createErr := s.createInNetwork(ctx, group, dep.name, config, dep.memory, false)
		if createErr != nil {
			return result, createErr
		}
		if dep.health != nil {
			healthIDs = append(healthIDs, id)
		}
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, s.stack.ReadyTimeout)
	defer readyCancel()
	log.Printf("[docker] env=%s step=wait-dependencies", group)
	for _, id := range healthIDs {
		if err = s.waitHealthy(readyCtx, id); err != nil {
			return result, fmt.Errorf("dependency %s readiness: %w", id, err)
		}
	}
	appEnv := []string{
		"SPRING_DATASOURCE_URL=jdbc:postgresql://db:5432/todolist",
		"SPRING_DATASOURCE_USERNAME=preview", "SPRING_DATASOURCE_PASSWORD=" + dbPassword,
		"REDIS_HOST=redis", "REDIS_PORT=6379", "REDIS_PASSWORD=" + redisPassword,
		"RABBITMQ_HOST=rabbitmq", "RABBITMQ_PORT=5672", "RABBITMQ_USER=preview", "RABBITMQ_PASSWORD=" + rabbitPassword,
		"JWT_SECRET=" + application.PreviewSecret(s.stack.Secret, req.EnvID, "jwt"),
		"APP_BOOTSTRAP_ENABLED=true", "SPRING_INIT_ADMIN_USERNAME=previewadmin",
		"SPRING_INIT_ADMIN_PASSWORD=" + secret("admin"), "SPRING_INIT_ADMIN_EMAIL=previewadmin@example.invalid",
		"SMTP_HOST=mail", "SMTP_PORT=1025", "GMAIL_USER=preview@example.invalid", "GMAIL_PASS=",
		"SPRING_MAIL_PROPERTIES_MAIL_SMTP_AUTH=false", "SPRING_MAIL_PROPERTIES_MAIL_SMTP_STARTTLS_ENABLE=false",
		"AI_MODEL_CHAT=none", "AI_PRIORITY_ENABLED=false", "JAVA_TOOL_OPTIONS=-XX:MaxRAMPercentage=65.0",
	}
	log.Printf("[docker] env=%s step=start-backend", group)
	appID, err := s.createInNetwork(ctx, group, "app", &container.Config{Image: imageRef, Env: appEnv, Labels: labelsFor(group), ExposedPorts: nat.PortSet{"8080/tcp": {}}}, 768<<20, true)
	if err != nil {
		return result, err
	}
	inspect, err := s.cli.ContainerInspect(ctx, appID)
	if err != nil {
		return result, err
	}
	previewURL := buildURL(inspect)
	if previewURL == "" {
		return result, errors.New("preview has no published HTTP port")
	}
	probeURL := previewURL
	if s.stack.ControlContainer != "" {
		probeURL = "http://" + group + "-app:8080"
	}
	appReadyCtx, appReadyCancel := context.WithTimeout(ctx, s.stack.ReadyTimeout)
	defer appReadyCancel()
	if err = s.waitBackend(appReadyCtx, appID, probeURL+"/actuator/health"); err != nil {
		return result, err
	}
	succeeded = true
	log.Printf("[docker] env=%s step=ready url=%s", group, previewURL)
	return application.ProvisionResult{ContainerID: appID, URL: previewURL, ResourceGroup: group}, nil
}

func (s *RealService) buildCommit(ctx context.Context, req application.ProvisionRequest, tag, group string) error {
	if err := reportStage(ctx, group, "wait-build-slot", func() error {
		select {
		case s.buildGate <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		return err
	}
	defer func() { <-s.buildGate }()
	var archive io.ReadCloser
	if err := reportStage(ctx, group, "source-download", func() error {
		var err error
		archive, err = s.source.Archive(ctx, req.Repo, req.CommitSHA)
		return err
	}); err != nil {
		return err
	}
	defer archive.Close()
	labels := labelsFor(group)
	labels["org.opencontainers.image.revision"] = req.CommitSHA
	if err := reportStage(ctx, group, "docker-build", func() error {
		response, err := s.cli.ImageBuild(ctx, archive, build.ImageBuildOptions{
			Tags: []string{tag}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true,
			Version: build.BuilderV1, Labels: labels,
		})
		if err != nil {
			return fmt.Errorf("build commit: %w", err)
		}
		defer response.Body.Close()
		if err := consumeDockerStream(response.Body, func(line string) {
			for _, part := range strings.Split(redactBuildLog(line), "\n") {
				if strings.TrimSpace(part) == "" {
					continue
				}
				if len(part) > 2048 {
					part = part[:2048] + "…"
				}
				log.Printf("[docker-build] env=%s %s", group, part)
			}
		}); err != nil {
			return fmt.Errorf("build commit: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	_, err := s.cli.ImageInspect(ctx, tag)
	return err
}

func consumeDockerStream(reader io.Reader, progress ...func(string)) error {
	decoder := json.NewDecoder(reader)
	lastStatus := make(map[string]string)
	for {
		var message struct {
			Stream      string `json:"stream"`
			Status      string `json:"status"`
			ID          string `json:"id"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&message); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if message.Error != "" {
			return errors.New(redactBuildLog(message.Error))
		}
		if message.ErrorDetail.Message != "" {
			return errors.New(redactBuildLog(message.ErrorDetail.Message))
		}
		line := message.Stream
		if line == "" && message.Status != "" && lastStatus[message.ID] != message.Status {
			line = strings.TrimSpace(message.ID + " " + message.Status)
			lastStatus[message.ID] = message.Status
		}
		if line != "" {
			for _, notify := range progress {
				notify(redactBuildLog(line))
			}
		}
	}
}

func (s *RealService) createInNetwork(ctx context.Context, group, role string, config *container.Config, memory int64, publish bool) (string, error) {
	pids := int64(512)
	host := &container.HostConfig{Resources: container.Resources{Memory: memory, PidsLimit: &pids}, SecurityOpt: []string{"no-new-privileges:true"}, LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "5m", "max-file": "2"}}}
	if publish {
		host.PortBindings = nat.PortMap{"8080/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}}}
		host.CapDrop = []string{"ALL"}
	}
	created, err := s.cli.ContainerCreate(ctx, config, host, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{group: {Aliases: []string{role}}}}, nil, group+"-"+role)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", role, err)
	}
	if err = s.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start %s: %w", role, err)
	}
	return created.ID, nil
}

func (s *RealService) waitHealthy(ctx context.Context, id string) error {
	return poll(ctx, func() (bool, error) {
		inspect, err := s.cli.ContainerInspect(ctx, id)
		if err != nil {
			return false, err
		}
		if inspect.State == nil || !inspect.State.Running {
			return false, fmt.Errorf("dependency %s stopped before ready", inspect.Name)
		}
		return inspect.State.Health != nil && inspect.State.Health.Status == "healthy", nil
	})
}

func (s *RealService) waitBackend(ctx context.Context, id, endpoint string) error {
	client := s.httpClient
	return poll(ctx, func() (bool, error) {
		inspect, err := s.cli.ContainerInspect(ctx, id)
		if err != nil {
			return false, err
		}
		if inspect.State == nil || !inspect.State.Running {
			return false, fmt.Errorf("backend stopped before readiness; inspect logs for %s", inspect.Name)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		var health struct {
			Status string `json:"status"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health)
		return resp.StatusCode == http.StatusOK && decodeErr == nil && health.Status == "UP", nil
	})
}

func poll(ctx context.Context, check func() (bool, error)) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ready, err := check()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("preview readiness timed out or cancelled: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *RealService) destroyStack(ctx context.Context, group string) error {
	filter := filters.NewArgs(filters.Arg("label", ownerLabel+"="+ownerValue), filters.Arg("label", groupLabel+"="+group))
	containers, err := s.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filter})
	if err != nil {
		return err
	}
	var failures []error
	for _, ctr := range containers {
		if err := s.cli.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	netInfo, inspectErr := s.cli.NetworkInspect(ctx, group, network.InspectOptions{})
	if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
		return inspectErr
	}
	if inspectErr == nil {
		if netInfo.Labels[ownerLabel] != ownerValue || netInfo.Labels[groupLabel] != group {
			return errors.New("refusing to remove an unmanaged network")
		}
		if s.stack.ControlContainer != "" {
			err = s.cli.NetworkDisconnect(ctx, group, s.stack.ControlContainer, true)
			if err != nil && !errdefs.IsNotFound(err) {
				log.Printf("[docker] network detach group=%s: %v", group, err)
			}
		}
		if err = s.cli.NetworkRemove(ctx, group); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	tag := "peo-preview:" + strings.TrimPrefix(group, "peo-")
	inspect, err := s.cli.ImageInspect(ctx, tag)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if inspect.Config == nil || inspect.Config.Labels[ownerLabel] != ownerValue || inspect.Config.Labels[groupLabel] != group {
		return errors.New("refusing to remove unmanaged preview image")
	}
	_, err = s.cli.ImageRemove(ctx, tag, image.RemoveOptions{Force: false, PruneChildren: false})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}
