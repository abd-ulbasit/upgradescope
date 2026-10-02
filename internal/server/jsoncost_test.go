package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

// tokenCost counts a valid JSON text's values with encoding/json's own
// tokenizer; ok is false when it is not valid JSON.
func tokenCost(text []byte) (c jsonCost, ok bool) {
	if !json.Valid(text) {
		return c, false
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			return c, true
		}
		switch tok {
		case json.Delim('{'):
			c.objects++
		case json.Delim('}'), json.Delim(']'):
		default:
			c.values++ // '[', and every scalar, object keys included
		}
	}
}

// checkJSONCost fails when the meter's count of a valid JSON text differs
// from encoding/json's, whole or fed in small pieces.
func checkJSONCost(t *testing.T, text []byte) {
	t.Helper()
	want, ok := tokenCost(text)
	if !ok {
		return
	}
	var whole, pieces jsonMeter
	whole.feed(text)
	for p := text; len(p) > 0; p = p[min(len(p), 3):] {
		pieces.feed(p[:min(len(p), 3)])
	}
	if whole.cost != want || pieces.cost != want {
		t.Errorf("measured %+v (in pieces %+v), encoding/json reads %+v\n%q", whole.cost, pieces.cost, want, text)
	}
}

var jsonSeeds = []string{
	`{}`, `[]`, `null`, `"a"`, `-1.5e+10`, `true`,
	`{"a":1,"b":[true,false,null],"c":{"d":"e"}}`,
	`{"a\"b":"c\\","d":"é{[,]}"}`,
	`[{},{},[[]],"",0]`,
	" {\n \"a\" : [ 1 , 2 ] }\n",
}

func TestJSONMeterIsExact(t *testing.T) {
	for _, s := range jsonSeeds {
		checkJSONCost(t, []byte(s))
	}
	for _, shape := range ingestHeapShapes() {
		checkJSONCost(t, []byte(shape(16<<10)))
	}
}

func FuzzJSONMeterIsExact(f *testing.F) {
	for _, s := range jsonSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { checkJSONCost(t, []byte(s)) })
}
