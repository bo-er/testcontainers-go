package testcontainers

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-connections/nat"

	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	TestcontainerLabel          = "org.testcontainers.golang"
	TestcontainerLabelSessionID = TestcontainerLabel + ".sessionId"
	TestcontainerLabelIsReaper  = TestcontainerLabel + ".reaper"

	ReaperDefaultImage = "docker.io/testcontainers/ryuk:0.3.4"
)

type reaperContextKey string

var (
	dockerHostContextKey = reaperContextKey("docker_host")
	reaper               *Reaper // We would like to create reaper only once
	mutex                sync.Mutex
)

// ReaperProvider represents a provider for the reaper to run itself with
// The ContainerProvider interface should usually satisfy this as well, so it is pluggable
type ReaperProvider interface {
	RunContainer(ctx context.Context, req ContainerRequest) (Container, error)
	Config() TestContainersConfig
}

// NewReaper creates a Reaper with a sessionID to identify containers and a provider to use
// Deprecated: it's not possible to create a reaper anymore.
func NewReaper(ctx context.Context, sessionID string, provider ReaperProvider, reaperImageName string) (*Reaper, error) {
	return newReaper(ctx, sessionID, provider, WithImageName(reaperImageName))
}

// newReaper creates a Reaper with a sessionID to identify containers and a provider to use
func newReaper(ctx context.Context, sessionID string, provider ReaperProvider, opts ...ContainerOption) (*Reaper, error) {
	mutex.Lock()
	defer mutex.Unlock()
	// If reaper already exists re-use it
	if reaper != nil {
		return reaper, nil
	}

	dockerHost := extractDockerHost(ctx)

	// Otherwise create a new one
	reaper = &Reaper{
		Provider:  provider,
		SessionID: sessionID,
	}

	listeningPort := nat.Port("8080/tcp")

	reaperOpts := containerOptions{}

	for _, opt := range opts {
		opt(&reaperOpts)
	}

	req := ContainerRequest{
		Image:        reaperImage(reaperOpts.ImageName),
		ExposedPorts: []string{string(listeningPort)},
		NetworkMode:  Bridge,
		Labels: map[string]string{
			TestcontainerLabelIsReaper: "true",
		},
		SkipReaper:    true,
		RegistryCred:  reaperOpts.RegistryCredentials,
		Mounts:        Mounts(BindMount(dockerHost, "/var/run/docker.sock")),
		AutoRemove:    true,
		WaitingFor:    wait.ForListeningPort(listeningPort),
		ReaperOptions: opts,
	}

	// keep backwards compatibility
	req.ReaperImage = req.Image

	// include reaper-specific labels to the reaper container
	for k, v := range reaper.Labels() {
		req.Labels[k] = v
	}

	tcConfig := provider.Config()
	req.Privileged = tcConfig.RyukPrivileged

	// Attach reaper container to a requested network if it is specified
	if p, ok := provider.(*DockerProvider); ok {
		req.Networks = append(req.Networks, p.DefaultNetwork)
	}

	c, err := provider.RunContainer(ctx, req)
	if err != nil {
		return nil, err
	}

	endpoint, err := c.PortEndpoint(ctx, "8080", "")
	if err != nil {
		return nil, err
	}
	reaper.Endpoint = endpoint

	return reaper, nil
}

// Reaper is used to start a sidecar container that cleans up resources
type Reaper struct {
	Provider  ReaperProvider
	SessionID string
	Endpoint  string
}

// Connect runs a goroutine which can be terminated by sending true into the returned channel
func (r *Reaper) Connect() (chan bool, error) {
	conn, err := net.DialTimeout("tcp", r.Endpoint, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w: Connecting to Ryuk on %s failed", err, r.Endpoint)
	}

	terminationSignal := make(chan bool)
	go func(conn net.Conn) {
		sock := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		defer conn.Close()

		labelFilters := []string{}
		for l, v := range r.Labels() {
			labelFilters = append(labelFilters, fmt.Sprintf("label=%s=%s", l, v))
		}

		retryLimit := 3
		for retryLimit > 0 {
			retryLimit--

			if _, err := sock.WriteString(strings.Join(labelFilters, "&")); err != nil {
				continue
			}

			if _, err := sock.WriteString("\n"); err != nil {
				continue
			}

			if err := sock.Flush(); err != nil {
				continue
			}

			resp, err := sock.ReadString('\n')
			if err != nil {
				continue
			}

			if resp == "ACK\n" {
				break
			}
		}

		<-terminationSignal
	}(conn)
	return terminationSignal, nil
}

// Labels returns the container labels to use so that this Reaper cleans them up
func (r *Reaper) Labels() map[string]string {
	return map[string]string{
		TestcontainerLabel:          "true",
		TestcontainerLabelSessionID: r.SessionID,
	}
}

func extractDockerHost(ctx context.Context) (dockerHostPath string) {
	if dockerHostPath = os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"); dockerHostPath != "" {
		return dockerHostPath
	}

	dockerHostPath = "/var/run/docker.sock"

	var hostRawURL string
	if h, ok := ctx.Value(dockerHostContextKey).(string); !ok || h == "" {
		return dockerHostPath
	} else {
		hostRawURL = h
	}
	var hostURL *url.URL
	if u, err := url.Parse(hostRawURL); err != nil {
		return dockerHostPath
	} else {
		hostURL = u
	}

	switch hostURL.Scheme {
	case "unix":
		return hostURL.Path
	default:
		return dockerHostPath
	}
}

func reaperImage(reaperImageName string) string {
	if reaperImageName == "" {
		return ReaperDefaultImage
	}
	return reaperImageName
}
