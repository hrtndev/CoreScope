package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func TestConvexHull(t *testing.T) {
	tests := []struct {
		name string
		pts  [][2]float64
		want [][2]float64
	}{
		{"empty", nil, [][2]float64{}},
		{"single point", [][2]float64{{1, 1}}, [][2]float64{{1, 1}}},
		{"two points", [][2]float64{{0, 0}, {1, 1}}, [][2]float64{{0, 0}, {1, 1}}},
		{
			"duplicate points collapse below 3",
			[][2]float64{{0, 0}, {0, 0}, {1, 1}},
			[][2]float64{{0, 0}, {1, 1}},
		},
		{
			"collinear points collapse to endpoints",
			[][2]float64{{0, 0}, {1, 1}, {2, 2}, {3, 3}},
			[][2]float64{{0, 0}, {3, 3}},
		},
		{
			"square with interior point excludes the interior point",
			[][2]float64{{0, 0}, {0, 10}, {10, 10}, {10, 0}, {5, 5}},
			[][2]float64{{0, 0}, {10, 0}, {10, 10}, {0, 10}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convexHull(tt.pts)
			if !hullPointSetsEqual(got, tt.want) {
				t.Errorf("convexHull(%v) = %v, want point set %v", tt.pts, got, tt.want)
			}
		})
	}
}

// hullPointSetsEqual compares hulls as sets — convexHull's starting
// vertex/winding direction is an implementation detail, only membership
// (and count, to catch accidental duplicates) matters here.
func hullPointSetsEqual(a, b [][2]float64) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, pa := range a {
		found := false
		for i, pb := range b {
			if !used[i] && pa == pb {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestGetNodesWithPosition(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}

	if _, err := conn.Exec(`CREATE TABLE nodes (
		public_key TEXT PRIMARY KEY, name TEXT,
		lat REAL, lon REAL, foreign_advert INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	insert := func(pk, name string, lat, lon, foreign interface{}) {
		t.Helper()
		if _, err := conn.Exec(
			`INSERT INTO nodes (public_key, name, lat, lon, foreign_advert) VALUES (?, ?, ?, ?, ?)`,
			pk, name, lat, lon, foreign,
		); err != nil {
			t.Fatalf("insert %s: %v", pk, err)
		}
	}
	insert("pk-good", "Good Node", 37.4, -122.0, 0)
	insert("pk-nogeo", "No GPS", nil, nil, 0)    // no GPS: excluded
	insert("pk-foreign", "Foreign", 1.0, 1.0, 1) // foreign_advert=1: excluded

	got, err := db.GetNodesWithPosition()
	if err != nil {
		t.Fatalf("GetNodesWithPosition: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1: %+v", len(got), got)
	}
	if got[0].PubKey != "pk-good" || got[0].Lat != 37.4 || got[0].Lon != -122.0 {
		t.Errorf("unexpected result: %+v", got[0])
	}
}

// storeTx is a small helper for building a *StoreTx with the fields
// handleScopeCoverage's relay-info path actually reads.
func storeTx(id int, payloadType int, scopeName string) *StoreTx {
	pt := payloadType
	return &StoreTx{ID: id, FirstSeen: "2026-01-01T00:00:00Z", PayloadType: &pt, ScopeName: scopeName}
}

func TestHandleScopeCoverage(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}

	if _, err := conn.Exec(`CREATE TABLE nodes (
		public_key TEXT PRIMARY KEY, name TEXT,
		lat REAL, lon REAL, foreign_advert INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	nodes := []struct {
		pk, name string
		lat, lon float64
	}{
		{"pk-1", "N1", 0, 0},
		{"pk-2", "N2", 0, 10},
		{"pk-3", "N3", 10, 0},
		{"pk-4", "N4", 40, -100},
		{"pk-blacklisted", "Blacklisted", 5, 5},
		{"pk-advert-only", "AdvertOnly", 20, 20}, // own ADVERT carries a scope, but never relays — must NOT count
	}
	for _, n := range nodes {
		if _, err := conn.Exec(
			`INSERT INTO nodes (public_key, name, lat, lon, foreign_advert) VALUES (?, ?, ?, ?, 0)`,
			n.pk, n.name, n.lat, n.lon,
		); err != nil {
			t.Fatalf("insert %s: %v", n.pk, err)
		}
	}

	const payloadTypeAdvertConst = 4 // must match payloadTypeAdvert in repeater_liveness.go
	store := &PacketStore{
		byPathHop: map[string][]*StoreTx{
			// pk-1, pk-2, pk-3 relayed non-ADVERT traffic scoped #eu.
			"pk-1": {storeTx(1, 1, "#eu")},
			"pk-2": {storeTx(2, 1, "#eu")},
			"pk-3": {storeTx(3, 1, "#eu")},
			// pk-4 relayed non-ADVERT traffic scoped #usa.
			"pk-4": {storeTx(4, 1, "#usa")},
			// pk-blacklisted relayed #eu traffic too, but must be filtered by blacklist.
			"pk-blacklisted": {storeTx(5, 1, "#eu")},
			// pk-advert-only's only path-hop entry is its own ADVERT (payloadType 4) —
			// TransportedScopes must NOT count adverts (self-declaration, not relay evidence).
			"pk-advert-only": {storeTx(6, payloadTypeAdvertConst, "#japan")},
		},
	}

	cfg := &Config{}
	cfg.SetNodeBlacklist([]string{"pk-blacklisted"})
	srv := &Server{db: db, cfg: cfg, store: store}

	req := httptest.NewRequest(http.MethodGet, "/api/scope-coverage", nil)
	w := httptest.NewRecorder()
	srv.handleScopeCoverage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp ScopeCoverageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// #japan must be absent: pk-advert-only's only evidence was its own
	// ADVERT, which TransportedScopes deliberately excludes.
	if len(resp.Regions) != 2 {
		t.Fatalf("got %d regions, want 2 (#eu, #usa — not #japan): %+v", len(resp.Regions), resp.Regions)
	}

	var eu, usa *ScopeCoverageRegion
	for i := range resp.Regions {
		switch resp.Regions[i].Name {
		case "#eu":
			eu = &resp.Regions[i]
		case "#usa":
			usa = &resp.Regions[i]
		}
	}
	if eu == nil || usa == nil {
		t.Fatalf("missing expected regions: %+v", resp.Regions)
	}

	// Blacklisted node must not count toward #eu, even though it relayed #eu traffic.
	if eu.NodeCount != 3 {
		t.Errorf("#eu NodeCount = %d, want 3 (blacklisted relay excluded)", eu.NodeCount)
	}
	wantHull := []ScopeCoveragePoint{{0, 0}, {0, 10}, {10, 0}}
	if !reflect.DeepEqual(sortedPoints(eu.Hull), sortedPoints(wantHull)) {
		t.Errorf("#eu Hull = %v, want point set %v", eu.Hull, wantHull)
	}

	// A single-node region's hull is just that one point.
	if usa.NodeCount != 1 {
		t.Errorf("#usa NodeCount = %d, want 1", usa.NodeCount)
	}
	if len(usa.Hull) != 1 || usa.Hull[0] != (ScopeCoveragePoint{40, -100}) {
		t.Errorf("#usa Hull = %v, want [{40 -100}]", usa.Hull)
	}
}

func TestHandleScopeCoverageNoStore(t *testing.T) {
	// No store wired (e.g. API-only/degraded mode) — must degrade to an
	// empty response, not error or panic (mirrors enrichNodeList's guard).
	srv := &Server{db: &DB{}, cfg: &Config{}, store: nil}

	req := httptest.NewRequest(http.MethodGet, "/api/scope-coverage", nil)
	w := httptest.NewRecorder()
	srv.handleScopeCoverage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp ScopeCoverageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Regions) != 0 {
		t.Errorf("Regions = %+v, want empty", resp.Regions)
	}
}

func sortedPoints(pts []ScopeCoveragePoint) []ScopeCoveragePoint {
	out := make([]ScopeCoveragePoint, len(pts))
	copy(out, pts)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j][0] < out[j-1][0] || (out[j][0] == out[j-1][0] && out[j][1] < out[j-1][1])); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
