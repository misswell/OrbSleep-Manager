package gateway

// Snapshot returns a copy of the current route table for diagnostics.
func (g *Gateway) Snapshot() []Route {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Route, 0, len(g.routes))
	for _, rt := range g.routes {
		rt.mu.RLock()
		out = append(out, rt.Route)
		rt.mu.RUnlock()
	}
	return out
}
