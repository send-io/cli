package spec

import (
	"os"
	"testing"
)

// TestSpecUpToDate regenerates tools.json from the backend registry (when the
// backend is checked out next to the CLI) and fails if the embedded copy is
// stale: run `go generate ./...` in cli/.
func TestSpecUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	src := "../../../backend/core/internal/transport/mcp/mcp.go"
	if _, err := os.Stat(src); err != nil {
		t.Skip("backend not checked out next to the CLI")
	}
	want, err := Generate(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(toolsJSON) {
		t.Fatal("internal/spec/tools.json is stale: run `go generate ./...` in cli/")
	}
}

func TestToolsLoad(t *testing.T) {
	if len(Tools()) < 100 {
		t.Fatalf("only %d tools", len(Tools()))
	}
	tl, ok := Lookup("get-contact-import")
	if !ok || tl.Method != "GET" || tl.Path != "/contacts/imports/{id}" {
		t.Fatalf("lookup %+v", tl)
	}
	if pp := tl.PathParams(); len(pp) != 1 || pp[0] != "id" {
		t.Fatalf("path params %v", pp)
	}
	l, _ := Lookup("list-contacts")
	if !l.Paginated() {
		t.Fatal("list-contacts should paginate")
	}
	if u, _ := Lookup("create-contact-import"); u.Upload != "csv" {
		t.Fatal("upload not carried")
	}
}
