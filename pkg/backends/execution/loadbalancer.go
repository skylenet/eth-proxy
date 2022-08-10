package execution

import (
	"net/http"
	"sync"

	log "github.com/sirupsen/logrus"
)

type LoadBalancer struct {
	mu            sync.Mutex
	roundRobinIdx uint
	backends      []*Backend
}

func NewLoadBalancer(backends []*Backend) *LoadBalancer {
	lb := LoadBalancer{
		backends: backends,
	}
	return &lb
}

func (lb *LoadBalancer) RoundRobin(rw http.ResponseWriter, req *http.Request) {
	len := len(lb.backends)
	downCount := 0
	lb.mu.Lock()
	var b *Backend
	for b = lb.backends[int(lb.roundRobinIdx)%len]; !b.isAlive; downCount++ {
		lb.roundRobinIdx++
		if downCount == len {
			log.Errorf("no execution upstreams available. %d/%d are down", downCount, len)
			rw.WriteHeader(http.StatusBadGateway)
			lb.mu.Unlock()
			return
		}

	}
	lb.roundRobinIdx++
	lb.mu.Unlock()
	addr := b.URL()
	wsAddr := b.WeboscketURL()
	log.WithField("module", "lb-exec").
		WithField("upstream", addr.String()).
		WithField("wsupstream", wsAddr.String()).
		WithField("path", req.URL.Path).
		Debug("load balance execution rpc request ")
	b.ServeHTTP(rw, req)
}
