package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestCallFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "remote rejection", response: `{"id":1,"ok":false,"error":"schema changed"}` + "\n", want: "schema changed"},
		{name: "missing result", response: `{"id":1,"ok":true}` + "\n", want: "no result"},
		{name: "wrong identity", response: `{"id":2,"ok":true,"result":{}}` + "\n", want: "ID mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			go func() {
				defer server.Close()
				_ = json.NewDecoder(server).Decode(&request{})
				_, _ = server.Write([]byte(test.response))
			}()
			_, err := call(client, request{ID: 1, Method: "ping"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("call() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSameIDsIgnoresOrderButRejectsChanges(t *testing.T) {
	left := []conversation{{ID: "a"}, {ID: "b"}}
	if !sameIDs(left, []conversation{{ID: "b"}, {ID: "a"}}) {
		t.Fatal("sameIDs() rejected the same identities in a different order")
	}
	if sameIDs(left, []conversation{{ID: "a"}, {ID: "c"}}) {
		t.Fatal("sameIDs() accepted a changed identity")
	}
}
