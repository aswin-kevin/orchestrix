package tool

import (
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"
)

var (
	rawMessageType    = reflect.TypeOf(json.RawMessage(nil))
	timeType          = reflect.TypeFor[time.Time]()
	byteSliceType     = reflect.TypeFor[[]byte]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	schemaHookType    = reflect.TypeFor[interface{ JSONSchema() map[string]any }]()
)

// schemaHook returns the schema a type declares through a JSONSchema method
// on the type or its pointer, called on the zero value, as a shallow copy so
// callers may add top-level keys. A method returning nil yields {}.
func schemaHook(t reflect.Type) (map[string]any, bool) {
	var h interface{ JSONSchema() map[string]any }
	switch {
	case t.Kind() != reflect.Pointer && t.Implements(schemaHookType):
		h = reflect.Zero(t).Interface().(interface{ JSONSchema() map[string]any })
	case reflect.PointerTo(t).Implements(schemaHookType):
		h = reflect.New(t).Interface().(interface{ JSONSchema() map[string]any })
	default:
		return nil, false
	}
	s := maps.Clone(h.JSONSchema())
	if s == nil {
		s = map[string]any{}
	}
	return s, true
}

// SchemaFor generates the JSON schema of the struct type T from its fields:
//
//   - the property name comes from the `json` tag (`json:"-"` skips the
//     field);
//   - `desc:"..."` becomes the description and `enum:"a,b"` the enum list;
//   - a field is required unless it is a pointer or tagged omitempty;
//   - embedded structs without a json name are flattened into their parent.
//
// A type may declare its own schema: when the type or its pointer has a
// JSONSchema() map[string]any method, its result for the zero value is used
// as is (a shallow copy is taken; nil means {}). Otherwise time.Time is a
// date-time string, []byte is a base64 string, and any
// encoding.TextMarshaler is a string; the enum tag may narrow the latter. The
// desc and enum tags apply on top.
//
// It panics when T declares a schema whose "type" is not "object" (tool input
// is always an object), when T declares none and is not a struct, or when T
// contains a kind JSON cannot express (chan, func, complex), naming the
// offending field path, because that is a programming error.
func SchemaFor[T any]() map[string]any {
	t := reflect.TypeFor[T]()
	if s, ok := schemaHook(t); ok {
		if s["type"] != "object" {
			panic(fmt.Sprintf("tool: SchemaFor: %s's JSONSchema is not an object schema", t))
		}
		return s
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("tool: SchemaFor: %s is not a struct", t))
	}
	return objectSchema(t, "", nil)
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"required":             []string{},
		"additionalProperties": false,
	}
}

// objectSchema builds the schema of struct type t. path locates t inside the
// root type for panic messages; stack holds the struct types being expanded so
// that self-referential types are reported instead of recursing forever.
func objectSchema(t reflect.Type, path string, stack []reflect.Type) map[string]any {
	for _, seen := range stack {
		if seen == t {
			panic(fmt.Sprintf("tool: SchemaFor: recursive type %s at %q", t, path))
		}
	}
	stack = append(stack, t)

	schema := emptyObjectSchema()
	addFields(schema, t, path, stack)
	return schema
}

func addFields(schema map[string]any, t reflect.Type, path string, stack []reflect.Type) {
	props := schema["properties"].(map[string]any)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, omitEmpty, skip := parseJSONTag(f)
		if skip {
			continue
		}
		fieldPath := joinPath(path, name)

		if f.Anonymous && !hasJSONName(f) {
			if et := derefType(f.Type); et.Kind() == reflect.Struct {
				addFields(schema, et, path, stack)
				continue
			}
		}

		props[name] = fieldSchema(f, fieldPath, stack)
		if !omitEmpty && f.Type.Kind() != reflect.Pointer {
			schema["required"] = append(schema["required"].([]string), name)
		}
	}
}

// parseJSONTag returns the JSON property name of f and whether the field is
// optional (omitempty) or excluded (unexported or `json:"-"`).
func parseJSONTag(f reflect.StructField) (name string, omitEmpty, skip bool) {
	if !f.IsExported() && !f.Anonymous {
		return "", false, true
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	tagName, opts, _ := strings.Cut(tag, ",")
	if tagName == "" {
		tagName = f.Name
	}
	return tagName, strings.Contains(","+opts+",", ",omitempty,"), false
}

func hasJSONName(f reflect.StructField) bool {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name != ""
}

func fieldSchema(f reflect.StructField, path string, stack []reflect.Type) map[string]any {
	s := typeSchema(f.Type, path, stack)
	if desc := f.Tag.Get("desc"); desc != "" {
		s["description"] = desc
	}
	if enum := f.Tag.Get("enum"); enum != "" {
		if s["type"] != "string" {
			panic(fmt.Sprintf("tool: SchemaFor: field %q: enum is only supported on strings", path))
		}
		s["enum"] = splitEnum(enum)
	}
	return s
}

func splitEnum(tag string) []any {
	var values []any
	for _, v := range strings.Split(tag, ",") {
		values = append(values, strings.TrimSpace(v))
	}
	return values
}

// typeSchema maps a Go type to its JSON schema. Types with no fixed JSON shape
// (json.RawMessage, interfaces) yield an empty schema that accepts anything.
func typeSchema(t reflect.Type, path string, stack []reflect.Type) map[string]any {
	if t == rawMessageType {
		return map[string]any{}
	}
	if s, ok := schemaHook(t); ok {
		return s
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == byteSliceType:
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case t.Kind() != reflect.Pointer && (t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)):
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return typeSchema(t.Elem(), path, stack)
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem(), path+"[]", stack)}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.Struct:
		return objectSchema(t, path, stack)
	case reflect.Interface:
		return map[string]any{}
	default:
		panic(fmt.Sprintf("tool: SchemaFor: field %q has unsupported kind %s", path, t.Kind()))
	}
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
