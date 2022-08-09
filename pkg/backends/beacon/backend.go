package beacon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/skylenet/eth-proxy/pkg/matcher"
)

const defaultUpstreamTimeoutSeconds uint = 15
const defaultHealthCheckIntervalSeconds uint = 30

type Backend struct {
	mu      sync.RWMutex
	url     url.URL
	isAlive bool

	proxy       func(http.ResponseWriter, *http.Request)
	config      Config
	status      Status
	pathMatcher matcher.Matcher
}

type Config struct {
	ProxyTimeoutSeconds        uint
	HealthCheckIntervalSeconds uint
	APIAllowPaths              []string
}

type Status struct {
	Version   string        `json:"version"`
	Syncing   SyncInfo      `json:"syncing"`
	PeerCount PeerCountInfo `json:"peer_count"`
	LastCheck int64         `json:"last_check"`
}

type SyncInfo struct {
	HeadSlot     string `json:"head_slot"`
	SyncDistance string `json:"sync_distance"`
	IsSyncing    bool   `json:"is_syncing"`
	IsOptimistic bool   `json:"is_optimistic"`
}

type PeerCountInfo struct {
	Disconnected int `json:"disconnected"`
	Connected    int `json:"connected"`
}

func NewBackend(url url.URL, cfg Config) *Backend {
	if cfg.HealthCheckIntervalSeconds == 0 {
		cfg.HealthCheckIntervalSeconds = defaultHealthCheckIntervalSeconds
	}
	if cfg.ProxyTimeoutSeconds == 0 {
		cfg.ProxyTimeoutSeconds = defaultUpstreamTimeoutSeconds
	}

	b := Backend{
		url:     url,
		isAlive: false,
		config:  cfg,
	}

	b.proxy = b.proxyHandler()
	b.pathMatcher = matcher.NewAllowMatcher(cfg.APIAllowPaths)

	ticker := time.NewTicker(time.Duration(b.config.HealthCheckIntervalSeconds) * time.Second)
	done := make(chan bool)

	go func() {
		b.Check()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				b.Check()
			}
		}
	}()

	return &b
}

func (b *Backend) IsAlive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.isAlive
}

func (b *Backend) Status() *Status {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return &b.status
}

func (b *Backend) URL() url.URL {
	return b.url
}

func (b *Backend) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	b.proxy(rw, req)
}

func (b *Backend) Check() {
	b.mu.Lock()
	defer b.mu.Unlock()

	addr := b.url.String()
	log.WithField("node", addr).Debug("checking beacon node")

	version, err := b.checkNodeVersion()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting beacon node version")
	}

	syncInfo, err := b.checkNodeSyncing()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting beacon node sync info")
	}

	peerCountInfo, err := b.checkNodePeerCount()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting beacon node peer count info")
	}

	now := time.Now().Unix()
	s := Status{
		Version:   version,
		LastCheck: now,
	}

	if syncInfo != nil {
		s.Syncing = *syncInfo
	}

	if peerCountInfo != nil {
		s.PeerCount = *peerCountInfo
	}

	b.isAlive = !s.Syncing.IsSyncing && s.PeerCount.Connected >= 1
	b.status = s
}

func (b *Backend) checkNodeVersion() (string, error) {
	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get(fmt.Sprintf("%s/eth/v1/node/version", b.url.String()))

	if err != nil {
		return "", err
	}

	r := struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}{}

	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return "", errors.Wrap(err, "failed decoding response body")
	}

	return r.Data.Version, nil
}

func (b *Backend) checkNodeSyncing() (*SyncInfo, error) {
	client := http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(fmt.Sprintf("%s/eth/v1/node/syncing", b.url.String()))
	if err != nil {
		return nil, err
	}

	r := struct {
		Data struct {
			SyncInfo
		} `json:"data"`
	}{}

	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return nil, errors.Wrap(err, "failed decoding response body")
	}

	return &r.Data.SyncInfo, nil
}

func (b *Backend) checkNodePeerCount() (*PeerCountInfo, error) {
	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get(fmt.Sprintf("%s/eth/v1/node/peer_count", b.url.String()))

	if err != nil {
		return nil, err
	}

	// Some clients return the fields as int, others as strings, so we have to deal with both cases
	r := struct {
		Data struct {
			Connected    interface{} `json:"connected"`
			Disconnected interface{} `json:"disconnected"`
		} `json:"data"`
	}{}

	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return nil, errors.Wrap(err, "failed decoding response body")
	}

	ret := PeerCountInfo{}

	switch v := r.Data.Connected.(type) {
	case float64:
		ret.Connected = int(v)
	case string:
		m, _ := strconv.Atoi(v)
		ret.Connected = m
	}

	switch v := r.Data.Disconnected.(type) {
	case float64:
		ret.Disconnected = int(v)
	case string:
		m, _ := strconv.Atoi(v)
		ret.Disconnected = m
	}

	return &ret, nil
}

func (b *Backend) proxyHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only allow GET methods for now
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)

			_, err := fmt.Fprintf(w, "METHOD NOT ALLOWED\n")
			if err != nil {
				log.WithError(err).Error("failed writing to http.ResponseWriter")
			}

			return
		}
		// Check if path is allowed
		if !b.pathMatcher.Matches(r.URL.Path) {
			w.WriteHeader(http.StatusForbidden)

			_, err := fmt.Fprintf(w, "FORBIDDEN. Path is not allowed\n")
			if err != nil {
				log.WithError(err).Error("failed writing to http.ResponseWriter")
			}

			return
		}
		// Pass request to backend url
		rp := httputil.NewSingleHostReverseProxy(&b.url)
		dialer := &net.Dialer{
			Timeout: time.Duration(b.config.ProxyTimeoutSeconds) * time.Second,
		}
		rp.Transport = &http.Transport{
			Dial: dialer.Dial,
		}
		rp.ServeHTTP(w, r)
	}
}
