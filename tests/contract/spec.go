// Package contract guards the OpenAPI specification against silent drift
// from the assembled router (issue #41 [O-32]).
//
// The contract has two directions and both are enforced:
//
//  1. every spec path+method exists in the chi router (route enumeration),
//  2. every router route under the versioned base is documented in the spec
//     (bidirectional), with 404/405 transport semantics asserted on the wire.
//
// The response envelope ({data, meta} success / {error:{code,message,details?}}
// failure) is conformance-checked against internal/platform/httpx helpers,
// pkg/errors kind→status mapping and the wire (sample assertions, no database
// required anywhere).
//
// File ownership (issue #41): this package + api/openapi/orvexa-v1.yaml
// (drift fixes only). Router changes found by these tests must be filed as
// issues, never patched here.
package contract

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// SpecPath is the OpenAPI document path relative to the repository root.
const SpecPath = "api/openapi/orvexa-v1.yaml"

// methodKeys are the OpenAPI path-item keys that express HTTP operations.
// Everything else (summary, parameters, requestBody, …) is path-item metadata.
var methodKeys = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	"trace":            true,
}

// Spec is the parsed OpenAPI document, reduced to the pieces the guard needs.
type Spec struct {
	doc       map[string]any
	pathItems map[string]map[string]any // path → full path-item node
	ops       map[string]map[string]any // path → method(lowercase) → operation node
}

// LoadSpec parses api/openapi/orvexa-v1.yaml. The repository root is derived
// from this file's location so the guard works from any working directory.
func LoadSpec() (*Spec, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("contract: cannot locate spec.go to anchor the repository root")
	}
	// tests/contract/spec.go → repo root is three directories up.
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	return LoadSpecAt(filepath.Join(root, SpecPath))
}

// LoadSpecAt parses the document at an explicit path (kept exported so the
// loader itself stays testable and reusable by future tooling).
func LoadSpecAt(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("contract: read spec: %w", err)
	}
	var doc map[string]any
	// yaml.v3 rejects duplicate mapping keys, so a duplicated top-level
	// section (the regression fixed under issue #41) fails here instead of
	// silently shadowing half the contract.
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("contract: spec is not parseable: %w", err)
	}
	pathsDoc, ok := doc["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("contract: spec has no paths object")
	}
	pathItems := make(map[string]map[string]any, len(pathsDoc))
	ops := make(map[string]map[string]any, len(pathsDoc))
	for p, node := range pathsDoc {
		item, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("contract: path %q is not a mapping", p)
		}
		pathItems[p] = item
		methodOps := make(map[string]any, len(item))
		for key, op := range item {
			uk := strings.ToUpper(key)
			if methodKeys[uk] {
				methodOps[strings.ToLower(uk)] = op
			}
		}
		ops[p] = methodOps
	}
	return &Spec{doc: doc, pathItems: pathItems, ops: ops}, nil
}

// MustLoadSpec is the test-facing loader: a spec that cannot be parsed is a
// hard contract failure.
func MustLoadSpec(t testing.TB) *Spec {
	t.Helper()
	s, err := LoadSpec()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// RouteSet returns the documented surface: path → set of HTTP methods
// (uppercase), e.g. "/customers" → {GET, POST}.
func (s *Spec) RouteSet() map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(s.ops))
	for p, ops := range s.ops {
		methods := make(map[string]bool, len(ops))
		for m := range ops {
			methods[strings.ToUpper(m)] = true
		}
		out[p] = methods
	}
	return out
}

// Operation returns the operation node for path+method.
func (s *Spec) Operation(path, method string) (map[string]any, bool) {
	ops, ok := s.ops[path]
	if !ok {
		return nil, false
	}
	op, ok := ops[strings.ToLower(method)]
	if !ok {
		return nil, false
	}
	m, _ := op.(map[string]any)
	return m, m != nil
}

// ResponseKeys returns the documented response status codes for an operation.
func (s *Spec) ResponseKeys(path, method string) map[string]bool {
	out := map[string]bool{}
	op, ok := s.Operation(path, method)
	if !ok {
		return out
	}
	resps, _ := op["responses"].(map[string]any)
	for code := range resps {
		out[code] = true
	}
	return out
}

// ResponseDescription returns the description text of one documented response
// (used to tie wire codes like search.not_configured back to the spec text).
func (s *Spec) ResponseDescription(path, method, status string) (string, bool) {
	op, ok := s.Operation(path, method)
	if !ok {
		return "", false
	}
	resps, _ := op["responses"].(map[string]any)
	resp, ok := resps[status]
	if !ok {
		return "", false
	}
	m, ok := resp.(map[string]any)
	if !ok {
		return "", false
	}
	d, _ := m["description"].(string)
	return d, true
}

// OKDataProperties returns the declared property names of the `data` object
// inside an operation's 200 JSON response (empty when the response does not
// declare that shape). Used for envelope conformance of /search/conversations.
func (s *Spec) OKDataProperties(path, method string) map[string]bool {
	out := map[string]bool{}
	op, ok := s.Operation(path, method)
	if !ok {
		return out
	}
	resps, _ := op["responses"].(map[string]any)
	resp, _ := resps["200"].(map[string]any)
	content, _ := resp["content"].(map[string]any)
	jsonNode, _ := content["application/json"].(map[string]any)
	schema, _ := jsonNode["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	data, _ := props["data"].(map[string]any)
	dataProps, _ := data["properties"].(map[string]any)
	for name := range dataProps {
		out[name] = true
	}
	return out
}

// HitProperties returns the declared property names of the search hit items
// (the shape nested at data.properties.hits.items.properties).
func (s *Spec) HitProperties(path, method string) map[string]bool {
	out := map[string]bool{}
	op, ok := s.Operation(path, method)
	if !ok {
		return out
	}
	resps, _ := op["responses"].(map[string]any)
	resp, _ := resps["200"].(map[string]any)
	content, _ := resp["content"].(map[string]any)
	jsonNode, _ := content["application/json"].(map[string]any)
	schema, _ := jsonNode["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	data, _ := props["data"].(map[string]any)
	dataProps, _ := data["properties"].(map[string]any)
	hits, _ := dataProps["hits"].(map[string]any)
	items, _ := hits["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	for name := range itemProps {
		out[name] = true
	}
	return out
}

// ResolveRef dereferences an internal JSON pointer ("#/components/…").
func (s *Spec) ResolveRef(ref string) error {
	if !strings.HasPrefix(ref, "#/") {
		return fmt.Errorf("external or malformed $ref %q", ref)
	}
	cur := any(s.doc)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return fmt.Errorf("$ref %q traverses a non-mapping at %q", ref, seg)
		}
		cur, ok = m[seg]
		if !ok {
			return fmt.Errorf("$ref %q does not resolve (missing %q)", ref, seg)
		}
	}
	return nil
}

// DanglingRefs walks the whole document and returns every $ref that does not
// resolve. Empty ⇒ the spec is internally consistent.
func (s *Spec) DanglingRefs() []string {
	var bad []string
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if k == "$ref" {
					if r, ok := child.(string); ok {
						if err := s.ResolveRef(r); err != nil {
							bad = append(bad, fmt.Sprintf("%s (%v)", r, err))
						}
						continue
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(s.doc)
	sort.Strings(bad)
	return bad
}

// IsPublic reports whether the operation opts out of the document-level
// security requirement (`security: []` at operation or path-item level) —
// the public surface that must NOT answer 401 for unauthenticated probes.
func (s *Spec) IsPublic(path, method string) bool {
	if op, ok := s.Operation(path, method); ok {
		if sec, present := op["security"]; present {
			list, _ := sec.([]any)
			return len(list) == 0
		}
	}
	item, ok := s.pathItems[path]
	if !ok {
		return false
	}
	if sec, present := item["security"]; present {
		list, _ := sec.([]any)
		return len(list) == 0
	}
	return false
}

// ComponentNames returns the declared component schema names (the spec-level
// schema vocabulary clients build against).
func (s *Spec) ComponentNames() []string {
	comp, _ := s.doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)
	out := make([]string, 0, len(schemas))
	for name := range schemas {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ServerBase returns the declared base path (servers[0].url), "/api/v1".
func (s *Spec) ServerBase() string {
	servers, _ := s.doc["servers"].([]any)
	if len(servers) == 0 {
		return ""
	}
	first, _ := servers[0].(map[string]any)
	u, _ := first["url"].(string)
	return u
}
