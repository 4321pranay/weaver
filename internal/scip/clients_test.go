package scip

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"weaver/internal/extract"
)

// SCIP decides what a call is: a client API's method, generated gRPC code (its
// full method name read from the generated file), never a look-alike name.
func TestClientCalls(t *testing.T) {
	td := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(td, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(repo, path, sym string, role, line int) {
		if _, err := db.Exec("INSERT INTO occurrences VALUES (?,?,?,?,?,0,?,5,NULL,NULL)", repo, path, sym, role, line, line); err != nil {
			t.Fatal(err)
		}
	}
	// generated gRPC code in shop's repo, as protoc-gen-go-grpc writes it
	gen := "package v1\n\nconst (\n\tCart_GetCart_FullMethodName = \"/cart.v1.Cart/GetCart\"\n)\n\n" +
		"func (c *cartClient) GetCart(ctx context.Context, in *Req, opts ...grpc.CallOption) (*Resp, error) {\n" +
		"\terr := c.cc.Invoke(ctx, Cart_GetCart_FullMethodName, in, out, opts...)\n}\n\n" +
		"func RegisterCartServer(s grpc.ServiceRegistrar, srv CartServer) {\n\ts.RegisterService(&Cart_ServiceDesc, srv)\n}\n\n" +
		"var Cart_ServiceDesc = grpc.ServiceDesc{\n\tServiceName: \"cart.v1.Cart\",\n\tMethods: []grpc.MethodDesc{\n\t\t{\n\t\t\tMethodName: \"GetCart\",\n\t\t},\n\t},\n}\n"
	if err := os.MkdirAll(filepath.Join(td, "shop", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "shop", "api", "cart_grpc.pb.go"), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	const (
		goCli  = "scip-go gomod shop v1 `shop/api`/CartClient#GetCart." // scip-go: no parens on methods
		goReg  = "scip-go gomod shop v1 `shop/api`/RegisterCartServer()."
		entCli = "scip-go gomod shop v1 `shop/ent`/NewBeerClient()."
		set    = "scip-java maven org.springframework.data/spring-data-redis 3.5 org/springframework/data/redis/core/ValueOperations#set()."
		ops    = "scip-java maven org.springframework.data/spring-data-redis 3.5 org/springframework/data/redis/core/RedisTemplate#opsForValue()."
		list   = "scip-java maven jdk 21 java/util/List#get()."
		amqp   = "scip-java maven org.springframework.amqp/spring-amqp 3.1 org/springframework/amqp/core/AmqpTemplate#convertAndSend()."
	)
	ins("shop", "api/cart_grpc.pb.go", goCli, 1, 10)
	ins("shop", "api/cart_grpc.pb.go", goReg, 1, 12)
	ins("shop", "ent/client.go", entCli, 1, 3)
	ins("shop", "data.go", goCli, 0, 5)
	ins("shop", "server.go", goReg, 0, 8)
	ins("shop", "wire_gen.go", entCli, 0, 2) // looks like a gRPC client by name: it isn't one
	ins("app", "Cart.java", ops, 0, 7)
	ins("app", "Cart.java", set, 0, 7)
	ins("app", "Cart.java", list, 0, 9)
	ins("app", "Order.java", amqp, 0, 4)

	dir := func(repo string) string { return filepath.Join(td, repo) }
	calls, unknown, err := ClientCalls(db, dir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := func(repo, path string, line int, method string, h extract.Hit) {
		t.Helper()
		got := calls[repo][extract.Site{Path: path, Line: line}][method]
		if len(got) != 1 || got[0].Kind != h.Kind || got[0].Role != h.Role || got[0].Arg != h.Arg || !slices.Equal(got[0].Resources, h.Resources) {
			t.Errorf("%s %s:%d %s: got %+v, want %+v", repo, path, line, method, got, h)
		}
	}
	want("shop", "data.go", 5, "GetCart", extract.Hit{Kind: "grpc", Role: "produce", Resources: []string{"cart.v1.Cart/GetCart"}})
	want("shop", "server.go", 8, "RegisterCartServer", extract.Hit{Kind: "grpc", Role: "consume", Resources: []string{"cart.v1.Cart/GetCart"}})
	want("app", "Cart.java", 7, "set", extract.Hit{Kind: "redis", Role: "produce", Arg: 0})
	if calls["shop"][extract.Site{Path: "wire_gen.go", Line: 2}] != nil || calls["app"][extract.Site{Path: "Cart.java", Line: 9}] != nil {
		t.Error("NewBeerClient and List.get are no client calls")
	}
	if calls["app"][extract.Site{Path: "Cart.java", Line: 7}]["opsForValue"] != nil {
		t.Error("opsForValue() is a builder step, not a call")
	}
	u := unknown["app"]["scip-java maven org.springframework.amqp/spring-amqp 3.1 org/springframework/amqp/core/AmqpTemplate#"]
	if u == nil || u.Calls != 1 || !slices.Equal(u.Methods, []string{"convertAndSend"}) {
		t.Errorf("AmqpTemplate is an unknown client: %+v", unknown)
	}
	// answered (weaver answer ... kafka): its calls count, its reads by name
	calls, unknown, _ = ClientCalls(db, dir, nil, nil, map[string]string{"org/springframework/amqp/core/AmqpTemplate#": "kafka"})
	want("app", "Order.java", 4, "convertAndSend", extract.Hit{Kind: "kafka", Role: "produce", Arg: -1})
	if len(unknown["app"]) != 0 {
		t.Errorf("answered type still unknown: %+v", unknown)
	}
}
