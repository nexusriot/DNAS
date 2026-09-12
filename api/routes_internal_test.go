package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

func specServer(t *testing.T) *Server {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	n := node.New(node.Config{ListenAddr: ":0"}, core.NewBlockchain(), core.NewMempool(), w)
	return New(n)
}

// The table is the API's description, so an entry that describes nothing is a
// hole in it. This is what stops "add the handler now, document it later".
func TestEveryRouteIsDescribed(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range specServer(t).routes() {
		if rt.Pattern == "" {
			t.Fatalf("route with no pattern: %+v", rt)
		}
		if seen[rt.Pattern] {
			t.Errorf("%s: registered twice, which http.ServeMux would panic on", rt.Pattern)
		}
		seen[rt.Pattern] = true

		if rt.handler == nil {
			t.Errorf("%s: no handler, so it would 404 despite being documented", rt.Pattern)
		}
		if strings.TrimSpace(rt.Summary) == "" {
			t.Errorf("%s: no summary", rt.Pattern)
		}
		if len(rt.Methods) == 0 {
			t.Errorf("%s: no methods", rt.Pattern)
		}
		// A JSON endpoint must name the type it answers with; a non-JSON one must
		// say what it answers with instead. Neither is optional, because "object"
		// is exactly the uselessly vague schema this table exists to avoid.
		if rt.Response == nil && rt.ContentType == "" {
			t.Errorf("%s: neither a response type nor a content type", rt.Pattern)
		}
	}
}

// Every {parameter} in a path must be declared, or a generated client will build
// a URL with a literal brace in it.
func TestPathParametersAreDeclared(t *testing.T) {
	placeholder := regexp.MustCompile(`\{([^}]+)\}`)
	for _, rt := range specServer(t).routes() {
		declared := map[string]bool{}
		for _, p := range rt.Params {
			if p.In == "path" {
				declared[p.Name] = true
			}
		}
		for _, m := range placeholder.FindAllStringSubmatch(rt.Path, -1) {
			if !declared[m[1]] {
				t.Errorf("%s: path parameter %q is not declared", rt.Path, m[1])
			}
		}
		for _, p := range rt.Params {
			if p.In == "path" && !strings.Contains(rt.Path, "{"+p.Name+"}") {
				t.Errorf("%s: declares path parameter %q that the path does not contain", rt.Path, p.Name)
			}
			if p.In != "path" && p.In != "query" {
				t.Errorf("%s: parameter %q has location %q", rt.Path, p.Name, p.In)
			}
			if p.Type == "" {
				t.Errorf("%s: parameter %q has no type", rt.Path, p.Name)
			}
		}
	}
}

// An endpoint marked as needing the token must ACTUALLY refuse an unauthenticated
// request. Marking it in the table and forgetting s.guard would publish a
// security property the server does not have — which is worse than not
// publishing one.
func TestRoutesMarkedAuthAreActuallyGuarded(t *testing.T) {
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	n := node.New(node.Config{ListenAddr: ":0"}, core.NewBlockchain(), core.NewMempool(), w)
	s := NewWithToken(n, "s3cret")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	guarded := 0
	for _, rt := range s.routes() {
		if !rt.Auth {
			continue
		}
		guarded++
		resp, err := http.Post(srv.URL+rt.Pattern, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: %v", rt.Pattern, err)
		}
		body := resp.StatusCode
		resp.Body.Close()
		if body != http.StatusUnauthorized {
			t.Errorf("%s is documented as requiring the token but answered %d without one",
				rt.Pattern, body)
		}
	}
	if guarded == 0 {
		t.Fatal("no route is marked as requiring auth, so this test proved nothing")
	}
}

// ...and the mirror: an endpoint NOT marked as needing the token must be
// reachable without one, or the spec understates what a client must send.
func TestRoutesNotMarkedAuthAreOpen(t *testing.T) {
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	n := node.New(node.Config{ListenAddr: ":0"}, core.NewBlockchain(), core.NewMempool(), w)
	s := NewWithToken(n, "s3cret")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	for _, rt := range s.routes() {
		if rt.Auth || rt.ContentType == "text/event-stream" {
			continue // the SSE stream holds the connection open by design
		}
		method := rt.Methods[0]
		req, err := http.NewRequest(method, srv.URL+rt.Pattern, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", rt.Pattern, err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusUnauthorized {
			t.Errorf("%s answered 401 but is not documented as requiring the token", rt.Pattern)
		}
	}
}

// The generated document must be structurally valid, and — the part that matters
// — every $ref it emits must resolve, or a code generator stops at the first one.
func TestGeneratedDocumentIsWellFormed(t *testing.T) {
	doc := specServer(t).openAPIDoc()

	if doc["openapi"] != OpenAPIVersion {
		t.Errorf("openapi = %v, want %s", doc["openapi"], OpenAPIVersion)
	}
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("document has no paths")
	}
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if len(schemas) == 0 {
		t.Fatal("document has no component schemas")
	}

	// Walk the whole document and resolve every reference.
	var refs []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, inner := range t {
				if k == "$ref" {
					if s, ok := inner.(string); ok {
						refs = append(refs, s)
					}
					continue
				}
				walk(inner)
			}
		case []any:
			for _, inner := range t {
				walk(inner)
			}
		case []map[string]any:
			for _, inner := range t {
				walk(inner)
			}
		}
	}
	walk(doc)
	if len(refs) == 0 {
		t.Fatal("no schema references at all, so nothing is really described")
	}
	const prefix = "#/components/schemas/"
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref, prefix)
		if !ok {
			t.Errorf("reference %q is not a local component reference", ref)
			continue
		}
		if _, ok := schemas[name]; !ok {
			t.Errorf("reference %q does not resolve", ref)
		}
	}

	// No empty component: a placeholder left behind by the recursion guard would
	// describe a type as having no fields at all.
	for name, schema := range schemas {
		m, _ := schema.(map[string]any)
		if len(m) == 0 {
			t.Errorf("component %q is empty", name)
		}
	}
}

// An operationId is what a generator names the method it emits, so two endpoints
// sharing one silently loses an endpoint.
func TestOperationIDsAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, rt := range specServer(t).routes() {
		id := operationID(rt)
		if prev, ok := seen[id]; ok {
			t.Errorf("%s and %s share the operationId %q", prev, rt.Path, id)
		}
		seen[id] = rt.Path
	}
}

// The schemas are derived from the Go types rather than written alongside them,
// which is the only reason they can be trusted. Spot-check that a response type's
// actual fields came through, including one that must be optional.
func TestSchemasComeFromTheGoTypes(t *testing.T) {
	doc := specServer(t).openAPIDoc()
	components := doc["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)

	supply, ok := schemas["CoreSupply"].(map[string]any)
	if !ok {
		t.Fatalf("core.Supply was not described; schemas present: %v", keysOf(schemas))
	}
	props := supply["properties"].(map[string]any)
	for _, field := range []string{"height", "minted", "burned", "circulating", "consistent"} {
		if _, ok := props[field]; !ok {
			t.Errorf("Supply schema is missing %q", field)
		}
	}
	if m, _ := props["minted"].(map[string]any); m["type"] != "integer" {
		t.Errorf("minted is %v, want an integer", m["type"])
	}
	if m, _ := props["consistent"].(map[string]any); m["type"] != "boolean" {
		t.Errorf("consistent is %v, want a boolean", m["type"])
	}

	// A pointer field is optional, so it must not be in `required` — a generated
	// client would otherwise be forced to send a nonce it wants the node to pick.
	send := schemas["SendRequest"].(map[string]any)
	for _, r := range toStrings(send["required"]) {
		if r == "nonce" {
			t.Error("sendRequest.nonce is a pointer (optional) but the schema requires it")
		}
	}
}

// And the document is actually served.
func TestOpenAPIIsServed(t *testing.T) {
	s := specServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("served document is not JSON: %v", err)
	}
	paths := doc["paths"].(map[string]any)
	for _, want := range []string{"/info", "/supply", "/tx/{hash}", "/stateproof/{address}"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("served document does not describe %s", want)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
