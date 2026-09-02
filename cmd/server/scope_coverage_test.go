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

// scopeCoverageSchema creates the subset of the transmissions/observations
// schema GetScopedRelayHops queries against.
func scopeCoverageSchema(t *testing.T, conn *sql.DB) {
	t.Helper()
	if _, err := conn.Exec(`CREATE TABLE transmissions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		payload_type INTEGER, scope_name TEXT
	)`); err != nil {
		t.Fatalf("create transmissions: %v", err)
	}
	if _, err := conn.Exec(`CREATE TABLE observations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		transmission_id INTEGER, resolved_path TEXT
	)`); err != nil {
		t.Fatalf("create observations: %v", err)
	}
}

func TestGetScopedRelayHops(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}
	db.hasScopeName = true
	db.hasResolvedPath = true
	scopeCoverageSchema(t, conn)

	insertTx := func(id, payloadType int, scope string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO transmissions (id, payload_type, scope_name) VALUES (?, ?, ?)`, id, payloadType, scope); err != nil {
			t.Fatalf("insert tx %d: %v", id, err)
		}
	}
	insertObs := func(txID int, resolvedPath string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO observations (transmission_id, resolved_path) VALUES (?, ?)`, txID, resolvedPath); err != nil {
			t.Fatalf("insert obs for tx %d: %v", txID, err)
		}
	}

	insertTx(1, 1, "#eu")
	insertObs(1, `["pk-1","pk-2"]`)
	insertTx(2, 1, "#eu")
	insertObs(2, `["pk-2","pk-3"]`) // pk-2 repeated across transmissions: must dedup
	insertTx(3, 1, "#usa")
	insertObs(3, `["pk-4"]`)
	insertTx(4, payloadTypeAdvert, "#japan") // ADVERT-only: must be excluded entirely
	insertObs(4, `["pk-5"]`)
	insertTx(5, 1, "") // empty scope: must be excluded
	insertObs(5, `["pk-6"]`)
	insertTx(6, 1, "#empty-path")
	insertObs(6, ``) // NULL/empty resolved_path (unresolved hop-only path): contributes nothing

	got, err := db.GetScopedRelayHops()
	if err != nil {
		t.Fatalf("GetScopedRelayHops: %v", err)
	}
	if want := map[string]bool{"pk-1": true, "pk-2": true, "pk-3": true}; !mapSetEqual(got["#eu"], want) {
		t.Errorf("#eu = %v, want %v", got["#eu"], want)
	}
	if want := map[string]bool{"pk-4": true}; !mapSetEqual(got["#usa"], want) {
		t.Errorf("#usa = %v, want %v", got["#usa"], want)
	}
	if _, ok := got["#japan"]; ok {
		t.Errorf("#japan should be absent (ADVERT-only evidence): %v", got["#japan"])
	}
	if _, ok := got[""]; ok {
		t.Error("empty scope_name should never produce a map entry")
	}
	if _, ok := got["#empty-path"]; ok {
		t.Error("#empty-path should be absent (no resolved_path rows contributed)")
	}
}

func TestGetScopedRelayHopsMissingSchema(t *testing.T) {
	// Schemas predating scope_name/resolved_path: (nil, nil), not an error.
	db := &DB{}
	got, err := db.GetScopedRelayHops()
	if err != nil {
		t.Fatalf("GetScopedRelayHops: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func mapSetEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func TestHandleScopeCoverage(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}
	db.hasScopeName = true
	db.hasResolvedPath = true

	if _, err := conn.Exec(`CREATE TABLE nodes (
		public_key TEXT PRIMARY KEY, name TEXT,
		lat REAL, lon REAL, foreign_advert INTEGER
	)`); err != nil {
		t.Fatalf("create nodes: %v", err)
	}
	scopeCoverageSchema(t, conn)

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

	insertTx := func(id, payloadType int, scope string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO transmissions (id, payload_type, scope_name) VALUES (?, ?, ?)`, id, payloadType, scope); err != nil {
			t.Fatalf("insert tx %d: %v", id, err)
		}
	}
	insertObs := func(txID int, resolvedPath string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO observations (transmission_id, resolved_path) VALUES (?, ?)`, txID, resolvedPath); err != nil {
			t.Fatalf("insert obs for tx %d: %v", txID, err)
		}
	}

	// pk-1, pk-2, pk-3 relayed non-ADVERT traffic scoped #eu.
	insertTx(1, 1, "#eu")
	insertObs(1, `["pk-1","pk-2","pk-3"]`)
	// pk-4 relayed non-ADVERT traffic scoped #usa.
	insertTx(2, 1, "#usa")
	insertObs(2, `["pk-4"]`)
	// pk-blacklisted relayed #eu traffic too, but must be filtered by blacklist.
	insertTx(3, 1, "#eu")
	insertObs(3, `["pk-blacklisted"]`)
	// pk-advert-only's only evidence is its own ADVERT (payload_type 4) —
	// must NOT count as relay evidence (self-declaration, not relaying).
	insertTx(4, payloadTypeAdvert, "#japan")
	insertObs(4, `["pk-advert-only"]`)

	cfg := &Config{}
	cfg.SetNodeBlacklist([]string{"pk-blacklisted"})
	srv := &Server{db: db, cfg: cfg}

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
	// ADVERT, which is excluded at the SQL layer.
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

func TestHandleScopeCoverageMissingSchema(t *testing.T) {
	// Schema predates scope_name/resolved_path — must degrade to an empty
	// response, not error or panic.
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn}
	srv := &Server{db: db, cfg: &Config{}}

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

func TestHandleScopeCoverageNodes(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}
	db.hasScopeName = true
	db.hasResolvedPath = true

	if _, err := conn.Exec(`CREATE TABLE nodes (
		public_key TEXT PRIMARY KEY, name TEXT,
		lat REAL, lon REAL, foreign_advert INTEGER
	)`); err != nil {
		t.Fatalf("create nodes: %v", err)
	}
	scopeCoverageSchema(t, conn)

	nodes := []struct {
		pk, name string
		lat, lon float64
	}{
		{"pk-boundary", "Boundary", 0, 0},   // relays for BOTH #eu and #usa
		{"pk-eu-only", "EuOnly", 0, 10},     // relays for #eu only
		{"pk-blacklisted", "Blacklisted", 5, 5},
		{"pk-advert-only", "AdvertOnly", 20, 20}, // own ADVERT only — must NOT count
	}
	for _, n := range nodes {
		if _, err := conn.Exec(
			`INSERT INTO nodes (public_key, name, lat, lon, foreign_advert) VALUES (?, ?, ?, ?, 0)`,
			n.pk, n.name, n.lat, n.lon,
		); err != nil {
			t.Fatalf("insert %s: %v", n.pk, err)
		}
	}

	insertTx := func(id, payloadType int, scope string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO transmissions (id, payload_type, scope_name) VALUES (?, ?, ?)`, id, payloadType, scope); err != nil {
			t.Fatalf("insert tx %d: %v", id, err)
		}
	}
	insertObs := func(txID int, resolvedPath string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO observations (transmission_id, resolved_path) VALUES (?, ?)`, txID, resolvedPath); err != nil {
			t.Fatalf("insert obs for tx %d: %v", txID, err)
		}
	}

	// pk-boundary relays both #eu and #usa traffic — must appear in both regions' lists.
	insertTx(1, 1, "#eu")
	insertObs(1, `["pk-boundary","pk-eu-only"]`)
	insertTx(2, 1, "#usa")
	insertObs(2, `["pk-boundary"]`)
	// pk-blacklisted relayed #eu too, but must be filtered by blacklist.
	insertTx(3, 1, "#eu")
	insertObs(3, `["pk-blacklisted"]`)
	// pk-advert-only's only evidence is its own ADVERT — must NOT count.
	insertTx(4, payloadTypeAdvert, "#japan")
	insertObs(4, `["pk-advert-only"]`)

	cfg := &Config{}
	cfg.SetNodeBlacklist([]string{"pk-blacklisted"})
	srv := &Server{db: db, cfg: cfg}

	req := httptest.NewRequest(http.MethodGet, "/api/scope-coverage/nodes", nil)
	w := httptest.NewRecorder()
	srv.handleScopeCoverageNodes(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp ScopeCoverageNodesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (pk-boundary, pk-eu-only — not blacklisted/advert-only): %+v", len(resp.Nodes), resp.Nodes)
	}

	byPubkey := make(map[string]ScopeCoverageNode, len(resp.Nodes))
	for _, n := range resp.Nodes {
		byPubkey[n.PubKey] = n
	}
	boundary, ok := byPubkey["pk-boundary"]
	if !ok {
		t.Fatalf("pk-boundary missing from response: %+v", resp.Nodes)
	}
	if !reflect.DeepEqual(boundary.Regions, []string{"#eu", "#usa"}) {
		t.Errorf("pk-boundary Regions = %v, want [#eu #usa] (multi-region membership)", boundary.Regions)
	}
	euOnly, ok := byPubkey["pk-eu-only"]
	if !ok {
		t.Fatalf("pk-eu-only missing from response: %+v", resp.Nodes)
	}
	if !reflect.DeepEqual(euOnly.Regions, []string{"#eu"}) {
		t.Errorf("pk-eu-only Regions = %v, want [#eu]", euOnly.Regions)
	}
}

func TestHandleScopeCoverageNodesMissingSchema(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn}
	srv := &Server{db: db, cfg: &Config{}}

	req := httptest.NewRequest(http.MethodGet, "/api/scope-coverage/nodes", nil)
	w := httptest.NewRecorder()
	srv.handleScopeCoverageNodes(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp ScopeCoverageNodesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Nodes) != 0 {
		t.Errorf("Nodes = %+v, want empty", resp.Nodes)
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
