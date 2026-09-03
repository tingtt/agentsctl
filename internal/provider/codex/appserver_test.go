package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
)

func TestRPCIgnoresNotificationsAndCorrelatesMachineResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	rpc := &rpcClient{enc: json.NewEncoder(client), scan: bufio.NewScanner(client)}
	go func() {
		var req map[string]any
		_ = json.NewDecoder(server).Decode(&req)
		_, _ = server.Write([]byte("{\"method\":\"thread/updated\"}\n"))
		_, _ = server.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"data\":[]}}\n"))
	}()
	var out struct {
		Data []Thread `json:"data"`
	}
	if err := rpc.call(context.Background(), "thread/list", map[string]any{}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestRPCRejectsMalformedResponseStream(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	rpc := &rpcClient{enc: json.NewEncoder(client), scan: bufio.NewScanner(client)}
	go func() {
		var req map[string]any
		_ = json.NewDecoder(server).Decode(&req)
		_, _ = server.Write([]byte("not-json\n"))
		_ = server.Close()
	}()
	if err := rpc.call(context.Background(), "thread/list", map[string]any{}, nil); err == nil {
		t.Fatal("malformed response stream accepted")
	}
}
