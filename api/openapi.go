package api

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
)

// The OpenAPI document, generated from the route table.
//
// Writing a spec by hand and keeping it beside the code is a promise to update
// two things forever, and the second one always loses. This derives the whole
// document from routes.go: the paths from the same entries that register the
// handlers, and every schema by REFLECTION over the Go request and response
// types, so a renamed field renames itself in the spec.
//
// What the spec is FOR here is the four clients in this repo plus anything
// outside it: a generated client, a request validator, or simply a reader who
// wants to know what /stateproof returns without reading Go.

// OpenAPIVersion is the specification version this document declares, and
// apiVersion the version of the API it describes. The latter moves when an
// endpoint's shape changes, which is what a client generator pins against — it
// is deliberately not the node's build version, which moves for reasons a client
// does not care about.
const (
	OpenAPIVersion = "3.1.0"
	apiVersion     = "1.0.0"
)

// schemaBuilder accumulates the named component schemas as it walks types, so a
// type used by six endpoints is described once and referenced six times.
type schemaBuilder struct {
	components map[string]map[string]any
	seen       map[reflect.Type]string
}

func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{
		components: map[string]map[string]any{},
		seen:       map[reflect.Type]string{},
	}
}

// schemaFor returns the JSON Schema for a value, registering named components
// for struct types.
func (b *schemaBuilder) schemaFor(v any) map[string]any {
	if v == nil {
		return nil
	}
	return b.typeSchema(reflect.TypeOf(v))
}

func (b *schemaBuilder) typeSchema(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		// A pointer field means "may be absent"; the schema is the pointee's, and
		// absence is expressed by the field simply not being in `required`.
		return b.typeSchema(t.Elem())
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// Unsigned 64-bit values are amounts, heights and nonces. They are encoded
		// as JSON numbers, and the minimum records that they are never negative.
		return map[string]any{"type": "integer", "format": "int64", "minimum": 0}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "description": "base64-encoded bytes"}
		}
		return map[string]any{"type": "array", "items": b.typeSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": b.typeSchema(t.Elem())}
	case reflect.Struct:
		return map[string]any{"$ref": "#/components/schemas/" + b.registerStruct(t)}
	default:
		// Anything else (an interface, a channel) has no honest JSON shape, so it
		// is described as an unconstrained value rather than guessed at.
		return map[string]any{}
	}
}

// registerStruct describes a struct once and returns its component name.
func (b *schemaBuilder) registerStruct(t reflect.Type) string {
	if name, ok := b.seen[t]; ok {
		return name
	}
	name := componentName(t)
	// Record the name BEFORE walking the fields: a type that contains itself
	// (directly or through a slice) would otherwise recurse forever.
	b.seen[t] = name
	b.components[name] = map[string]any{} // placeholder, replaced below

	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: never encoded
		}
		jsonName, omitempty, skip := jsonFieldName(f)
		if skip {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct && jsonName == "" {
			// An embedded struct's fields are inlined by encoding/json, so they are
			// inlined here too rather than appearing as a nested object.
			inner := b.registerStruct(f.Type)
			for k, v := range b.components[inner]["properties"].(map[string]any) {
				props[k] = v
			}
			if req, ok := b.components[inner]["required"].([]string); ok {
				required = append(required, req...)
			}
			continue
		}
		if jsonName == "" {
			jsonName = f.Name
		}
		schema := b.typeSchema(f.Type)
		if doc := strings.TrimSpace(f.Tag.Get("doc")); doc != "" {
			schema["description"] = doc
		}
		props[jsonName] = schema
		if !omitempty && f.Type.Kind() != reflect.Pointer {
			required = append(required, jsonName)
		}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	b.components[name] = out
	return name
}

// componentName is the schema name for a type: its Go name, prefixed by its
// package when that is needed to keep two same-named types apart.
//
// The first letter is capitalized whatever the Go name was, because a type being
// unexported is a fact about this package's internals and not about the API: a
// client generated from this spec should see SendRequest, not sendRequest.
func componentName(t reflect.Type) string {
	if t.Name() == "" {
		return "Anonymous"
	}
	name := exported(t.Name())
	if pkg := t.PkgPath(); pkg != "" {
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			pkg = pkg[i+1:]
		}
		if pkg != "api" {
			return exported(pkg) + name
		}
	}
	return name
}

// exported upper-cases the first letter of an ASCII identifier.
func exported(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// jsonFieldName reads a struct field's encoding/json tag.
func jsonFieldName(f reflect.StructField) (name string, omitempty, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, false
}

// openAPIDoc builds the whole document.
func (s *Server) openAPIDoc() map[string]any {
	b := newSchemaBuilder()
	paths := map[string]any{}

	for _, rt := range s.routes() {
		op := map[string]any{
			"summary":     rt.Summary,
			"operationId": operationID(rt),
			"tags":        []string{tagFor(rt)},
		}
		if rt.Desc != "" {
			op["description"] = rt.Desc
		}
		if len(rt.Params) > 0 {
			params := make([]map[string]any, 0, len(rt.Params))
			for _, p := range rt.Params {
				params = append(params, map[string]any{
					"name":        p.Name,
					"in":          p.In,
					"description": p.Desc,
					"required":    p.Required || p.In == "path",
					"schema":      map[string]any{"type": p.Type},
				})
			}
			op["parameters"] = params
		}
		if rt.Request != nil {
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/json": map[string]any{"schema": b.schemaFor(rt.Request)},
				},
			}
		}

		contentType := rt.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		ok := map[string]any{"description": "success"}
		if rt.Response != nil {
			ok["content"] = map[string]any{contentType: map[string]any{"schema": b.schemaFor(rt.Response)}}
		} else {
			ok["content"] = map[string]any{contentType: map[string]any{}}
		}
		responses := map[string]any{"200": ok}
		// Every endpoint can refuse a request, and a rate limit applies to all of
		// them — including the reads, since the expensive requests here are reads.
		errSchema := map[string]any{
			"content": map[string]any{
				"application/json": map[string]any{"schema": b.schemaFor(ErrorResponse{})},
			},
		}
		withDesc := func(d string) map[string]any {
			m := map[string]any{"description": d}
			for k, v := range errSchema {
				m[k] = v
			}
			return m
		}
		responses["400"] = withDesc("malformed request")
		responses["429"] = withDesc("rate limited")
		if rt.Auth {
			op["security"] = []map[string]any{{"bearerAuth": []string{}}}
			responses["401"] = withDesc("missing or invalid API token")
		}
		op["responses"] = responses

		entry, _ := paths[rt.Path].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			paths[rt.Path] = entry
		}
		for _, m := range rt.Methods {
			entry[strings.ToLower(m)] = op
		}
	}

	// The component map is typed for building; the document is handed to
	// encoding/json as plain `any` so callers (and the tests that walk it) see one
	// uniform shape rather than two.
	schemas := make(map[string]any, len(b.components))
	for name, schema := range b.components {
		schemas[name] = schema
	}

	return map[string]any{
		"openapi": OpenAPIVersion,
		"info": map[string]any{
			"title": "DNAS node API",
			"description": "The HTTP interface to a DNAS node. This document is generated from " +
				"the same route table that registers the handlers, so it cannot describe an " +
				"endpoint that is not served or miss one that is.\n\n" +
				"DNAS is a learning project, not money. Do not point it at the internet.",
			"version": apiVersion,
		},
		"servers": []map[string]any{{"url": "http://127.0.0.1:8080", "description": "a local node"}},
		"components": map[string]any{
			"schemas": schemas,
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":   "http",
					"scheme": "bearer",
					"description": "Set DNAS_API_TOKEN on the node to require this on write " +
						"endpoints. It is one shared token, not per-user authentication.",
				},
			},
		},
		"paths": paths,
	}
}

// operationID is a stable, unique name for one endpoint, as a code generator
// needs for the method it will emit.
func operationID(rt route) string {
	name := strings.Trim(rt.Path, "/")
	if name == "" {
		name = "explorer"
	}
	name = strings.NewReplacer("/", "_", "{", "", "}", "", ".", "_", "-", "_").Replace(name)
	verb := strings.ToLower(rt.Methods[0])
	return verb + "_" + name
}

// tagFor groups endpoints in the rendered document by what they are about.
func tagFor(rt route) string {
	switch {
	case strings.HasPrefix(rt.Path, "/mempool"), rt.Path == "/tx", strings.HasPrefix(rt.Path, "/tx/"),
		rt.Path == "/estimatefee", rt.Path == "/send":
		return "transactions"
	case strings.HasPrefix(rt.Path, "/peers"), strings.HasPrefix(rt.Path, "/bans"),
		rt.Path == "/unban", rt.Path == "/addpeer", rt.Path == "/droppeer":
		return "network"
	case rt.Path == "/blocktemplate", rt.Path == "/submitblock", rt.Path == "/submitshare",
		rt.Path == "/shares", rt.Path == "/pool", rt.Path == "/mine", rt.Path == "/generate":
		return "mining"
	case strings.HasPrefix(rt.Path, "/header"), strings.HasPrefix(rt.Path, "/cf"),
		strings.HasPrefix(rt.Path, "/proof"), strings.HasPrefix(rt.Path, "/stateproof"),
		strings.HasPrefix(rt.Path, "/snapshot"):
		return "light-client"
	case strings.HasPrefix(rt.Path, "/multisig"), strings.HasPrefix(rt.Path, "/htlc"),
		strings.HasPrefix(rt.Path, "/vault"), strings.HasPrefix(rt.Path, "/wallet"):
		return "wallet"
	case strings.HasPrefix(rt.Path, "/asset"):
		return "assets"
	default:
		return "chain"
	}
}

var (
	openAPIOnce  sync.Once
	openAPICache map[string]any
)

// openAPI serves the document. It is built once: the route table is fixed for
// the life of the process, so regenerating it per request would only burn CPU on
// whatever scrapes it.
func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) {
	openAPIOnce.Do(func() { openAPICache = s.openAPIDoc() })
	writeJSON(w, http.StatusOK, openAPICache)
}
