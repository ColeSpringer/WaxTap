package main

import (
	"net"
	"strings"
	"testing"
)

// A proxy that accepts and never answers CONNECT leaves net/http with a bare
// context deadline, which named nothing; a proxy that never accepts is named
// by the dialer only when its timeout comes before the budget's. Both must
// name the proxy, whatever the budget.
func TestDeadProxyIsNamedUnderAShortBudget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // hold it open, answer nothing
		}
	}()
	t.Cleanup(func() {
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})

	t.Setenv("WAXTAP_EXTRACTION_TIMEOUT", "1")
	stdout, _, code := runMain(t, "info", "dummyVideo0", "--json", "--no-cache", "--proxy", "http://"+ln.Addr().String())
	doc := oneJSONDoc(t, stdout)
	if code != 9 {
		t.Fatalf("exit %d, want 9: %v", code, doc)
	}
	e, _ := doc["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if e["code"] != "network" || !strings.Contains(msg, "proxy") {
		t.Errorf("error = %v, want code network naming the proxy", e)
	}
}
