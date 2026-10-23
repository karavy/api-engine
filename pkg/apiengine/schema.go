package apiengine

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Formats supported by Schema.Format.
const (
	FormatXML   = "xml"
	FormatForm  = "form"
	FormatCSV   = "csv"
	FormatLines = "lines"
	FormatText  = "text"
)

const (
	maxSchemaFields = 100
	maxPathLength   = 256
	maxXMLDepth     = 64
)

var (
	ErrInvalidSchema  = errors.New("invalid schema")
	ErrSchemaMismatch = errors.New("response does not match schema")
	ErrSchemaRequired = errors.New("schema required for non-JSON body")
)

// Schema describes how to turn a non-JSON response body into structured data.
// It is plain data: the caller obtains the JSON from wherever it keeps its
// configuration and hands it to ParseSchema. A Schema is safe for concurrent
// use once built by ParseSchema and not modified.
//
// What Field.Path means depends on Format:
//
//	xml    slash-separated element path starting at the root element, with a
//	       final "@name" selecting an attribute ("order/@id", "order/status").
//	       The value of an element is its own trimmed text.
//	form   name of the key.
//	csv    name of a header column, or a 0-based column index. A scalar field
//	       reads the first data row; an array field reads the whole column.
//	lines  a 0-based line index, or a line prefix ("Status:"). A prefix yields
//	       the trimmed remainder of the first (or, for arrays, every) match.
//	text   regular expression (RE2) with exactly one capture group; a scalar
//	       field takes the first match, an array field every match.
type Schema struct {
	Format string  `json:"format"`
	Fields []Field `json:"fields"`

	compiled []*regexp.Regexp // text format only, parallel to Fields
}

// Field extracts one named value. Type is "string" (default), "int", "float"
// or "bool", optionally followed by "[]" for a list.
type Field struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// ParseSchema decodes and validates a JSON schema. Unknown keys are rejected.
func ParseSchema(raw []byte) (*Schema, error) {
	var schema Schema
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&schema); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSchema, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected data after schema", ErrInvalidSchema)
	}
	if err := schema.Validate(); err != nil {
		return nil, err
	}
	return &schema, nil
}

// Validate checks the schema and precompiles its regular expressions.
func (s *Schema) Validate() error {
	if s == nil {
		return fmt.Errorf("%w: nil schema", ErrInvalidSchema)
	}
	switch s.Format {
	case FormatXML, FormatForm, FormatCSV, FormatLines, FormatText:
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalidSchema, s.Format)
	}
	if len(s.Fields) == 0 || len(s.Fields) > maxSchemaFields {
		return fmt.Errorf("%w: need between 1 and %d fields", ErrInvalidSchema, maxSchemaFields)
	}
	names := make(map[string]bool, len(s.Fields))
	compiled := make([]*regexp.Regexp, len(s.Fields))
	for i, field := range s.Fields {
		if field.Name == "" || names[field.Name] {
			return fmt.Errorf("%w: field name %q is empty or duplicated", ErrInvalidSchema, field.Name)
		}
		names[field.Name] = true
		if field.Path == "" || len(field.Path) > maxPathLength {
			return fmt.Errorf("%w: field %q: path must be 1 to %d bytes", ErrInvalidSchema, field.Name, maxPathLength)
		}
		if _, _, err := splitType(field.Type); err != nil {
			return fmt.Errorf("%w: field %q: %w", ErrInvalidSchema, field.Name, err)
		}
		if s.Format == FormatText {
			expr, err := regexp.Compile(field.Path)
			if err != nil {
				return fmt.Errorf("%w: field %q: %w", ErrInvalidSchema, field.Name, err)
			}
			if expr.NumSubexp() != 1 {
				return fmt.Errorf("%w: field %q: regex needs exactly one capture group", ErrInvalidSchema, field.Name)
			}
			compiled[i] = expr
		}
	}
	s.compiled = compiled
	return nil
}

// Structured returns the response as JSON:
//
//	{"status":200,"success":true,"headers":{...},"data":...}
//
// With a nil schema the body must be valid JSON (or empty, giving null) and is
// embedded as is; any other body returns ErrSchemaRequired. With a schema,
// data is an object holding the declared fields. Do not use it on a partial
// response (Do returned an error).
func (r *Response) Structured(schema *Schema) ([]byte, error) {
	if r == nil {
		return nil, errors.New("structure response: nil response")
	}
	var data any
	if schema == nil {
		trimmed := bytes.TrimSpace(r.Body)
		switch {
		case len(trimmed) == 0:
			data = nil
		case json.Valid(trimmed):
			data = json.RawMessage(trimmed)
		default:
			return nil, ErrSchemaRequired
		}
	} else {
		extracted, err := schema.extract(r.Body)
		if err != nil {
			return nil, err
		}
		data = extracted
	}
	return json.Marshal(struct {
		Status  int         `json:"status"`
		Success bool        `json:"success"`
		Headers interface{} `json:"headers"`
		Data    any         `json:"data"`
	}{r.StatusCode, r.IsSuccess(), r.Headers, data})
}

func (s *Schema) extract(body []byte) (map[string]any, error) {
	compiled := s.compiled
	if len(compiled) != len(s.Fields) {
		// Built by hand rather than ParseSchema: validate on a copy so the
		// caller's value is never mutated by concurrent use.
		copied := &Schema{Format: s.Format, Fields: s.Fields}
		if err := copied.Validate(); err != nil {
			return nil, err
		}
		compiled = copied.compiled
	}
	var lookup func(index int, field Field) ([]string, error)
	switch s.Format {
	case FormatXML:
		root, err := parseXML(body)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSchemaMismatch, err)
		}
		lookup = func(_ int, field Field) ([]string, error) { return root.find(field.Path), nil }
	case FormatForm:
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSchemaMismatch, err)
		}
		lookup = func(_ int, field Field) ([]string, error) { return values[field.Path], nil }
	case FormatCSV:
		rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSchemaMismatch, err)
		}
		lookup = func(_ int, field Field) ([]string, error) { return csvColumn(rows, field.Path), nil }
	case FormatLines:
		lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
		lookup = func(_ int, field Field) ([]string, error) { return matchLines(lines, field.Path), nil }
	case FormatText:
		lookup = func(index int, _ Field) ([]string, error) {
			var found []string
			for _, match := range compiled[index].FindAllSubmatch(body, -1) {
				found = append(found, string(match[1]))
			}
			return found, nil
		}
	}
	result := make(map[string]any, len(s.Fields))
	for index, field := range s.Fields {
		raw, err := lookup(index, field)
		if err != nil {
			return nil, err
		}
		value, err := convert(field, raw)
		if err != nil {
			return nil, err
		}
		result[field.Name] = value
	}
	return result, nil
}

func splitType(name string) (base string, list bool, err error) {
	if name == "" {
		name = "string"
	}
	base = strings.TrimSuffix(name, "[]")
	list = base != name
	switch base {
	case "string", "int", "float", "bool":
		return base, list, nil
	}
	return "", false, fmt.Errorf("unsupported type %q", name)
}

// convert returns nil for an absent optional field.
func convert(field Field, raw []string) (any, error) {
	base, list, _ := splitType(field.Type)
	if len(raw) == 0 {
		if field.Required {
			return nil, fmt.Errorf("%w: required field %q not found", ErrSchemaMismatch, field.Name)
		}
		return nil, nil
	}
	if !list {
		raw = raw[:1]
	}
	values := make([]any, len(raw))
	for i, text := range raw {
		value, err := convertScalar(base, text)
		if err != nil {
			return nil, fmt.Errorf("%w: field %q: %w", ErrSchemaMismatch, field.Name, err)
		}
		values[i] = value
	}
	if !list {
		return values[0], nil
	}
	return values, nil
}

func convertScalar(base, text string) (any, error) {
	switch base {
	case "int":
		return strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	case "float":
		value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err == nil && (math.IsNaN(value) || math.IsInf(value, 0)) {
			err = errors.New("value is not a finite number")
		}
		return value, err
	case "bool":
		return strconv.ParseBool(strings.TrimSpace(text))
	}
	return text, nil
}

func csvColumn(rows [][]string, path string) []string {
	if len(rows) == 0 {
		return nil
	}
	column := -1
	for i, name := range rows[0] {
		if name == path {
			column = i
			break
		}
	}
	if column < 0 {
		index, err := strconv.Atoi(path)
		if err != nil || index < 0 {
			return nil
		}
		column = index
	}
	var values []string
	for _, row := range rows[1:] {
		if column < len(row) {
			values = append(values, row[column])
		}
	}
	return values
}

func matchLines(lines []string, path string) []string {
	if index, err := strconv.Atoi(path); err == nil {
		if index >= 0 && index < len(lines) {
			return []string{lines[index]}
		}
		return nil
	}
	var values []string
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, path); ok {
			values = append(values, strings.TrimSpace(rest))
		}
	}
	return values
}

type xmlNode struct {
	name     string
	attrs    map[string]string
	text     strings.Builder
	children []*xmlNode
}

func parseXML(body []byte) (*xmlNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var stack []*xmlNode
	var root *xmlNode
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			if len(stack) >= maxXMLDepth {
				return nil, errors.New("XML nesting too deep")
			}
			node := &xmlNode{name: t.Name.Local, attrs: make(map[string]string, len(t.Attr))}
			for _, attr := range t.Attr {
				node.attrs[attr.Name.Local] = attr.Value
			}
			if len(stack) == 0 {
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("no XML root element")
	}
	return root, nil
}

// find resolves "root/child/@attr" and returns every matching value.
func (root *xmlNode) find(path string) []string {
	segments := strings.Split(path, "/")
	attribute := ""
	if last := segments[len(segments)-1]; strings.HasPrefix(last, "@") {
		attribute = last[1:]
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 || segments[0] != root.name {
		return nil
	}
	nodes := []*xmlNode{root}
	for _, segment := range segments[1:] {
		var next []*xmlNode
		for _, node := range nodes {
			for _, child := range node.children {
				if child.name == segment {
					next = append(next, child)
				}
			}
		}
		nodes = next
	}
	var values []string
	for _, node := range nodes {
		if attribute != "" {
			if value, ok := node.attrs[attribute]; ok {
				values = append(values, value)
			}
		} else {
			values = append(values, strings.TrimSpace(node.text.String()))
		}
	}
	return values
}
