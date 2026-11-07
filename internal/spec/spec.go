// Package spec holds the send.io API surface the CLI's resource commands are
// built from: tools.json, generated from the backend MCP tool registry.
package spec

//go:generate go run ./gen -src ../../../backend/core/internal/transport/mcp/mcp.go -out tools.json

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed tools.json
var toolsJSON []byte

// Param is one input of a tool (a JSON-schema property).
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // string, boolean, integer, object, array
	Items       string   `json:"items,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// Tool is one API operation.
type Tool struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Method      string  `json:"method"`
	Path        string  `json:"path"`
	Family      string  `json:"family"`
	Upload      string  `json:"upload,omitempty"`
	Params      []Param `json:"params"`
	// InputSchema is the tool's JSON schema exactly as the hosted MCP server
	// lists it (sendio mcp serve exposes the same one).
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// PathParams returns the {name} placeholders of the tool path, in order.
func (t Tool) PathParams() []string {
	var out []string
	p := t.Path
	for {
		i := strings.IndexByte(p, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(p[i:], '}')
		if j < 0 {
			return out
		}
		out = append(out, p[i+1:i+j])
		p = p[i+j+1:]
	}
}

// Param returns the named parameter.
func (t Tool) Param(name string) (Param, bool) {
	for _, p := range t.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

// Paginated reports whether the tool takes cursor pagination.
func (t Tool) Paginated() bool {
	_, a := t.Param("after")
	_, l := t.Param("limit")
	return t.Method == "GET" && a && l
}

var (
	once  sync.Once
	tools []Tool
	byNm  map[string]Tool
)

func load() {
	if err := json.Unmarshal(toolsJSON, &tools); err != nil {
		panic("spec: tools.json: " + err.Error())
	}
	byNm = map[string]Tool{}
	for _, t := range tools {
		byNm[t.Name] = t
	}
}

// Tools returns every tool, in registry order.
func Tools() []Tool { once.Do(load); return tools }

// Lookup returns the named tool.
func Lookup(name string) (Tool, bool) { once.Do(load); t, ok := byNm[name]; return t, ok }

// Generate extracts the tool registry (ToolRegistry) from the backend's mcp.go, runs it and
// returns the normalised tools.json bytes.
func Generate(mcpGo string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, mcpGo, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && (fd.Name.Name == "toolRegistry" || fd.Name.Name == "ToolRegistry") && fd.Recv == nil {
			fn = fd
		}
	}
	if fn == nil {
		return nil, fmt.Errorf("toolRegistry not found in %s", mcpGo)
	}
	var body bytes.Buffer
	if err := printer.Fprint(&body, fset, fn); err != nil {
		return nil, err
	}
	prog := `package main

import (
	"encoding/json"
	"os"
)

type M = map[string]any

type Tool struct {
	Name        string ` + "`json:\"name\"`" + `
	Description string ` + "`json:\"description\"`" + `
	InputSchema M      ` + "`json:\"inputSchema\"`" + `
	Method      string ` + "`json:\"method\"`" + `
	Path        string ` + "`json:\"path\"`" + `
	Family      string ` + "`json:\"family\"`" + `
	Upload      string ` + "`json:\"upload\"`" + `
}

` + strings.ReplaceAll(body.String(), "httpx.M", "M") + `

func main() { _ = json.NewEncoder(os.Stdout).Encode(` + fn.Name.Name + `()) }
`
	dir, err := os.MkdirTemp("", "sendio-specgen-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(prog), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module specgen\n\ngo 1.21\n"), 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running extracted registry: %v: %s", err, stderr.String())
	}
	return Normalize(raw)
}

// Normalize turns the registry's JSON (MCP tool shape) into tools.json.
func Normalize(raw []byte) ([]byte, error) {
	var in []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
		Method      string         `json:"method"`
		Path        string         `json:"path"`
		Family      string         `json:"family"`
		Upload      string         `json:"upload"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(in))
	for _, t := range in {
		props, _ := t.InputSchema["properties"].(map[string]any)
		req := map[string]bool{}
		if rl, ok := t.InputSchema["required"].([]any); ok {
			for _, r := range rl {
				req[fmt.Sprint(r)] = true
			}
		}
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		params := make([]Param, 0, len(names))
		for _, k := range names {
			pm, _ := props[k].(map[string]any)
			p := Param{Name: k, Required: req[k]}
			p.Type, _ = pm["type"].(string)
			p.Description, _ = pm["description"].(string)
			if items, ok := pm["items"].(map[string]any); ok {
				p.Items, _ = items["type"].(string)
			}
			if ev, ok := pm["enum"].([]any); ok {
				for _, e := range ev {
					p.Enum = append(p.Enum, fmt.Sprint(e))
				}
			}
			params = append(params, p)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		out = append(out, Tool{Name: t.Name, Description: t.Description, Method: t.Method, Path: t.Path, Family: t.Family, Upload: t.Upload, Params: params, InputSchema: schema})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
