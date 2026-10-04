package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/pigeaca/agentium/internal/container"
	"github.com/pigeaca/agentium/internal/home"
)

// DockerClient is what the images and clean commands, and recovery, need of the Docker driver (*container.Docker):
// the pinned images' state and their consented pull and build, and the data folder's containers and volumes.
type DockerClient interface {
	Engine() container.Engine
	Close() error
	Plan(ctx context.Context, pins []container.Pin, built container.BuiltImages) (container.ImagePlan, error)
	Fetch(ctx context.Context, it container.PlanItem, data string, out io.Writer) (container.Built, error)
	LocalImages(ctx context.Context) ([]container.LocalImage, error)
	RemoveImage(ctx context.Context, ref string) error
	Inventory(ctx context.Context, data string) ([]container.Item, error)
	RemoveIdle(ctx context.Context, data string, it container.Item) error
	RemoveRun(ctx context.Context, data, run string) error
}

// SystemDocker opens the user's local Docker daemon (Env.Docker in main): the endpoint the user's client would use,
// refused unless local, then pinned, with an empty client configuration (container.Open).
func SystemDocker(ctx context.Context, environ []string) (DockerClient, error) {
	d, err := container.Open(ctx, container.Options{Environ: environ})
	if err != nil {
		return nil, err // never a typed nil in the interface
	}
	return d, nil
}

// errNoDocker: the command needs Docker, and Env.Docker is not set (tests, and builds without it).
var errNoDocker = errors.New("Docker is not set up for Agentium here")

// docker opens the daemon through Env.Docker.
func (env Env) docker(ctx context.Context) (DockerClient, error) {
	if env.Docker == nil {
		return nil, errNoDocker
	}
	var environ []string
	if env.Environ != nil {
		environ = env.Environ()
	}
	return env.Docker(ctx, environ)
}

// dataID is the data folder's ID in container names and labels (container.DataID of its resolved path).
func dataID(layout home.Layout) string {
	root := layout.Root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return container.DataID(root)
}

// builtImagesPath is the data folder's record of the grading images built for it (container.BuiltImages).
func builtImagesPath(layout home.Layout) string {
	return filepath.Join(layout.Root, "container-images.json")
}

// containerRemover is recovery's removal of what dead runs' grades left in Docker (run.RecoverContainers), opening
// Docker only when a run left a marker; closeFn closes it.
func (env Env) containerRemover(layout home.Layout) (remove func(ctx context.Context, run string) error, closeFn func()) {
	var d DockerClient
	var openErr error
	opened := false
	data := dataID(layout)
	if env.Docker == nil {
		return nil, func() {} // recovery then warns about each run that had a container
	}
	remove = func(ctx context.Context, run string) error {
		if !opened {
			opened = true
			d, openErr = env.docker(ctx)
		}
		if openErr != nil {
			return openErr
		}
		return d.RemoveRun(ctx, data, run)
	}
	return remove, func() {
		if d != nil {
			d.Close()
		}
	}
}
