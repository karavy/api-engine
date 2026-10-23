package apiengine

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func mustSchema(t *testing.T, raw string) *Schema {
	t.Helper()
	schema, err := ParseSchema([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func structuredData(t *testing.T, response *Response, schema *Schema) map[string]any {
	t.Helper()
	out, err := response.Structured(schema)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Status  int
		Success bool
		Headers map[string][]string
		Data    map[string]any
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != response.StatusCode || envelope.Success != response.IsSuccess() {
		t.Fatalf("unexpected envelope: %s", out)
	}
	return envelope.Data
}

func TestStructuredFormats(t *testing.T) {
	tests := []struct {
		name, schema, body string
		want               map[string]any
	}{
		{
			"xml",
			`{"format":"xml","fields":[
				{"name":"id","path":"order/@id","type":"int","required":true},
				{"name":"status","path":"order/status"},
				{"name":"items","path":"order/items/item","type":"string[]"},
				{"name":"missing","path":"order/nope"}]}`,
			`<order id="42"><status> shipped </status><items><item>a</item><item>b</item></items></order>`,
			map[string]any{"id": 42.0, "status": "shipped", "items": []any{"a", "b"}, "missing": nil},
		},
		{
			"form",
			`{"format":"form","fields":[{"name":"ok","path":"ok","type":"bool"},{"name":"tags","path":"tag","type":"string[]"}]}`,
			`ok=true&tag=x&tag=y%20z`,
			map[string]any{"ok": true, "tags": []any{"x", "y z"}},
		},
		{
			"csv",
			`{"format":"csv","fields":[{"name":"first","path":"name"},{"name":"ages","path":"1","type":"int[]"}]}`,
			"name,age\nann,30\nbob,41\n",
			map[string]any{"first": "ann", "ages": []any{30.0, 41.0}},
		},
		{
			"lines",
			`{"format":"lines","fields":[{"name":"status","path":"Status:"},{"name":"head","path":"0"}]}`,
			"HTTP ok\r\nStatus: running\r\n",
			map[string]any{"status": "running", "head": "HTTP ok"},
		},
		{
			"text",
			`{"format":"text","fields":[{"name":"load","path":"load=([0-9.]+)","type":"float"}]}`,
			"up, load=1.25 now",
			map[string]any{"load": 1.25},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &Response{StatusCode: 200, Headers: http.Header{"X-A": {"b"}}, Body: []byte(test.body)}
			got := structuredData(t, response, mustSchema(t, test.schema))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestStructuredWithoutSchema(t *testing.T) {
	out, err := (&Response{StatusCode: 404, Body: []byte(` {"error":"gone"} `)}).Structured(nil)
	if err != nil || string(out) != `{"status":404,"success":false,"headers":null,"data":{"error":"gone"}}` {
		t.Fatalf("got %s, %v", out, err)
	}
	out, err = (&Response{StatusCode: 204}).Structured(nil)
	if err != nil || string(out) != `{"status":204,"success":true,"headers":null,"data":null}` {
		t.Fatalf("got %s, %v", out, err)
	}
	if _, err := (&Response{StatusCode: 200, Body: []byte("plain")}).Structured(nil); !errors.Is(err, ErrSchemaRequired) {
		t.Fatalf("got %v", err)
	}
}

func TestStructuredMismatch(t *testing.T) {
	tests := []struct{ name, schema, body string }{
		{"required missing", `{"format":"form","fields":[{"name":"a","path":"a","required":true}]}`, "b=1"},
		{"bad int", `{"format":"form","fields":[{"name":"a","path":"a","type":"int"}]}`, "a=x"},
		{"non finite float", `{"format":"form","fields":[{"name":"a","path":"a","type":"float"}]}`, "a=NaN"},
		{"bad xml", `{"format":"xml","fields":[{"name":"a","path":"r/a"}]}`, "<r><a>"},
		{"no xml", `{"format":"xml","fields":[{"name":"a","path":"r/a"}]}`, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (&Response{Body: []byte(test.body)}).Structured(mustSchema(t, test.schema))
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestParseSchemaRejectsInvalid(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      `nope`,
		"unknown key":   `{"format":"xml","fields":[{"name":"a","path":"a"}],"extra":1}`,
		"trailing data": `{"format":"xml","fields":[{"name":"a","path":"a"}]} {}`,
		"format":        `{"format":"yaml","fields":[{"name":"a","path":"a"}]}`,
		"no fields":     `{"format":"xml","fields":[]}`,
		"duplicate":     `{"format":"xml","fields":[{"name":"a","path":"a"},{"name":"a","path":"b"}]}`,
		"empty path":    `{"format":"xml","fields":[{"name":"a","path":""}]}`,
		"type":          `{"format":"xml","fields":[{"name":"a","path":"a","type":"date"}]}`,
		"bad regex":     `{"format":"text","fields":[{"name":"a","path":"("}]}`,
		"no group":      `{"format":"text","fields":[{"name":"a","path":"a+"}]}`,
		"two groups":    `{"format":"text","fields":[{"name":"a","path":"(a)(b)"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSchema([]byte(raw)); !errors.Is(err, ErrInvalidSchema) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStructuredHandBuiltSchemaAndConcurrency(t *testing.T) {
	schema := &Schema{Format: FormatText, Fields: []Field{{Name: "n", Path: `n=(\d+)`, Type: "int"}}}
	response := &Response{StatusCode: 200, Body: []byte("n=7")}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if got := structuredData(t, response, schema); got["n"] != 7.0 {
				t.Errorf("got %v", got)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if schema.compiled != nil {
		t.Fatal("caller's schema was mutated")
	}
}
