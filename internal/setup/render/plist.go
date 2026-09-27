package render

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"text/template"

	"github.com/janit/viiwork/v2/internal/config"
)

// PlistInput is the LaunchAgent's inputs. Every path is absolute: launchd
// expands neither ~ nor $HOME.
type PlistInput struct {
	Binary, Config, Log string
	Secret              []byte // nil for an open mesh
}

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"x": escape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Written by viiwork init; ` + "`viiwork uninstall`" + ` removes it. Mode 0600: it holds the
  mesh secret in a secured mesh.

  Reload the config:  launchctl kill HUP gui/$(id -u)/fi.viiwork.node
  Restart:            launchctl kickstart -k gui/$(id -u)/fi.viiwork.node
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>fi.viiwork.node</string>

  <key>ProgramArguments</key>
  <array>
    <string>{{x .Binary}}</string>
    <string>--config</string>
    <string>{{x .Config}}</string>
  </array>

  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/bin:/bin:/usr/sbin:/sbin</string>
{{- if .Secret}}
    <key>{{.SecretEnv}}</key>
    <string>{{.Secret}}</string>
{{- end}}
  </dict>

  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>

  <!-- The node leaves the mesh and drains for up to about 75 s. -->
  <key>ExitTimeOut</key>
  <integer>90</integer>

  <key>StandardOutPath</key>
  <string>{{x .Log}}</string>
  <key>StandardErrorPath</key>
  <string>{{x .Log}}</string>
</dict>
</plist>
`))

func escape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Plist renders the node's LaunchAgent.
func Plist(in PlistInput) []byte {
	data := struct {
		Binary, Config, Log, SecretEnv, Secret string
	}{Binary: in.Binary, Config: in.Config, Log: in.Log, SecretEnv: config.DefaultMeshSecretEnv}
	if in.Secret != nil {
		data.Secret = base64.StdEncoding.EncodeToString(in.Secret) // base64: nothing to escape
	}
	var b bytes.Buffer
	plistTmpl.Execute(&b, data)
	return b.Bytes()
}
