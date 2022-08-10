package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	log "github.com/sirupsen/logrus"
	beaconbackend "github.com/skylenet/eth-proxy/pkg/backends/beacon"
	executionbackend "github.com/skylenet/eth-proxy/pkg/backends/execution"
)

type Proxy struct {
	cfg               Config
	beaconBackends    map[string]*beaconbackend.Backend
	executionBackends map[string]*executionbackend.Backend

	beaconLoadBalancer    *beaconbackend.LoadBalancer
	executionLoadBalancer *executionbackend.LoadBalancer
}

func NewProxy(conf *Config) *Proxy {
	if err := conf.Validate(); err != nil {
		log.Fatalf("can't start proxy: %s", err)
	}

	p := &Proxy{
		cfg: *conf,
	}

	// build up beacon backends
	p.beaconBackends = make(map[string]*beaconbackend.Backend)
	beaconBackends := make([]*beaconbackend.Backend, len(conf.BeaconUpstreams))

	for i, b := range conf.BeaconUpstreams {
		u, err := url.Parse(b.Address)
		if err != nil {
			log.Fatalf("beacon upstream %s has an invalid url: %s (%v)", b.Name, b.Address, err)
		}

		be := beaconbackend.NewBackend(beaconbackend.Config{
			URL:           u,
			APIAllowPaths: conf.BeaconConfig.APIAllowPaths,
		})
		p.beaconBackends[b.Name] = be
		beaconBackends[i] = be
	}

	p.beaconLoadBalancer = beaconbackend.NewLoadBalancer(beaconBackends)

	// build up execution backends
	p.executionBackends = make(map[string]*executionbackend.Backend)
	executionBackends := make([]*executionbackend.Backend, len(conf.ExecutionUpstreams))

	for i, b := range conf.ExecutionUpstreams {
		u, err := url.Parse(b.Address)
		if err != nil {
			log.Fatalf("execution upstream %s has an invalid url: %s (%v)", b.Name, b.Address, err)
		}

		wsu, err := url.Parse(b.WsAddress)
		if err != nil {
			log.Fatalf("execution upstream %s has an invalid websocket url: %s (%v)", b.Name, b.WsAddress, err)
		}

		be := executionbackend.NewBackend(executionbackend.Config{
			URL:             u,
			WebsocketURL:    wsu,
			RPCAllowMethods: conf.ExecutionConfig.RPCAllowMethods,
		})
		p.executionBackends[b.Name] = be
		executionBackends[i] = be
	}

	p.executionLoadBalancer = executionbackend.NewLoadBalancer(executionBackends)

	return p
}

func (p *Proxy) Serve() error {
	for _, upstream := range p.cfg.BeaconConfig.BeaconUpstreams {
		endpoint := fmt.Sprintf("/proxy/beacon/%s/", upstream.Name)
		http.HandleFunc(endpoint, p.beaconProxyRequestHandler(upstream.Name))
	}

	for _, upstream := range p.cfg.ExecutionConfig.ExecutionUpstreams {
		endpoint := fmt.Sprintf("/proxy/execution/%s/", upstream.Name)
		http.HandleFunc(endpoint, p.executionProxyRequestHandler(upstream.Name))
	}

	http.HandleFunc("/lb/beacon/", p.beaconLoadBalanceHandler())
	http.HandleFunc("/lb/execution/", p.executionLoadBalanceHandler())

	http.HandleFunc("/status", p.statusRequestHandler())
	log.WithField("listenAddr", p.cfg.ListenAddr).Info("started proxy server")

	err := http.ListenAndServe(p.cfg.ListenAddr, nil)
	if err != nil {
		log.WithError(err).Fatal("can't start HTTP server")
	}

	return err
}

func (p *Proxy) beaconProxyRequestHandler(upstreamName string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, fmt.Sprintf("/proxy/beacon/%s", upstreamName), "")
		p.beaconBackends[upstreamName].ServeHTTP(w, r)
	}
}

func (p *Proxy) beaconLoadBalanceHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, "/lb/beacon", "")
		p.beaconLoadBalancer.RoundRobin(w, r)
	}
}

func (p *Proxy) executionProxyRequestHandler(upstreamName string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, fmt.Sprintf("/proxy/execution/%s", upstreamName), "")
		p.executionBackends[upstreamName].ServeHTTP(w, r)
	}
}

func (p *Proxy) executionLoadBalanceHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, "/lb/execution/", "")
		p.executionLoadBalancer.RoundRobin(w, r)
	}
}

func (p *Proxy) statusRequestHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")

		resp := struct {
			Beacon    map[string]*beaconbackend.Status    `json:"beacon_nodes"`
			Execution map[string]*executionbackend.Status `json:"execution_nodes"`
		}{
			Beacon:    make(map[string]*beaconbackend.Status),
			Execution: make(map[string]*executionbackend.Status),
		}

		for k, v := range p.beaconBackends {
			resp.Beacon[k] = v.Status()
		}

		for k, v := range p.executionBackends {
			resp.Execution[k] = v.Status()
		}

		b, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		_, err = w.Write(b)
		if err != nil {
			log.WithError(err).Error("failed writing to status to http.ResponseWriter")
		}
	}
}
