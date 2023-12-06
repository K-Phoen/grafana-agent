package echo

import (
	"context"
	"sync"

	"github.com/grafana/agent/component"
	"github.com/grafana/agent/component/grafana"
	"github.com/grafana/agent/pkg/flow/logging/level"
)

func init() {
	component.Register(component.Registration{
		Name:    "grafana.echo",
		Args:    Arguments{},
		Exports: Exports{},

		Build: func(opts component.Options, args component.Arguments) (component.Component, error) {
			return New(opts, args.(Arguments))
		},
	})
}

// Arguments holds values which are used to configure the grafana.echo
// component.
type Arguments struct{}

// Exports holds the values exported by the grafana.echo component.
type Exports struct {
	Receiver grafana.Appendable `river:"receiver,attr"`
}

// DefaultArguments defines the default settings for echoing things.
var DefaultArguments = Arguments{}

// SetToDefault implements river.Defaulter.
func (args *Arguments) SetToDefault() {
	*args = DefaultArguments
}

var (
	_ component.Component = (*Component)(nil)
)

// Component implements the grafana.echo component.
type Component struct {
	opts component.Options

	mut      sync.RWMutex
	args     Arguments
	receiver grafana.AppendableFunc
}

// New creates a new grafana.echo component.
func New(o component.Options, args Arguments) (*Component, error) {
	c := &Component{
		opts: o,
		receiver: func(ctx context.Context, raw grafana.RawResources) error {
			level.Info(o.Logger).Log("msg", "grafana.echo", "resources", string(raw))

			return nil
		},
	}

	// Call to Update() once at the start.
	if err := c.Update(args); err != nil {
		return nil, err
	}

	// Immediately export the receiver which remains the same for the component
	// lifetime.
	o.OnStateChange(Exports{Receiver: c.receiver})

	return c, nil
}

// Run implements component.Component.
func (c *Component) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		}
	}
}

// Update implements component.Component.
func (c *Component) Update(args component.Arguments) error {
	newArgs := args.(Arguments)

	c.mut.Lock()
	defer c.mut.Unlock()
	c.args = newArgs

	return nil
}
