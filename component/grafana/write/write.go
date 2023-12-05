package echo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/grafana/agent/component"
	common_config "github.com/grafana/agent/component/common/config"
	"github.com/grafana/agent/component/grafana"
	"github.com/grafana/agent/pkg/flow/logging/level"
	"github.com/grafana/river/rivertypes"
	prom_config "github.com/prometheus/common/config"
)

func init() {
	component.Register(component.Registration{
		Name:    "grafana.write",
		Args:    Arguments{},
		Exports: Exports{},

		Build: func(opts component.Options, args component.Arguments) (component.Component, error) {
			return New(opts, args.(Arguments))
		},
	})
}

// Arguments holds values which are used to configure the grafana.write
// component.
type Arguments struct {
	Host   string            `river:"host,attr"`
	Scheme string            `river:"scheme,attr,optional"`
	Path   string            `river:"path,attr,optional"`
	Token  rivertypes.Secret `river:"token,attr,optional"`

	HTTPClientConfig common_config.HTTPClientConfig `river:"client,block,optional"`
}

// Exports holds the values exported by the grafana.write component.
type Exports struct {
	Receiver grafana.Appendable `river:"receiver,attr"`
}

// DefaultArguments defines the default settings for writing to Grafana.
var DefaultArguments = Arguments{
	Scheme: "http",
}

// SetToDefault implements river.Defaulter.
func (args *Arguments) SetToDefault() {
	*args = DefaultArguments
}

var (
	_ component.Component = (*Component)(nil)
)

// Component implements the loki.source.file component.
type Component struct {
	opts component.Options

	mut  sync.RWMutex
	args Arguments
	http *http.Client
}

// New creates a new loki.echo component.
func New(o component.Options, args Arguments) (*Component, error) {
	c := &Component{
		opts: o,
	}

	// Call to Update() once at the start.
	if err := c.Update(args); err != nil {
		return nil, err
	}

	// Immediately export the receiver which remains the same for the component
	// lifetime.
	o.OnStateChange(Exports{Receiver: grafana.AppendableFunc(c.dashboardsReceiver)})

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

	cli, err := prom_config.NewClientFromConfig(*newArgs.HTTPClientConfig.Convert(), c.opts.ID)
	if err != nil {
		return err
	}
	c.http = cli

	return nil
}

func (c *Component) dashboardsReceiver(_ context.Context, raw grafana.RawDashboards) error {
	rawDashboards := []grafana.Dashboard{}
	if err := json.Unmarshal(raw, &rawDashboards); err != nil {
		level.Error(c.opts.Logger).Log("msg", "could not unmarshal dashboards list", "err", err)
		return err
	}

	for _, dashboard := range rawDashboards {
		if err := c.persistRawDashboard(dashboard); err != nil {
			level.Error(c.opts.Logger).Log("msg", "could not persist dashboard", "err", err)
		}
	}

	return nil
}

func (c *Component) persistRawDashboard(dashboard grafana.Dashboard) error {
	// the API will return an error if an ID is present in the JSON but doesn't exist in Grafana
	delete(dashboard, "id")

	dashboardJSON, err := json.Marshal(struct {
		Dashboard *grafana.Dashboard `json:"dashboard"`
		FolderID  uint               `json:"folderId"`
		Overwrite bool               `json:"overwrite"`
	}{
		Dashboard: &dashboard,
		FolderID:  1, // TODO
		Overwrite: true,
	})
	if err != nil {
		return err
	}

	ctx := context.Background()

	url := fmt.Sprintf("%s://%s%s", c.args.Scheme, c.args.Host, c.args.Path)
	url += "/api/dashboards/db"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(dashboardJSON))
	if err != nil {
		return err
	}

	token := &rivertypes.OptionalSecret{}
	if err := c.args.Token.ConvertInto(token); err != nil {
		return err
	}

	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("Authorization", "Bearer "+token.Value)

	resp, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return c.httpError(resp)
	}

	return nil
}

func (c *Component) httpError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	return fmt.Errorf("could not query grafana: %s (HTTP status %d)", body, resp.StatusCode)
}
