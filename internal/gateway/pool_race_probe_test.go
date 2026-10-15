package gateway

import (
	"errors"
	"sync"
	"testing"
	"time"

	"opencode2api/internal/config"
)

// Concurrency probe: pool management path (Rebind/Restore) vs request path
// (Cursor/Next/Proxy/Mark*) and admin readers (bindingCount under lock,
// keyStatuses) running concurrently. Adapted from the 2api-gateway-review
// race-probe template for opencode2api's nodePool/anonymousPool shapes.
func TestZZRaceProbePoolMutationsVsReaders(t *testing.T) {
	transports, err := newTransportPool([]string{"direct", "direct", "direct"},
		config.PerformanceConfig{MaxIdleConns: 8, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 8, IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 5}, time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	pool, err := newNodePool([]string{"k1", "k2", "k3", "k4"}, transports, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("node pool: %v", err)
	}
	anon := newAnonymousPool(true, transports, 50*time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { // management-path mutations
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			pool.RebindProxy(i % len(transports.items))
			pool.RestoreProxy((i + 1) % len(transports.items))
			anon.RestoreProxy(transports.items[i%len(transports.items)])
		}
	}()
	go func() { // request path: failover cursor
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			cursor := pool.CursorFor("session-1")
			node := cursor.Next()
			if node == nil {
				continue
			}
			_ = pool.Proxy(node)
			pool.MarkFailure(node, nil, errors.New("probe failure"))
			pool.MarkSuccess(node)
			anonCursor := anon.CursorFor("session-1")
			if anonNode := anonCursor.Next(); anonNode != nil {
				anon.MarkFailure(anonNode, nil, errors.New("probe failure"))
				anon.MarkSuccess(anonNode)
			}
		}
	}()
	go func() { // admin readers: binding counts + key statuses (Resources path)
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			pool.bindingsMu.Lock()
			_ = append([]int(nil), pool.bindingCount...)
			pool.bindingsMu.Unlock()
			_ = keyStatuses("zen", pool)
			_ = pool.Len()
			_ = pool.NodeByID(config.Fingerprint("k2"))
		}
	}()
	go func() { // anonymous request path
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			cursor := anon.CursorFor("")
			_ = cursor.Next()
			_ = anon.Len()
		}
	}()
	wg.Wait()
}
