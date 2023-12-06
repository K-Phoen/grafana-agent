package scrape

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/grafana/agent/component"
	component_config "github.com/grafana/agent/component/common/config"
	"github.com/grafana/agent/component/discovery"
	"github.com/grafana/agent/component/grafana"
	"github.com/grafana/agent/component/prometheus/scrape"
	"github.com/grafana/agent/pkg/flow/logging/level"
	"github.com/grafana/agent/service/cluster"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

func init() {
	component.Register(component.Registration{
		Name: "grafana.scrape",
		Args: Arguments{},
		Build: func(opts component.Options, args component.Arguments) (component.Component, error) {
			return New(opts, args.(Arguments))
		},
	})
}

// Arguments holds values which are used to configure the grafana.scrape
// component.
type Arguments struct {
	Targets   []discovery.Target   `river:"targets,attr"`
	ForwardTo []grafana.Appendable `river:"forward_to,attr"`

	// The job name to override the job label with.
	JobName string `river:"job_name,attr,optional"`
	// How frequently to scrape the targets of this scrape config.
	ScrapeInterval time.Duration `river:"scrape_interval,attr,optional"`
	// The timeout for scraping targets of this config.
	ScrapeTimeout time.Duration `river:"scrape_timeout,attr,optional"`
	// The HTTP resource path on which to fetch Grafana resources from targets.
	ResourcesPath string `river:"resources_path,attr,optional"`
	// The URL scheme with which to fetch metrics from targets.
	Scheme string `river:"scheme,attr,optional"`
	// A set of query parameters with which the target is scraped.
	Params url.Values `river:"params,attr,optional"`

	HTTPClientConfig component_config.HTTPClientConfig `river:",squash"`

	Clustering cluster.ComponentBlock `river:"clustering,block,optional"`
}

// SetToDefault implements river.Defaulter.
func (arg *Arguments) SetToDefault() {
	*arg = Arguments{
		ResourcesPath: "/grafana",
		Scheme:        "http",

		HTTPClientConfig: component_config.DefaultHTTPClientConfig,
		ScrapeInterval:   1 * time.Minute,  // From config.DefaultGlobalConfig
		ScrapeTimeout:    10 * time.Second, // From config.DefaultGlobalConfig
	}
}

// Validate implements river.Validator.
func (arg *Arguments) Validate() error {
	if arg.ScrapeTimeout > arg.ScrapeInterval {
		return fmt.Errorf("scrape_timeout (%s) greater than scrape_interval (%s) for scrape config", arg.ScrapeTimeout, arg.ScrapeInterval)
	}

	// We must explicitly Validate because HTTPClientConfig is squashed, and it won't run otherwise.
	return arg.HTTPClientConfig.Validate()
}

// Component implements the grafana.scrape component.
type Component struct {
	opts    component.Options
	cluster cluster.Cluster

	reloadTargets chan struct{}

	mut        sync.RWMutex
	args       Arguments
	scraper    *Manager
	appendable *grafana.Fanout
}

var (
	_ component.Component = (*Component)(nil)
)

// New creates a new grafana.scrape component.
func New(o component.Options, args Arguments) (*Component, error) {
	data, err := o.GetServiceData(cluster.ServiceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get info about cluster service: %w", err)
	}
	clusterData := data.(cluster.Cluster)

	flowAppendable := grafana.NewFanout(args.ForwardTo, o.ID, o.Registerer)
	scraper := NewManager(flowAppendable, o.Logger)

	c := &Component{
		opts:          o,
		cluster:       clusterData,
		reloadTargets: make(chan struct{}, 1),
		scraper:       scraper,
		appendable:    flowAppendable,
	}

	// Call to Update() to set the receivers and targets once at the start.
	if err := c.Update(args); err != nil {
		return nil, err
	}

	return c, nil
}

// Run implements component.Component.
func (c *Component) Run(ctx context.Context) error {
	defer c.scraper.Stop()

	targetSetsChan := make(chan map[string][]*targetgroup.Group)

	go func() {
		c.scraper.Run(targetSetsChan)
		level.Info(c.opts.Logger).Log("msg", "scrape manager stopped")
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.reloadTargets:
			c.mut.RLock()
			var (
				tgs        = c.args.Targets
				jobName    = c.opts.ID
				clustering = c.args.Clustering.Enabled
			)
			if c.args.JobName != "" {
				jobName = c.args.JobName
			}
			c.mut.RUnlock()

			// NOTE(@tpaschalis) First approach, manually building the
			// 'clustered' targets implementation every time.
			ct := discovery.NewDistributedTargets(clustering, c.cluster, tgs)
			promTargets := c.componentTargetsToProm(jobName, ct.Get())

			select {
			case targetSetsChan <- promTargets:
				level.Debug(c.opts.Logger).Log("msg", "passed new targets to scrape manager")
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// Update implements component.Component.
func (c *Component) Update(args component.Arguments) error {
	newArgs := args.(Arguments)

	c.mut.Lock()
	defer c.mut.Unlock()
	c.args = newArgs

	c.appendable.UpdateChildren(newArgs.ForwardTo)

	err := c.scraper.ApplyConfig(newArgs)
	if err != nil {
		return fmt.Errorf("error applying scrape configs: %w", err)
	}
	level.Debug(c.opts.Logger).Log("msg", "scrape config was updated")

	select {
	case c.reloadTargets <- struct{}{}:
	default:
	}

	return nil
}

// NotifyClusterChange implements component.ClusterComponent.
func (c *Component) NotifyClusterChange() {
	c.mut.RLock()
	defer c.mut.RUnlock()

	if !c.args.Clustering.Enabled {
		return // no-op
	}

	// Schedule a reload so targets get redistributed.
	select {
	case c.reloadTargets <- struct{}{}:
	default:
	}
}

func (c *Component) componentTargetsToProm(jobName string, tgs []discovery.Target) map[string][]*targetgroup.Group {
	promGroup := &targetgroup.Group{Source: jobName}
	for _, tg := range tgs {
		promGroup.Targets = append(promGroup.Targets, convertLabelSet(tg))
	}

	return map[string][]*targetgroup.Group{jobName: {promGroup}}
}

func convertLabelSet(tg discovery.Target) model.LabelSet {
	lset := make(model.LabelSet, len(tg))
	for k, v := range tg {
		lset[model.LabelName(k)] = model.LabelValue(v)
	}
	return lset
}

// DebugInfo implements component.DebugComponent.
func (c *Component) DebugInfo() interface{} {
	var res []scrape.TargetStatus

	for job, stt := range c.scraper.TargetsActive() {
		for _, st := range stt {
			var lastError string
			if st.LastError() != nil {
				lastError = st.LastError().Error()
			}
			if st != nil {
				res = append(res, scrape.TargetStatus{
					JobName:            job,
					URL:                st.URL(),
					Health:             string(st.Health()),
					Labels:             st.discoveredLabels.Map(),
					LastError:          lastError,
					LastScrape:         st.LastScrape(),
					LastScrapeDuration: st.LastScrapeDuration(),
				})
			}
		}
	}

	return scrape.ScraperStatus{TargetStatus: res}
}
