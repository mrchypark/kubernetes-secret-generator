package observability

import (
	"context"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

const (
	defaultAPIProbeInterval = 15 * time.Second
	defaultAPIProbeTimeout  = 2 * time.Second
)

type APIConnectivityProbe struct {
	probe    func(context.Context) error
	interval time.Duration
	timeout  time.Duration
	ticks    <-chan time.Time
	after    func(error)
}

func NewAPIConnectivityProbe(config *rest.Config) (*APIConnectivityProbe, error) {
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return &APIConnectivityProbe{
		probe: func(ctx context.Context) error {
			return client.RESTClient().Get().AbsPath("/readyz").Do(ctx).Error()
		},
		interval: defaultAPIProbeInterval,
		timeout:  defaultAPIProbeTimeout,
	}, nil
}

func (p *APIConnectivityProbe) Start(ctx context.Context) error {
	p.observe(ctx)
	if p.ticks != nil {
		return p.run(ctx, p.ticks)
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	return p.run(ctx, ticker.C)
}

// The connectivity signal must run on passive replicas so idle outages remain
// visible independently of leader election.
func (*APIConnectivityProbe) NeedLeaderElection() bool { return false }

func (p *APIConnectivityProbe) run(ctx context.Context, ticks <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return nil
			}
			p.observe(ctx)
		}
	}
}

func (p *APIConnectivityProbe) observe(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	err := p.probe(ctx)
	ObserveAPIResult(ControllerManager, err)
	if p.after != nil {
		p.after(err)
	}
}
