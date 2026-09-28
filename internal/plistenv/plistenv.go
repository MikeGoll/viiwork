// Package plistenv reads one variable from a macOS LaunchAgent plist's
// EnvironmentVariables. On a Mac installed by viiwork init, the mesh secret
// lives only there, so the commands that need it (join-code, update) read it
// from the node's agent instead of asking the user to export it.
package plistenv

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// Value is the value of name in a LaunchAgent plist's EnvironmentVariables.
func Value(data []byte, name string) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var inEnv bool
	var depth, envDepth int
	var key, lastKey string
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "dict":
				if lastKey == "EnvironmentVariables" && !inEnv {
					inEnv, envDepth = true, depth
				}
			case "key", "string":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", false
				}
				depth--
				if t.Name.Local == "key" {
					lastKey = strings.TrimSpace(text)
					key = lastKey
				} else if inEnv && depth == envDepth && key == name {
					return strings.TrimSpace(text), true
				}
				continue
			}
			lastKey = ""
		case xml.EndElement:
			if inEnv && depth == envDepth && t.Name.Local == "dict" {
				return "", false
			}
			depth--
		}
	}
}
