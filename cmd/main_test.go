package main

import "testing"

func TestParseWatchNamespaces(t *testing.T) {
	got := parseWatchNamespaces(" team-a,team-b, team-a ,,")
	if len(got) != 2 {
		t.Fatalf("got %d namespaces, want 2", len(got))
	}
	if _, ok := got["team-a"]; !ok {
		t.Error("team-a missing")
	}
	if _, ok := got["team-b"]; !ok {
		t.Error("team-b missing")
	}
}
