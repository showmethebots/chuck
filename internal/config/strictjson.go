package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// checkJSONKeys closes two gaps DisallowUnknownFields leaves open:
// encoding/json matches keys to fields case-insensitively, and a repeated
// key silently overrides the first. Either lets a loop.json diff look like
// it keeps "auto_merge": false while a later "Auto_Merge": true wins, at
// the one place a human's review is the control. So every object key must
// appear once, and a key of a struct-shaped object must be its json tag
// byte for byte.
func checkJSONKeys(b []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var p problems
	if err := walkJSON(dec, t, "", &p); err != nil {
		return err
	}
	return p.err()
}

func walkJSON(dec *json.Decoder, t reflect.Type, at string, p *problems) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch delim {
	case '[':
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for i := 0; dec.More(); i++ {
			if err := walkJSON(dec, elem, fmt.Sprintf("%s[%d]", at, i), p); err != nil {
				return err
			}
		}
	case '{':
		fields := map[string]reflect.Type(nil)
		if t != nil && t.Kind() == reflect.Struct {
			fields = jsonFields(t)
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, ok := kt.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			field := strings.TrimPrefix(at+"."+k, ".")
			if seen[k] {
				p.add("%s: key appears more than once", field)
			}
			seen[k] = true
			var child reflect.Type
			switch {
			case fields != nil:
				ft, exact := fields[k]
				if !exact {
					p.add("%s: unknown field (keys are case-sensitive)", field)
				}
				child = ft
			case t != nil && t.Kind() == reflect.Map:
				child = t.Elem()
			}
			if err := walkJSON(dec, child, field, p); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // closing delimiter
	return err
}

func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}
