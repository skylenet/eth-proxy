package execution

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/skylenet/eth-proxy/pkg/jsonrpc"
	"github.com/skylenet/eth-proxy/pkg/matcher"
)

const defaultUpstreamTimeoutSeconds uint = 15
const defaultHealthCheckIntervalSeconds uint = 30

type Backend struct {
	mu      sync.RWMutex
	url     *url.URL
	wsUrl   *url.URL
	isAlive bool

	proxy             func(http.ResponseWriter, *http.Request)
	config            Config
	status            Status
	rpcMethodsMatcher matcher.Matcher
}

type Config struct {
	URL                        *url.URL
	WebsocketURL               *url.URL
	ProxyTimeoutSeconds        uint
	HealthCheckIntervalSeconds uint
	RPCAllowMethods            []string
}

type Status struct {
	HeadBlock uint64 `json:"head_block"`
	ChainID   uint64 `json:"chain_id"`
	PeerCount uint64 `json:"peer_count"`
	IsSyncing bool   `json:"is_syncing"`
	LastCheck int64  `json:"last_check"`
}

func NewBackend(cfg Config) *Backend {
	if cfg.HealthCheckIntervalSeconds == 0 {
		cfg.HealthCheckIntervalSeconds = defaultHealthCheckIntervalSeconds
	}
	if cfg.ProxyTimeoutSeconds == 0 {
		cfg.ProxyTimeoutSeconds = defaultUpstreamTimeoutSeconds
	}

	b := Backend{
		url:     cfg.URL,
		wsUrl:   cfg.WebsocketURL,
		isAlive: false,
		config:  cfg,
	}

	b.proxy = b.proxyHandler()
	b.rpcMethodsMatcher = matcher.NewAllowMatcher(cfg.RPCAllowMethods)

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
	return *b.url
}

func (b *Backend) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	b.proxy(rw, req)
}

func (b *Backend) Check() {
	b.mu.Lock()
	defer b.mu.Unlock()

	addr := b.url.String()
	log.WithField("node", addr).Debug("checking execution node")

	syncing, err := b.checkNodeSyncing()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting execution node sync info")
	}

	headBlock, err := b.checkNodeHeadBlock()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting execution node head block")
	}

	chainID, err := b.checkNodechainID()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting execution node chain id")
	}

	peerCount, err := b.checkNodePeerCount()
	if err != nil {
		log.WithField("upstream", addr).WithError(err).Error("failed getting execution node peer count")
	}

	now := time.Now().Unix()
	s := Status{
		IsSyncing: syncing,
		LastCheck: now,
		HeadBlock: headBlock,
		ChainID:   chainID,
		PeerCount: peerCount,
	}

	b.isAlive = !s.IsSyncing
	b.status = s
}

func (b *Backend) checkNodeSyncing() (bool, error) {
	client, err := rpc.Dial(b.url.String())
	if err != nil {
		return true, err
	}

	var result bool
	err = client.Call(&result, "eth_syncing")

	return result, err
}

func (b *Backend) checkNodeHeadBlock() (uint64, error) {
	client, err := rpc.Dial(b.url.String())
	if err != nil {
		return 0, err
	}

	var result string

	err = client.Call(&result, "eth_blockNumber")
	if err != nil {
		return 0, err
	}

	res, err := hexutil.DecodeUint64(result)
	if err != nil {
		return 0, err
	}

	return res, err
}

func (b *Backend) checkNodechainID() (uint64, error) {
	client, err := rpc.Dial(b.url.String())
	if err != nil {
		return 0, err
	}

	var result string

	err = client.Call(&result, "eth_chainId")
	if err != nil {
		return 0, err
	}

	res, err := hexutil.DecodeUint64(result)
	if err != nil {
		return 0, err
	}

	return res, err
}

func (b *Backend) checkNodePeerCount() (uint64, error) {
	client, err := rpc.Dial(b.url.String())
	if err != nil {
		return 0, err
	}

	var result string

	err = client.Call(&result, "net_peerCount")
	if err != nil {
		return 0, err
	}

	res, err := hexutil.DecodeUint64(result)
	if err != nil {
		return 0, err
	}

	return res, err
}

func (b *Backend) proxyHandler() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("upgrade") == "websocket" && b.wsUrl != nil {
			b.handleWebsocket(w, r)
		} else {
			b.handleHTTP(w, r)
		}
	}
}

func (b *Backend) handleHTTP(w http.ResponseWriter, r *http.Request) {
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

	if !b.rpcMethodsMatcher.Matches(method) {
		w.WriteHeader(http.StatusForbidden)

		err := json.NewEncoder(w).Encode(jsonrpc.NewJSONRPCResponseError(json.RawMessage("1"), jsonrpc.ErrorMethodNotFound, "method not allowed"))
		if err != nil {
			log.WithError(err).Error("failed writing to http.ResponseWriter.3")
		}

		return
	}

	// Pass request to backend url
	rp := httputil.NewSingleHostReverseProxy(b.url)
	dialer := &net.Dialer{
		Timeout: time.Duration(b.config.ProxyTimeoutSeconds) * time.Second,
	}
	rp.Transport = &http.Transport{
		Dial: dialer.Dial,
	}
	rp.ServeHTTP(w, r)
}

func (b *Backend) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	wsp := NewWebsocketProxy(b.wsUrl, NewRPCWebsocketMessageProcessor(b.rpcMethodsMatcher))
	wsp.ServeHTTP(w, r)
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
