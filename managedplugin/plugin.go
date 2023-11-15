package managedplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	containerSpecs "github.com/opencontainers/image-spec/specs-go/v1"
	pbBase "github.com/pixisai/loki-plugin-pb-go/pb/base"
	pbDiscovery "github.com/pixisai/loki-plugin-pb-go/pb/discovery"
	pbSource "github.com/pixisai/loki-plugin-pb-go/pb/source"
	"github.com/rs/zerolog"
	"golang.org/x/exp/slices"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultDownloadDir = ".loki"
	maxMsgSize         = 100 * 1024 * 1024 // 100 MiB

	containerPortMappingRetries           = 30
	containerPortMappingInitialRetryDelay = 100 * time.Millisecond

	containerRunningRetries           = 30
	containerRunningInitialRetryDelay = 100 * time.Millisecond

	containerServerHealthyRetries           = 30
	containerServerHealthyInitialRetryDelay = 100 * time.Millisecond

	containerStopTimeoutSeconds = 10
)

// PluginType specifies if a plugin is a source or a destination.
// It actually doesn't really have any effect as plugins can serve both as source and as destinations,
// but it is here for backward compatibility.
type PluginType int

const (
	PluginSource PluginType = iota
	PluginDestination
)

func (p PluginType) String() string {
	return [...]string{"source", "destination"}[p]
}

type Clients []*Client

type Config struct {
	Name     string
	Registry Registry
	Path     string
	Version  string
}

type Client struct {
	directory            string
	cmd                  *exec.Cmd
	logger               zerolog.Logger
	LocalPath            string
	grpcSocketName       string
	containerID          string
	logReader            io.ReadCloser
	wg                   *sync.WaitGroup
	Conn                 *grpc.ClientConn
	config               Config
	noSentry             bool
	noExec               bool
	otelEndpoint         string
	otelEndpointInsecure bool
	metrics              *Metrics
	registry             Registry
	authToken            string
	teamName             string
}

// typ will be deprecated soon but now required for a transition period
func NewClients(ctx context.Context, typ PluginType, specs []Config, opts ...Option) (Clients, error) {
	clients := make(Clients, len(specs))
	for i, spec := range specs {
		client, err := NewClient(ctx, typ, spec, opts...)
		if err != nil {
			return nil, err
		}
		clients[i] = client
	}
	return clients, nil
}

func (c Clients) ClientByName(name string) *Client {
	for _, client := range c {
		if client.config.Name == name {
			return client
		}
	}
	return nil
}

func (c Clients) Terminate() error {
	for _, client := range c {
		if err := client.Terminate(); err != nil {
			return err
		}
	}
	return nil
}

// NewClient creates a new plugin client.
// If registrySpec is GitHub then client downloads the plugin, spawns it and creates a gRPC connection.
// If registrySpec is Local then client spawns the plugin and creates a gRPC connection.
// If registrySpec is gRPC then clients creates a new connection
// If registrySpec is Docker then client downloads the docker image, runs it and creates a gRPC connection.
func NewClient(ctx context.Context, typ PluginType, config Config, opts ...Option) (*Client, error) {
	c := &Client{
		directory: defaultDownloadDir,
		wg:        &sync.WaitGroup{},
		config:    config,
		metrics:   &Metrics{},
		registry:  config.Registry,
	}
	for _, opt := range opts {
		opt(c)
	}
	if err := c.downloadPlugin(ctx, typ); err != nil {
		return nil, err
	}
	if !c.noExec {
		if err := c.execPlugin(ctx); err != nil {
			return nil, err
		}
	}

	return c, nil
}

func (c *Client) downloadPlugin(ctx context.Context, typ PluginType) error {
	switch c.config.Registry {
	case RegistryGrpc:
		return nil // GRPC plugins are not downloaded
	case RegistryLocal:
		return validateLocalExecPath(c.config.Path)
	case RegistryGithub:
		pathSplit := strings.Split(c.config.Path, "/")
		if len(pathSplit) != 2 {
			return fmt.Errorf("invalid github plugin path: %s. format should be owner/repo", c.config.Path)
		}
		org, name := pathSplit[0], pathSplit[1]
		c.LocalPath = filepath.Join(c.directory, "plugins", typ.String(), org, name, c.config.Version, "plugin")
		c.LocalPath = WithBinarySuffix(c.LocalPath)
		return DownloadPluginFromGithub(ctx, c.LocalPath, org, name, c.config.Version, typ)
	case RegistryDocker:
		if imageAvailable, err := isDockerImageAvailable(ctx, c.config.Path); err != nil {
			return err
		} else if !imageAvailable {
			return pullDockerImage(ctx, c.config.Path)
		}
		return nil
	case RegistryLoki:
		pathSplit := strings.Split(c.config.Path, "/")
		if len(pathSplit) != 2 {
			return fmt.Errorf("invalid loki plugin path: %s. format should be team/name", c.config.Path)
		}
		org, name := pathSplit[0], pathSplit[1]
		c.LocalPath = filepath.Join(c.directory, "plugins", typ.String(), org, name, c.config.Version, "plugin")
		c.LocalPath = WithBinarySuffix(c.LocalPath)
		return DownloadPluginFromHub(ctx, HubDownloadOptions{
			AuthToken:     c.authToken,
			TeamName:      c.teamName,
			LocalPath:     c.LocalPath,
			PluginTeam:    org,
			PluginKind:    typ.String(),
			PluginName:    name,
			PluginVersion: c.config.Version,
		})
	default:
		return fmt.Errorf("unknown registry %s", c.config.Registry.String())
	}
}

func (c *Client) execPlugin(ctx context.Context) error {
	switch c.config.Registry {
	case RegistryGrpc:
		return c.connectUsingTCP(ctx, c.config.Path)
	case RegistryLocal:
		return c.startLocal(ctx, c.config.Path)
	case RegistryGithub:
		return c.startLocal(ctx, c.LocalPath)
	case RegistryDocker:
		return c.startDockerPlugin(ctx, c.config.Path)
	case RegistryLoki:
		return c.startLocal(ctx, c.LocalPath)
	default:
		return fmt.Errorf("unknown registry %s", c.config.Registry.String())
	}
}

func (c *Client) ConnectionString() string {
	tgt := c.Conn.Target()
	switch c.registry {
	case RegistryGrpc:
		return tgt
	case RegistryLocal,
		RegistryGithub,
		RegistryLoki:
		return "unix://" + tgt
	case RegistryDocker:
		return tgt
	}
	return tgt
}

func (c *Client) Metrics() Metrics {
	return *c.metrics
}

func (c *Client) startDockerPlugin(ctx context.Context, configPath string) error {
	cli, err := dockerClient.NewClientWithOpts(dockerClient.FromEnv)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	pluginArgs := c.getPluginArgs()
	config := &container.Config{
		ExposedPorts: nat.PortSet{
			"7777/tcp": struct{}{},
		},
		Image: configPath,
		Cmd:   pluginArgs,
		Tty:   true,
	}
	hostConfig := &container.HostConfig{
		PortBindings: map[nat.Port][]nat.PortBinding{
			"7777/tcp": {
				{
					HostIP:   "localhost",
					HostPort: "", // let host assign a random unused port
				},
			},
		},
	}
	networkingConfig := &network.NetworkingConfig{}
	platform := &containerSpecs.Platform{}
	containerName := c.config.Name + "-" + uuid.New().String()
	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, platform, containerName)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}
	c.containerID = resp.ID
	if err := cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	// wait for container to start
	err = waitForContainerRunning(ctx, cli, resp.ID)
	if err != nil {
		return fmt.Errorf("error while waiting for container to reach running state: %w", err)
	}

	var hostConnection string
	err = retry.Do(func() error {
		hostConnection, err = getHostConnection(ctx, cli, resp.ID)
		return err
	}, retry.RetryIf(func(err error) bool {
		return err.Error() == "failed to get port mapping for container"
	}),
		// this should generally succeed on first or second try, because we're only waiting for the container to start
		// to get the port mapping, not the plugin to start. The plugin will be waited for when we establish the tcp
		// connection.
		retry.Attempts(containerPortMappingRetries),
		retry.Delay(containerPortMappingInitialRetryDelay),
		retry.DelayType(retry.BackOffDelay),
		retry.MaxDelay(1*time.Second),
	)
	if err != nil {
		return fmt.Errorf("failed to get host connection: %w", err)
	}
	reader, err := cli.ContainerLogs(ctx, resp.ID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Details:    false,
	})
	if err != nil {
		return fmt.Errorf("failed to get reader for container logs: %w", err)
	}
	c.logReader = reader
	c.wg.Add(1)
	go c.readLogLines(reader)

	return c.connectUsingTCP(ctx, hostConnection)
}

func getHostConnection(ctx context.Context, cli *dockerClient.Client, containerID string) (string, error) {
	// Retrieve the dynamically assigned HOST port
	containerJSON, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}
	if containerJSON.State != nil {
		switch containerJSON.State.Status {
		case "removing", "exited", "dead":
			return "", errors.New("container exited prematurely with error " + containerJSON.State.Error + ", exit code " + strconv.Itoa(containerJSON.State.ExitCode) + " and status " + containerJSON.State.Status)
		}
	}
	if len(containerJSON.NetworkSettings.Ports) == 0 || len(containerJSON.NetworkSettings.Ports["7777/tcp"]) == 0 {
		return "", errors.New("failed to get port mapping for container")
	}
	hostPort := containerJSON.NetworkSettings.Ports["7777/tcp"][0].HostPort
	return "localhost:" + hostPort, nil
}

func waitForContainerRunning(ctx context.Context, cli *dockerClient.Client, containerID string) error {
	err := retry.Do(func() error {
		containerJSON, err := cli.ContainerInspect(ctx, containerID)
		if err != nil {
			return fmt.Errorf("failed to inspect container: %w", err)
		}
		if containerJSON.State != nil {
			switch containerJSON.State.Status {
			case "removing", "exited", "dead":
				return errors.New("container exited prematurely with error " + containerJSON.State.Error + ", exit code " + strconv.Itoa(containerJSON.State.ExitCode) + " and status " + containerJSON.State.Status)
			case "running":
				return nil
			}
		}
		return errors.New("container not running")
	}, retry.RetryIf(func(err error) bool {
		return err != nil
	}),
		retry.Attempts(containerRunningRetries),
		retry.Delay(containerRunningInitialRetryDelay),
		retry.DelayType(retry.BackOffDelay),
		retry.MaxDelay(1*time.Second),
	)
	return err
}

func (c *Client) startLocal(ctx context.Context, path string) error {
	c.grpcSocketName = GenerateRandomUnixSocketName()
	// spawn the plugin first and then connect
	args := c.getPluginArgs()
	cmd := exec.CommandContext(ctx, path, args...)
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = getSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start plugin %s: %w", path, err)
	}

	c.cmd = cmd

	c.logReader = reader
	c.wg.Add(1)
	go c.readLogLines(reader)

	return c.connectToUnixSocket(ctx, cmd)
}

func (c *Client) getPluginArgs() []string {
	args := []string{"serve", "--log-level", c.logger.GetLevel().String(), "--log-format", "json"}
	if c.grpcSocketName != "" {
		args = append(args, "--network", "unix", "--address", c.grpcSocketName)
	} else {
		args = append(args, "--network", "tcp", "--address", "0.0.0.0:7777")
	}
	if c.noSentry {
		args = append(args, "--no-sentry")
	}
	if c.otelEndpoint != "" {
		args = append(args, "--otel-endpoint", c.otelEndpoint)
	}
	if c.otelEndpointInsecure {
		args = append(args, "--otel-endpoint-insecure")
	}
	return args
}

func (c *Client) readLogLines(reader io.ReadCloser) {
	defer c.wg.Done()
	lr := NewLogReader(reader)
	for {
		line, err := lr.NextLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, ErrLogLineToLong) {
			c.logger.Info().Str("line", string(line)).Msg("truncated plugin log line")
			continue
		}
		if err != nil {
			c.logger.Err(err).Msg("failed to read log line from plugin")
			break
		}
		var structuredLogLine map[string]any
		if err := json.Unmarshal(line, &structuredLogLine); err != nil {
			c.logger.Err(err).Str("line", string(line)).Msg("failed to unmarshal log line from plugin")
		} else {
			c.jsonToLog(c.logger, structuredLogLine)
		}
	}
}

func (c *Client) connectUsingTCP(ctx context.Context, path string) error {
	var err error
	c.Conn, err = grpc.DialContext(ctx, path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgSize),
			grpc.MaxCallSendMsgSize(maxMsgSize),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to dial grpc source plugin at %s: %w", path, err)
	}

	return retry.Do(
		func() error {
			state := c.Conn.GetState()
			if state == connectivity.Idle || state == connectivity.Ready {
				return nil
			}
			if state == connectivity.Shutdown {
				return fmt.Errorf("connection shutdown")
			}
			return fmt.Errorf("connection not ready")
		},
		retry.RetryIf(func(err error) bool {
			return err.Error() == "connection not ready"
		}),
		retry.Delay(containerServerHealthyInitialRetryDelay),
		retry.Attempts(containerServerHealthyRetries),
		retry.DelayType(retry.BackOffDelay),
		retry.MaxDelay(1*time.Second),
	)
}

func (c *Client) connectToUnixSocket(ctx context.Context, cmd *exec.Cmd) error {
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		d := &net.Dialer{
			Timeout: 5 * time.Second,
		}
		return d.DialContext(ctx, "unix", addr)
	}
	var err error
	c.Conn, err = grpc.DialContext(ctx, c.grpcSocketName,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgSize),
			grpc.MaxCallSendMsgSize(maxMsgSize),
		))
	if err != nil {
		if err := cmd.Process.Kill(); err != nil {
			c.logger.Error().Err(err).Msg("failed to kill plugin process")
		}
		return err
	}
	return nil
}

func (c *Client) Name() string {
	return c.config.Name
}

func (c *Client) oldDiscovery(ctx context.Context) ([]int, error) {
	discoveryClient := pbDiscovery.NewDiscoveryClient(c.Conn)
	versionsRes, err := discoveryClient.GetVersions(ctx, &pbDiscovery.GetVersions_Request{})
	if err != nil {
		if isUnimplemented(err) {
			// If we get an error here, we assume that the plugin is not a v1 plugin and we try to sync it as a v0 plugin
			// this is for backward compatibility where we used incorrect versioning mechanism
			oldDiscoveryClient := pbSource.NewSourceClient(c.Conn)
			versionRes, err := oldDiscoveryClient.GetProtocolVersion(ctx, &pbBase.GetProtocolVersion_Request{})
			if err != nil {
				return nil, err
			}
			switch versionRes.Version {
			case 2:
				return []int{0}, nil
			case 1:
				return []int{-1}, nil
			default:
				return nil, fmt.Errorf("unknown protocol version %d", versionRes.Version)
			}
		}
		return nil, err
	}
	versions := make([]int, len(versionsRes.Versions))
	for i, vStr := range versionsRes.Versions {
		vStr = strings.TrimPrefix(vStr, "v")
		v, err := strconv.ParseInt(vStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse version %s: %w", vStr, err)
		}
		versions[i] = int(v)
	}
	return versions, nil
}

func (c *Client) Versions(ctx context.Context) ([]int, error) {
	discoveryClient := pbDiscovery.NewDiscoveryClient(c.Conn)
	versionsRes, err := discoveryClient.GetVersions(ctx, &pbDiscovery.GetVersions_Request{})
	if err != nil {
		if isUnimplemented(err) {
			// this was only added post v3 so clients will fallback to using an older discovery service
			return c.oldDiscovery(ctx)
		}
		return nil, err
	}
	res := make([]int, len(versionsRes.Versions))
	for i, v := range versionsRes.Versions {
		res[i] = int(v)
	}
	return res, nil
}

func (c *Client) MaxVersion(ctx context.Context) (int, error) {
	discoveryClient := pbDiscovery.NewDiscoveryClient(c.Conn)
	versionsRes, err := discoveryClient.GetVersions(ctx, &pbDiscovery.GetVersions_Request{})
	if err != nil {
		// If we get an error here, we assume that the plugin is not a v1 plugin and we try to sync it as a v0 plugin
		// this is for backward compatibility where we used incorrect versioning mechanism
		oldDiscoveryClient := pbSource.NewSourceClient(c.Conn)
		versionRes, err := oldDiscoveryClient.GetProtocolVersion(ctx, &pbBase.GetProtocolVersion_Request{})
		if err != nil {
			return -1, err
		}
		switch versionRes.Version {
		case 2:
			return 0, nil
		case 1:
			return -1, nil
		default:
			return -1, fmt.Errorf("unknown protocol version %d", versionRes.Version)
		}
	}
	if slices.Contains(versionsRes.Versions, "v2") {
		return 2, nil
	}
	if slices.Contains(versionsRes.Versions, "v1") {
		return 1, nil
	}
	if slices.Contains(versionsRes.Versions, "v0") {
		return 0, nil
	}
	return -1, fmt.Errorf("unknown protocol versions %v", versionsRes.Versions)
}

func (c *Client) Terminate() error {
	// wait for log streaming to complete before returning from this function
	defer func() {
		c.wg.Wait()
		if c.logReader != nil {
			err := c.logReader.Close()
			if err != nil {
				c.logger.Error().Err(err).Msg("failed to close log reader")
			}
		}
	}()

	if c.grpcSocketName != "" {
		defer func() {
			if err := os.RemoveAll(c.grpcSocketName); err != nil {
				c.logger.Error().Err(err).Msg("failed to remove source socket file")
			}
		}()
	}
	if c.containerID != "" {
		cli, err := dockerClient.NewClientWithOpts(dockerClient.FromEnv)
		if err != nil {
			return fmt.Errorf("failed to create Docker client: %w", err)
		}
		timeout := containerStopTimeoutSeconds
		if err := cli.ContainerStop(context.Background(), c.containerID, container.StopOptions{Timeout: &timeout}); err != nil {
			return fmt.Errorf("failed to stop container: %w", err)
		}
		if err := cli.ContainerRemove(context.Background(), c.containerID, types.ContainerRemoveOptions{}); err != nil {
			return fmt.Errorf("failed to remove container: %w", err)
		}
	}

	if c.Conn != nil {
		if err := c.Conn.Close(); err != nil {
			c.logger.Error().Err(err).Msg("failed to close gRPC connection to source plugin")
		}
		c.Conn = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		if err := c.terminateProcess(); err != nil {
			return err
		}
	}

	return nil
}

func isDirectory(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fileInfo.IsDir(), err
}

func validateLocalExecPath(filePath string) error {
	directory, err := isDirectory(filePath)
	if err != nil {
		return fmt.Errorf("error validating plugin path, %s: %w", filePath, err)
	}
	if directory {
		return fmt.Errorf("invalid plugin path: %s. Path cannot point to a directory", filePath)
	}
	return nil
}
