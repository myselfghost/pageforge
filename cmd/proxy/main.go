// Command proxy builds the PageForge data plane as a custom Caddy binary.
// It imports the standard Caddy modules plus the s3static handler module.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/pageforge/pageforge/caddy"
)

func main() {
	caddycmd.Main()
}
