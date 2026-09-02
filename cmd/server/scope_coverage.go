package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// scopeCoverageTTL bounds how often /api/scope-coverage recomputes hulls.
// Node positions/scopes change slowly enough that 30s (matching
// /api/scope-stats' cache TTL) is more than fresh enough.
const scopeCoverageTTL = 30 * time.Second

// handleScopeCoverage groups repeaters/rooms by the hash regions they've
// actually carried traffic FOR — GetScopedRelayHops' deduplicated set of
// relay-hop pubkeys seen on non-ADVERT transmissions matching each
// transmissions.scope_name — and returns the convex hull of each region's
// member node positions. An area-like approximation of "what geography
// does this hash region's traffic reach," inferred purely from where
// relaying nodes actually are.
//
// Computed directly from transmissions+observations (see db.go's
// GetScopedRelayHops), NOT from the in-memory PacketStore's
// RepeaterRelayInfo.TransportedScopes — that would only cover whatever's
// still resident in the store's bounded retention window. Querying the DB
// means this works regardless of in-memory eviction, and even when no
// store is wired at all.
//
// Deliberately NOT nodes.default_scope: that field only reflects a node's
// own self-broadcast ADVERT scope (see shouldUpdateDefaultScope in
// cmd/ingestor/main.go — it's gated to the ADVERT branch of ingest), i.e.
// "what scope does this node claim to be in," not "whose traffic has it
// actually carried." A repeater sitting on a region boundary can — and
// should — contribute to more than one region's shape; GetScopedRelayHops
// is a set for exactly this reason, default_scope is a single string.
//
// Hash regions carry no authoritative shape of their own
// (internal/admindb's hash_regions table is just names hashed into HMAC
// keys), so this can only ever be an inferred coverage area, not a
// boundary. Public/unauthenticated: the same underlying data (node
// lat/lon + transported_scopes) is already served unfiltered by
// /api/nodes for repeaters/rooms.
func (s *Server) handleScopeCoverage(w http.ResponseWriter, r *http.Request) {
	s.scopeCoverageMu.Lock()
	if s.scopeCoverageCache != nil && time.Since(s.scopeCoverageCachedAt) < scopeCoverageTTL {
		cached := s.scopeCoverageCache
		s.scopeCoverageMu.Unlock()
		writeJSON(w, cached)
		return
	}
	s.scopeCoverageMu.Unlock()

	hopScopes, err := s.db.GetScopedRelayHops()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// No relay evidence at all (schema predates scope_name, or nothing's
	// matched a hash region yet) — skip the nodes query entirely rather
	// than scanning every node's position for nothing.
	byRegion := make(map[string][][2]float64)
	if len(hopScopes) > 0 {
		nodes, err := s.db.GetNodesWithPosition()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		posByPubkey := make(map[string]NodeWithPosition, len(nodes))
		for _, n := range nodes {
			posByPubkey[strings.ToLower(n.PubKey)] = n
		}

		for scope, pubkeys := range hopScopes {
			for pk := range pubkeys {
				pos, ok := posByPubkey[pk]
				if !ok {
					continue
				}
				if s.cfg.IsBlacklisted(pos.PubKey) || s.cfg.IsNameHidden(pos.Name) {
					continue
				}
				byRegion[scope] = append(byRegion[scope], [2]float64{pos.Lat, pos.Lon})
			}
		}
	}

	resp := &ScopeCoverageResponse{Regions: []ScopeCoverageRegion{}}
	for name, pts := range byRegion {
		hull := convexHull(pts)
		hullPoints := make([]ScopeCoveragePoint, len(hull))
		for i, p := range hull {
			hullPoints[i] = ScopeCoveragePoint(p)
		}
		resp.Regions = append(resp.Regions, ScopeCoverageRegion{
			Name:      name,
			NodeCount: len(pts),
			Hull:      hullPoints,
		})
	}
	sort.Slice(resp.Regions, func(i, j int) bool { return resp.Regions[i].Name < resp.Regions[j].Name })

	s.scopeCoverageMu.Lock()
	s.scopeCoverageCache = resp
	s.scopeCoverageCachedAt = time.Now()
	s.scopeCoverageMu.Unlock()

	writeJSON(w, resp)
}

// convexHull computes the convex hull of pts using Andrew's monotone
// chain algorithm, O(n log n). Treats each point as a generic 2D
// coordinate — works identically regardless of axis order (pts here are
// [lat, lon], matching the geo_filter polygon convention elsewhere in
// this codebase; the algorithm doesn't care which axis is "first").
// Deduplicates identical points first. If fewer than 3 unique points
// remain, returns them as-is — not a real polygon; the caller/frontend
// decides how to render 0, 1, or 2 points (omit, marker, line).
func convexHull(pts [][2]float64) [][2]float64 {
	seen := make(map[[2]float64]bool, len(pts))
	uniq := make([][2]float64, 0, len(pts))
	for _, p := range pts {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	if len(uniq) < 3 {
		return uniq
	}

	sort.Slice(uniq, func(i, j int) bool {
		if uniq[i][0] != uniq[j][0] {
			return uniq[i][0] < uniq[j][0]
		}
		return uniq[i][1] < uniq[j][1]
	})

	cross := func(o, a, b [2]float64) float64 {
		return (a[0]-o[0])*(b[1]-o[1]) - (a[1]-o[1])*(b[0]-o[0])
	}

	n := len(uniq)
	hull := make([][2]float64, 0, 2*n)

	// Lower chain.
	for _, p := range uniq {
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	// Upper chain.
	lower := len(hull) + 1
	for i := n - 2; i >= 0; i-- {
		p := uniq[i]
		for len(hull) >= lower && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	// Last point of each chain duplicates the first point of the other.
	return hull[:len(hull)-1]
}
