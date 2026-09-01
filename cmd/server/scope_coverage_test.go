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

func TestGetNodesWithScope(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}
	db.hasDefaultScope = true

	if _, err := conn.Exec(`CREATE TABLE nodes (
		public_key TEXT PRIMARY KEY, name TEXT, default_scope TEXT,
		lat REAL, lon REAL, foreign_advert INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	insert := func(pk, name string, scope, lat, lon, foreign interface{}) {
		t.Helper()
		if _, err := conn.Exec(
			`INSERT INTO nodes (public_key, name, default_scope, lat, lon, foreign_advert) VALUES (?, ?, ?, ?, ?, ?)`,
			pk, name, scope, lat, lon, foreign,
		); err != nil {
			t.Fatalf("insert %s: %v", pk, err)
		}
	}
	insert("pk-good", "Good Node", "#eu", 37.4, -122.0, 0)
	insert("pk-noscope", "No Scope", "", 37.4, -122.0, 0)      // empty scope: excluded
	insert("pk-nullscope", "Null Scope", nil, 37.4, -122.0, 0) // NULL scope: excluded
	insert("pk-nogeo", "No GPS", "#eu", nil, nil, 0)           // no GPS: excluded
	insert("pk-foreign", "Foreign", "#eu", 1.0, 1.0, 1)        // foreign_advert=1: excluded

	got, err := db.GetNodesWithScope()
	if err != nil {
		t.Fatalf("GetNodesWithScope: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1: %+v", len(got), got)
	}
	if got[0].PubKey != "pk-good" || got[0].Scope != "#eu" || got[0].Lat != 37.4 || got[0].Lon != -122.0 {
		t.Errorf("unexpected result: %+v", got[0])
	}
}

func TestHandleScopeCoverage(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db := &DB{conn: conn}
	db.hasDefaultScope = true

	if _, err := conn.Exec(`CREATE TABLE nodes (
		public_key TEXT PRIMARY KEY, name TEXT, default_scope TEXT,
		lat REAL, lon REAL, foreign_advert INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	nodes := []struct {
		pk, name, scope string
		lat, lon        float64
	}{
		{"pk-1", "N1", "#eu", 0, 0},
		{"pk-2", "N2", "#eu", 0, 10},
		{"pk-3", "N3", "#eu", 10, 0},
		{"pk-4", "N4", "#usa", 40, -100},
		{"pk-blacklisted", "Blacklisted", "#eu", 5, 5},
	}
	for _, n := range nodes {
		if _, err := conn.Exec(
			`INSERT INTO nodes (public_key, name, default_scope, lat, lon, foreign_advert) VALUES (?, ?, ?, ?, ?, 0)`,
			n.pk, n.name, n.scope, n.lat, n.lon,
		); err != nil {
			t.Fatalf("insert %s: %v", n.pk, err)
		}
	}

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
	if len(resp.Regions) != 2 {
		t.Fatalf("got %d regions, want 2: %+v", len(resp.Regions), resp.Regions)
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

	// Blacklisted node must not count toward #eu.
	if eu.NodeCount != 3 {
		t.Errorf("#eu NodeCount = %d, want 3 (blacklisted node excluded)", eu.NodeCount)
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
