// Command gen regenerates internal/spec/tools.json from the backend's MCP
// tool registry (backend/core/internal/transport/mcp/mcp.go, func toolRegistry), so the
// CLI's resource commands and the MCP tools cannot drift apart.
//
// The registry is Go code built with small helper closures, so instead of
// re-implementing it we extract the function's source verbatim, compile it in
// a throwaway package next to a Tool type that keeps Method/Path/Family/Upload
// (the backend hides them from JSON), run it and write its output.
//
//	go run ./internal/spec/gen -src ../backend/core/internal/transport/mcp/mcp.go -out internal/spec/tools.json
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"send.io/cli/internal/spec"
)

func main() {
	src := flag.String("src", "../backend/core/internal/transport/mcp/mcp.go", "path to the backend mcp.go")
	out := flag.String("out", "internal/spec/tools.json", "output file")
	check := flag.Bool("check", false, "exit 1 when the output file is stale instead of writing it")
	flag.Parse()
	data, err := spec.Generate(*src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if *check {
		old, _ := os.ReadFile(*out)
		if !bytes.Equal(old, data) {
			fmt.Fprintf(os.Stderr, "gen: %s is stale; run go generate ./...\n", *out)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
