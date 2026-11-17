package main

// Small helpers that reproduce the JavaScript semantics the TypeScript
// sources rely on (String(), Number(), parseInt, lenient base64, ISO
// timestamps), so the Go port answers byte-for-byte the way the original
// did wherever the wire can observe a difference.

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// jsString mirrors JS String(value) for the JSON-shaped values that flow
// through arguments and request bodies.
func jsString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case json.Number:
		f, _ := t.Float64()
		return jsNumberString(f)
	default:
		return ""
	}
}

// jsNumberString formats a float64 the way JS prints a Number: integers
// without a decimal point, everything else in shortest round-trip form.
func jsNumberString(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsNumber mirrors JS Number(value): ok=false stands in for NaN.
func jsNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	default:
		return 0, false
	}
}

// jsParseInt mirrors JS parseInt(text, 10): leading whitespace and sign,
// then a run of digits; ok=false stands in for NaN.
func jsParseInt(text string) (int64, bool) {
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r' || text[i] == '\v' || text[i] == '\f') {
		i++
	}
	neg := false
	if i < len(text) && (text[i] == '+' || text[i] == '-') {
		neg = text[i] == '-'
		i++
	}
	start := i
	var value int64
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		value = value*10 + int64(text[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		value = -value
	}
	return value, true
}

// lenientBase64Decode mirrors Node's Buffer.from(text, 'base64'), which
// ignores characters outside the alphabet and tolerates missing padding.
func lenientBase64Decode(text string) []byte {
	var cleaned []byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned)%4 == 1 {
		cleaned = cleaned[:len(cleaned)-1]
	}
	for len(cleaned)%4 != 0 {
		cleaned = append(cleaned, '=')
	}
	out, err := base64.StdEncoding.DecodeString(string(cleaned))
	if err != nil {
		return []byte{}
	}
	return out
}

// lenientBase64ToString decodes base64 the same lenient way and returns the
// bytes as a string (used for GraphQL cursors and global ids).
func lenientBase64ToString(text string) string {
	return string(lenientBase64Decode(text))
}

// isoFromUnixSeconds renders a Unix timestamp the way Date.toISOString does.
func isoFromUnixSeconds(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// isoNow renders the current time the way Date.toISOString does.
func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
