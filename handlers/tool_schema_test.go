package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claude2api/models"
)

const artifactCollectionPattern = `^(?!\.\.?(?:\/|$))[A-Za-z0-9_\-.~:@+]{1,200}(?:\/(?!\.\.?(?:\/|$))[A-Za-z0-9_\-.~:@+]{1,200}){0,14}$`

func TestArtifactCollectionPattern(t *testing.T) {
	collection := map[string]interface{}{"type": "string", "pattern": artifactCollectionPattern}
	for _, nested := range []bool{false, true} {
		name := "collection"
		valueSchema := collection
		if nested {
			name = "writes"
			valueSchema = map[string]interface{}{
				"type": "array", "items": map[string]interface{}{
					"type": "object", "properties": map[string]interface{}{"collection": collection},
					"required": []string{"collection"},
				},
			}
		}
		t.Run(name, func(t *testing.T) {
			tools, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{{
				Name: "Artifact", InputSchema: map[string]interface{}{
					"type": "object", "properties": map[string]interface{}{name: valueSchema},
					"required": []string{name}, "additionalProperties": false,
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				value string
				valid bool
			}{
				{"notes", true}, {"team/release-1.0", true}, {".hidden/a@b+c", true},
				{"", false}, {".", false}, {"..", false}, {"../notes", false},
				{"notes/./item", false}, {"notes/../item", false}, {"/notes", false},
				{"notes/", false}, {"notes//item", false}, {"with space", false},
				{strings.Repeat("a", 201), false}, {strings.Repeat("a/", 15) + "a", false},
			} {
				var value interface{} = tc.value
				if nested {
					value = []interface{}{map[string]interface{}{"collection": tc.value}}
				}
				call, _ := json.Marshal(map[string]interface{}{"name": "Artifact", "input": map[string]interface{}{name: value}})
				_, _, err := tools.parse("[TOOL_CALL]" + string(call) + "[/TOOL_CALL]")
				if (err == nil) != tc.valid {
					t.Errorf("collection %q: error=%v, valid=%v", tc.value, err, tc.valid)
				}
			}
		})
	}
}

func TestToolSchemaRegexpKeywords(t *testing.T) {
	for _, tc := range []struct {
		name, schema, valid, invalid string
	}{
		{"lookahead", `{"type":"string","pattern":"^(?=a)a$"}`, `"a"`, `"b"`},
		{"backreference", `{"type":"string","pattern":"^(a)\\1$"}`, `"aa"`, `"ab"`},
		{"local-ref", `{"$ref":"#/$defs/value","$defs":{"value":{"type":"string","pattern":"^(?!x)y$"}}}`, `"y"`, `"x"`},
		{"pattern-properties", `{"type":"object","patternProperties":{"^(?!private_)":{"type":"integer"}},"additionalProperties":false}`, `{"public":1}`, `{"private_key":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]interface{}
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			tools, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{{Name: "Check", InputSchema: schema}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{tc.valid, tc.invalid} {
				var input interface{}
				json.Unmarshal([]byte(value), &input)
				err := tools.schemas["Check"].Validate(input)
				if (err == nil) != (value == tc.valid) {
					t.Fatalf("value %s: %v", value, err)
				}
			}
		})
	}
}

func TestToolSchemaRejectsInvalidRegexp(t *testing.T) {
	for _, schema := range []map[string]interface{}{
		{"type": "string", "pattern": "(?!("},
		{"type": "object", "patternProperties": map[string]interface{}{"(?!(": map[string]interface{}{"type": "string"}}},
	} {
		if _, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{{Name: "Check", InputSchema: schema}}}); err == nil {
			t.Fatal("invalid regexp was accepted")
		}
	}
}

func TestToolSchemaRejectsExternalReferences(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte(`{"type":"string"}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(`{"type":"string"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{server.URL + "/schema", "file://" + path} {
		_, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{{
			Name: "Check", InputSchema: map[string]interface{}{"$ref": ref},
		}}})
		if err == nil {
			t.Fatalf("external reference accepted: %s", ref)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("schema compiler made an external request")
	}
}

func TestToolSchemaRegexpTimeout(t *testing.T) {
	pattern := map[string]interface{}{"type": "string", "pattern": "^(a+)+$"}
	value := strings.Repeat("a", 100) + "!"
	for _, tc := range []struct {
		name   string
		schema map[string]interface{}
		value  interface{}
	}{
		{"pattern", pattern, value},
		{"not", map[string]interface{}{"not": pattern}, value},
		{"anyOf", map[string]interface{}{"anyOf": []interface{}{pattern, true}}, value},
		{"pattern-properties", map[string]interface{}{"patternProperties": map[string]interface{}{"^(a+)+$": true}}, map[string]interface{}{value: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{{Name: "Check", InputSchema: tc.schema}}})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if err := tools.schemas["Check"].Validate(tc.value); err == nil {
				t.Fatal("timed out regexp accepted the value")
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("regexp timeout took %s", elapsed)
			}
		})
	}
}
