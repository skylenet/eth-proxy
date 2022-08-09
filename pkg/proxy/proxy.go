package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	beaconbackend "github.com/skylenet/eth-proxy/pkg/backends/beacon"
	"github.com/skylenet/eth-proxy/pkg/jsonrpc"
	"github.com/skylenet/eth-proxy/pkg/matcher"
)

type Proxy struct {
	Cfg Config
	Monitors
	beaconAPIPathMatcher       matcher.Matcher
	executionRPCMethodsMatcher matcher.Matcher
	beaconBackends             map[string]*beaconbackend.Backend
}

type Monitors struct {
	ExecutionMonitor *ExecutionMonitor
}

func NewProxy(conf *Config) *Proxy {
	if err := conf.Validate(); err != nil {
		log.Fatalf("can't start proxy: %s", err)
	}

	em := NewExecutionMonitor(conf.ExecutionUpstreams)
	p := &Proxy{
		Cfg: *conf,
		Monitors: Monitors{
			ExecutionMonitor: em,
		},
	}

	p.executionRPCMethodsMatcher = matcher.NewAllowMatcher(p.Cfg.ExecutionConfig.RPCAllowMethods)

	// build up beacon backends
	p.beaconBackends = make(map[string]*beaconbackend.Backend)
	for _, b := range conf.BeaconUpstreams {
		u, err := url.Parse(b.Address)
		if err != nil {
			log.Fatalf("beacon upstream %s has an invalid url: %s (%v)", b.Name, b.Address, err)
		}
		p.beaconBackends[b.Name] = beaconbackend.NewBackend(*u, beaconbackend.Config{
			APIAllowPaths: conf.BeaconConfig.APIAllowPaths,
		})
	}

	return p
}

func (p *Proxy) Serve() error {
	upstreamProxies := make(map[string]*httputil.ReverseProxy)

	for _, upstream := range p.Cfg.BeaconConfig.BeaconUpstreams {
		endpoint := fmt.Sprintf("/proxy/beacon/%s/", upstream.Name)
		http.HandleFunc(endpoint, p.beaconProxyRequestHandler(upstream.Name))
	}

	for _, upstream := range p.Cfg.ExecutionConfig.ExecutionUpstreams {
		rp, err := newHTTPReverseProxy(upstream.Address, p.Cfg.ExecutionConfig.ProxyTimeoutSeconds)
		if err != nil {
			log.WithError(err).Fatal("can't add execution upstream server")
		}

		upstreamProxies[upstream.Name] = rp
		endpoint := fmt.Sprintf("/proxy/execution/%s/", upstream.Name)
		http.HandleFunc(endpoint, p.executionProxyRequestHandler(rp, upstream.Name))
	}

	http.HandleFunc("/status", p.statusRequestHandler())
	log.WithField("listenAddr", p.Cfg.ListenAddr).Info("started proxy server")

	err := http.ListenAndServe(p.Cfg.ListenAddr, nil)
	if err != nil {
		log.WithError(err).Fatal("can't start HTTP server")
	}

	return err
}

func newHTTPReverseProxy(targetHost string, proxyTimeoutSeconds uint) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(targetHost)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(u)
	dialer := &net.Dialer{
		Timeout: time.Duration(proxyTimeoutSeconds) * time.Second,
	}
	rp.Transport = &http.Transport{
		Dial: dialer.Dial,
	}

	return rp, nil
}

func (p *Proxy) beaconProxyRequestHandler(upstreamName string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, fmt.Sprintf("/proxy/beacon/%s", upstreamName), "")
		p.beaconBackends[upstreamName].ServeHTTP(w, r)
	}
}

func (p *Proxy) executionProxyRequestHandler(proxy *httputil.ReverseProxy, upstreamName string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, fmt.Sprintf("/proxy/execution/%s", upstreamName), "")

		// Detect if websocket connection
		if r.Header.Get("upgrade") == "websocket" {
			wsUpstream := ""

			for _, node := range p.Cfg.ExecutionConfig.ExecutionUpstreams {
				if node.Name == upstreamName {
					wsUpstream = node.WsAddress
				}
			}

			u, _ := url.Parse(wsUpstream)
			wsp := NewWebsocketProxy(u, NewRPCWebsocketMessageProcessor(p.executionRPCMethodsMatcher))
			wsp.ServeHTTP(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.WithError(err).Error("failed reading rpc body")
			w.WriteHeader(http.StatusInternalServerError)

			err = json.NewEncoder(w).Encode(jsonrpc.NewJSONRPCResponseError(json.RawMessage("1"), jsonrpc.ErrorInternal, "server error"))
			if err != nil {
				log.WithError(err).Error("failed writing to http.ResponseWriter.1")
			}

			return
		}

		method, err := parseRPCPayload(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			err = json.NewEncoder(w).Encode(jsonrpc.NewJSONRPCResponseError(json.RawMessage("1"), jsonrpc.ErrorInvalidParams, err.Error()))
			if err != nil {
				log.WithError(err).Error("failed writing to http.ResponseWriter.2")
			}

			return
		}

		r.Body = io.NopCloser(bytes.NewBuffer(body))

		if !p.executionRPCMethodsMatcher.Matches(method) {
			w.WriteHeader(http.StatusForbidden)

			err := json.NewEncoder(w).Encode(jsonrpc.NewJSONRPCResponseError(json.RawMessage("1"), jsonrpc.ErrorMethodNotFound, "method not allowed"))
			if err != nil {
				log.WithError(err).Error("failed writing to http.ResponseWriter.3")
			}

			return
		}

		proxy.ServeHTTP(w, r)
	}
}

func (p *Proxy) statusRequestHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")

		resp := struct {
			Beacon    map[string]*beaconbackend.Status `json:"beacon_nodes"`
			Execution map[string]ExecutionStatus       `json:"execution_nodes"`
		}{
			Beacon:    make(map[string]*beaconbackend.Status),
			Execution: p.Monitors.ExecutionMonitor.status,
		}

		for k, v := range p.beaconBackends {
			resp.Beacon[k] = v.Status()
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

func parseRPCPayload(body []byte) (method string, err error) {
	rpcPayload := struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}{}

	err = json.Unmarshal(body, &rpcPayload)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse json RPC payload")
	}

	return rpcPayload.Method, nil
}
