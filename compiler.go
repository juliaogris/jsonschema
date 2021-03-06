// Copyright 2017 Santhosh Kumar Tekuri. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jsonschema

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/formats"
	"github.com/santhosh-tekuri/jsonschema/loader"
)

// A Draft represents json-schema draft
type Draft struct {
	meta *Schema
	id   string
}

var latest = Draft6

func (draft *Draft) validateSchema(url, ptr string, v interface{}) error {
	if meta := draft.meta; meta != nil {
		if err := meta.validate(v); err != nil {
			addContext(ptr, "", err)
			finishSchemaContext(err, meta)
			finishInstanceContext(err)
			var instancePtr string
			if ptr == "" {
				instancePtr = "#"
			} else {
				instancePtr = "#/" + ptr
			}
			return &SchemaError{
				url,
				&ValidationError{
					Message:     fmt.Sprintf("doesn't validate with %q", meta.URL+meta.Ptr),
					InstancePtr: instancePtr,
					SchemaURL:   meta.URL,
					SchemaPtr:   "#",
					Causes:      []*ValidationError{err.(*ValidationError)},
				},
			}
		}
	}
	return nil
}

// A Compiler represents a json-schema compiler.
//
// Currently draft4 and draft6 are supported
type Compiler struct {
	// Draft represents the draft used when '$schema' attribute is missing.
	//
	// This defaults to latest draft (currently draft6).
	Draft     *Draft
	resources map[string]*resource
}

// NewCompiler returns a draft4 json-schema Compiler object.
func NewCompiler() *Compiler {
	return &Compiler{resources: make(map[string]*resource)}
}

// AddResource adds in-memory resource to the compiler.
//
// Note that url must not have fragment
func (c *Compiler) AddResource(url string, r io.Reader) error {
	res, err := newResource(url, r)
	if err != nil {
		return err
	}
	c.resources[res.url] = res
	return nil
}

func (c *Compiler) draft(v interface{}) (*Draft, error) {
	if m, ok := v.(map[string]interface{}); ok {
		if url, ok := m["$schema"]; ok {
			switch url {
			case "http://json-schema.org/schema#":
				return latest, nil
			case "http://json-schema.org/draft-06/schema#":
				return Draft6, nil
			case "http://json-schema.org/draft-04/schema#":
				return Draft4, nil
			default:
				return nil, fmt.Errorf("unknown $schema %q", url)
			}
		}
	}
	if c.Draft == nil {
		return latest, nil
	}
	return c.Draft, nil
}

// MustCompile is like Compile but panics if the url cannot be compiled to *Schema.
// It simplifies safe initialization of global variables holding compiled Schemas.
func (c *Compiler) MustCompile(url string) *Schema {
	s, err := c.Compile(url)
	if err != nil {
		panic(fmt.Sprintf("jsonschema: Compile(%q): %s", url, err))
	}
	return s
}

// Compile parses json-schema at given url returns, if successful,
// a Schema object that can be used to match against json.
func (c *Compiler) Compile(url string) (*Schema, error) {
	base, fragment := split(url)
	if _, ok := c.resources[base]; !ok {
		r, err := loader.Load(base)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		if err := c.AddResource(base, r); err != nil {
			return nil, err
		}
	}
	r := c.resources[base]
	return c.compileRef(nil, r, nil, r.url, fragment)
}

func (c Compiler) compileRef(draft *Draft, r *resource, root map[string]interface{}, base, ref string) (*Schema, error) {
	var err error
	if rootFragment(ref) {
		if _, ok := r.schemas["#"]; !ok {
			if draft == nil {
				draft, err = c.draft(r.doc)
				if err != nil {
					return nil, err
				}
			}
			if err := draft.validateSchema(r.url, "", r.doc); err != nil {
				return nil, err
			}
			s := &Schema{URL: r.url, Ptr: "#"}
			r.schemas["#"] = s
			if m, ok := r.doc.(map[string]interface{}); ok {
				if _, err := c.compile(draft, r, s, base, m, m); err != nil {
					return nil, err
				}
			} else {
				if _, err := c.compile(draft, r, s, base, nil, r.doc); err != nil {
					return nil, err
				}
			}
		}
		return r.schemas["#"], nil
	}

	if strings.HasPrefix(ref, "#/") {
		if _, ok := r.schemas[ref]; !ok {
			docDraft := draft
			if docDraft == nil {
				docDraft = c.Draft
			}
			if docDraft == nil {
				docDraft = latest
			}
			ptrBase, doc, err := r.resolvePtr(docDraft, ref)
			if err != nil {
				return nil, err
			}
			if draft == nil {
				draft, err = c.draft(doc)
				if err != nil {
					return nil, err
				}
			}
			if err := draft.validateSchema(r.url, strings.TrimPrefix(ref, "#/"), doc); err != nil {
				return nil, err
			}
			r.schemas[ref] = &Schema{URL: base, Ptr: ref}
			if _, err := c.compile(draft, r, r.schemas[ref], ptrBase, root, doc); err != nil {
				return nil, err
			}
		}
		return r.schemas[ref], nil
	}

	refURL, err := resolveURL(base, ref)
	if err != nil {
		return nil, err
	}
	if rs, ok := r.schemas[refURL]; ok {
		return rs, nil
	}

	ids := make(map[string]map[string]interface{})
	if err := resolveIDs(draft, r.url, root, ids); err != nil {
		return nil, err
	}
	if v, ok := ids[refURL]; ok {
		if err := draft.validateSchema(r.url, "", v); err != nil {
			return nil, err
		}
		u, f := split(refURL)
		s := &Schema{URL: u, Ptr: f}
		r.schemas[refURL] = s
		if err := c.compileMap(draft, r, s, refURL, root, v); err != nil {
			return nil, err
		}
		return s, nil
	}

	base, _ = split(refURL)
	if base == r.url {
		return nil, fmt.Errorf("invalid ref: %q", refURL)
	}
	return c.Compile(refURL)
}

func (c Compiler) compile(draft *Draft, r *resource, s *Schema, base string, root map[string]interface{}, m interface{}) (*Schema, error) {
	if s == nil {
		s = new(Schema)
		s.URL, _ = split(base)
	}
	switch m := m.(type) {
	case bool:
		s.always = &m
		return s, nil
	default:
		return s, c.compileMap(draft, r, s, base, root, m.(map[string]interface{}))
	}
}

func (c Compiler) compileMap(draft *Draft, r *resource, s *Schema, base string, root, m map[string]interface{}) error {
	var err error

	if id, ok := m[draft.id]; ok {
		if base, err = resolveURL(base, id.(string)); err != nil {
			return err
		}
	}

	if ref, ok := m["$ref"]; ok {
		b, _ := split(base)
		s.Ref, err = c.compileRef(draft, r, root, b, ref.(string))
		if err != nil {
			return err
		}
		// All other properties in a "$ref" object MUST be ignored
		return nil
	}

	if t, ok := m["type"]; ok {
		switch t := t.(type) {
		case string:
			s.Types = []string{t}
		case []interface{}:
			s.Types = toStrings(t)
		}
	}

	if e, ok := m["enum"]; ok {
		s.Enum = e.([]interface{})
		allPrimitives := true
		for _, item := range s.Enum {
			switch jsonType(item) {
			case "object", "array":
				allPrimitives = false
				break
			}
		}
		s.enumError = "enum failed"
		if allPrimitives {
			if len(s.Enum) == 1 {
				s.enumError = fmt.Sprintf("value must be %#v", s.Enum[0])
			} else {
				strEnum := make([]string, len(s.Enum))
				for i, item := range s.Enum {
					strEnum[i] = fmt.Sprintf("%#v", item)
				}
				s.enumError = fmt.Sprintf("value must be one of %s", strings.Join(strEnum, ", "))
			}
		}
	}

	if not, ok := m["not"]; ok {
		s.Not, err = c.compile(draft, r, nil, base, root, not)
		if err != nil {
			return err
		}
	}

	loadSchemas := func(pname string) ([]*Schema, error) {
		if pvalue, ok := m[pname]; ok {
			pvalue := pvalue.([]interface{})
			schemas := make([]*Schema, len(pvalue))
			for i, v := range pvalue {
				sch, err := c.compile(draft, r, nil, base, root, v)
				if err != nil {
					return nil, err
				}
				schemas[i] = sch
			}
			return schemas, nil
		}
		return nil, nil
	}
	if s.AllOf, err = loadSchemas("allOf"); err != nil {
		return err
	}
	if s.AnyOf, err = loadSchemas("anyOf"); err != nil {
		return err
	}
	if s.OneOf, err = loadSchemas("oneOf"); err != nil {
		return err
	}

	loadInt := func(pname string) int {
		if num, ok := m[pname]; ok {
			i, _ := num.(json.Number).Int64()
			return int(i)
		}
		return -1
	}
	s.MinProperties, s.MaxProperties = loadInt("minProperties"), loadInt("maxProperties")

	if req, ok := m["required"]; ok {
		s.Required = toStrings(req.([]interface{}))
	}

	if props, ok := m["properties"]; ok {
		props := props.(map[string]interface{})
		s.Properties = make(map[string]*Schema, len(props))
		for pname, pmap := range props {
			s.Properties[pname], err = c.compile(draft, r, nil, base, root, pmap)
			if err != nil {
				return err
			}
		}
	}

	if regexProps, ok := m["regexProperties"]; ok {
		s.RegexProperties = regexProps.(bool)
	}

	if patternProps, ok := m["patternProperties"]; ok {
		patternProps := patternProps.(map[string]interface{})
		s.PatternProperties = make(map[*regexp.Regexp]*Schema, len(patternProps))
		for pattern, pmap := range patternProps {
			s.PatternProperties[regexp.MustCompile(pattern)], err = c.compile(draft, r, nil, base, root, pmap)
			if err != nil {
				return err
			}
		}
	}

	if additionalProps, ok := m["additionalProperties"]; ok {
		switch additionalProps := additionalProps.(type) {
		case bool:
			if !additionalProps {
				s.AdditionalProperties = false
			}
		case map[string]interface{}:
			s.AdditionalProperties, err = c.compile(draft, r, nil, base, root, additionalProps)
			if err != nil {
				return err
			}
		}
	}

	if deps, ok := m["dependencies"]; ok {
		deps := deps.(map[string]interface{})
		s.Dependencies = make(map[string]interface{}, len(deps))
		for pname, pvalue := range deps {
			switch pvalue := pvalue.(type) {
			case []interface{}:
				s.Dependencies[pname] = toStrings(pvalue)
			default:
				s.Dependencies[pname], err = c.compile(draft, r, nil, base, root, pvalue)
				if err != nil {
					return err
				}
			}
		}
	}

	s.MinItems, s.MaxItems = loadInt("minItems"), loadInt("maxItems")

	if unique, ok := m["uniqueItems"]; ok {
		s.UniqueItems = unique.(bool)
	}

	if items, ok := m["items"]; ok {
		switch items := items.(type) {
		case []interface{}:
			s.Items, err = loadSchemas("items")
			if err != nil {
				return err
			}
			if additionalItems, ok := m["additionalItems"]; ok {
				switch additionalItems := additionalItems.(type) {
				case bool:
					s.AdditionalItems = additionalItems
				case map[string]interface{}:
					s.AdditionalItems, err = c.compile(draft, r, nil, base, root, additionalItems)
					if err != nil {
						return err
					}
				}
			} else {
				s.AdditionalItems = true
			}
		default:
			s.Items, err = c.compile(draft, r, nil, base, root, items)
			if err != nil {
				return err
			}
		}
	}

	s.MinLength, s.MaxLength = loadInt("minLength"), loadInt("maxLength")

	if pattern, ok := m["pattern"]; ok {
		s.Pattern = regexp.MustCompile(pattern.(string))
	}

	if format, ok := m["format"]; ok {
		s.FormatName = format.(string)
		s.format, _ = formats.Get(s.FormatName)
	}

	loadFloat := func(pname string) *big.Float {
		if num, ok := m[pname]; ok {
			r, _ := new(big.Float).SetString(string(num.(json.Number)))
			return r
		}
		return nil
	}

	s.Minimum = loadFloat("minimum")
	if exclusive, ok := m["exclusiveMinimum"]; ok {
		if exclusive, ok := exclusive.(bool); ok {
			if exclusive {
				s.Minimum, s.ExclusiveMinimum = nil, s.Minimum
			}
		} else {
			s.ExclusiveMinimum = loadFloat("exclusiveMinimum")
		}
	}

	s.Maximum = loadFloat("maximum")
	if exclusive, ok := m["exclusiveMaximum"]; ok {
		if exclusive, ok := exclusive.(bool); ok {
			if exclusive {
				s.Maximum, s.ExclusiveMaximum = nil, s.Maximum
			}
		} else {
			s.ExclusiveMaximum = loadFloat("exclusiveMaximum")
		}
	}

	s.MultipleOf = loadFloat("multipleOf")

	if draft == Draft6 {
		if c, ok := m["const"]; ok {
			s.constant = []interface{}{c}
		}
		if propertyNames, ok := m["propertyNames"]; ok {
			s.propertyNames, err = c.compile(draft, r, nil, base, root, propertyNames)
			if err != nil {
				return err
			}
		}
		if contains, ok := m["contains"]; ok {
			s.contains, err = c.compile(draft, r, nil, base, root, contains)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func toStrings(arr []interface{}) []string {
	s := make([]string, len(arr))
	for i, v := range arr {
		s[i] = v.(string)
	}
	return s
}
