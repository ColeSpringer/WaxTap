package youtube

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNewPlayerRequest_POTokenBody checks the player-token JSON shape. An empty
// token must omit serviceIntegrityDimensions entirely.
func TestNewPlayerRequest_POTokenBody(t *testing.T) {
	c := New(Config{})
	sess := newSession("US")

	with, err := json.Marshal(c.newPlayerRequest(makeProfile(profileWeb), sess, "vid123", "PLAYER-TOK"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"serviceIntegrityDimensions":{"poToken":"PLAYER-TOK"}`) {
		t.Errorf("player request body missing poToken: %s", with)
	}

	without, err := json.Marshal(c.newPlayerRequest(makeProfile(profileWeb), sess, "vid123", ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "serviceIntegrityDimensions") {
		t.Errorf("empty token must omit serviceIntegrityDimensions: %s", without)
	}
}
