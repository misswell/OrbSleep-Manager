package engine

import "context"

// composeCLI is the subset of the docker CLI used by the engine. It is an
// interface so transaction behaviour can be tested with fault injection.
type composeCLI interface {
	ComposeConfig(ctx context.Context, project, file, workdir string) (map[string]any, error)
	UpService(ctx context.Context, project, file, workdir, service string) error
	ProxyUp(ctx context.Context, file string) error
	ProxyDown(ctx context.Context, file string) error
	Version(ctx context.Context) (string, error)
}
