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

	with, err := json.Marshal(c.newPlayerRequest(makeProfile(profileWeb), sess, playerRequestOpts{VideoID: "vid123", POToken: "PLAYER-TOK"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"serviceIntegrityDimensions":{"poToken":"PLAYER-TOK"}`) {
		t.Errorf("player request body missing poToken: %s", with)
	}

	without, err := json.Marshal(c.newPlayerRequest(makeProfile(profileWeb), sess, playerRequestOpts{VideoID: "vid123"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "serviceIntegrityDimensions") {
		t.Errorf("empty token must omit serviceIntegrityDimensions: %s", without)
	}
}

func TestNewPlayerRequest_SignatureTimestamp(t *testing.T) {
	c := New(Config{})
	sess := newSession("US")

	with, err := json.Marshal(c.newPlayerRequest(makeProfile(profileWeb), sess, playerRequestOpts{VideoID: "vid123", STS: 19834}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"signatureTimestamp":19834`) {
		t.Errorf("player body missing signatureTimestamp: %s", with)
	}

	without, err := json.Marshal(c.newPlayerRequest(makeProfile(profileWeb), sess, playerRequestOpts{VideoID: "vid123"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "signatureTimestamp") {
		t.Errorf("zero signature timestamp must be omitted from the body: %s", without)
	}
}
