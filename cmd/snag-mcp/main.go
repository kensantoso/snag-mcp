// Command snag-mcp is an MCP server for shopping at Bunnings in person.
//
// It answers from the same public APIs the Bunnings website uses, with the
// guest token any anonymous visitor gets: no account, no key, no browser.
// Every answer is limited to stores near the user, so an assistant cannot
// suggest something that is only sold on the other side of the country.
//
// Transport is stdio: the MCP client launches this as a subprocess.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/geo"
	"github.com/kensantoso/snag-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.3.1"

func main() {
	var (
		configPath  = flag.String("config", tools.DefaultConfigPath(), "where the user's location is remembered")
		showConfig  = flag.Bool("config-path", false, "print the config location and exit")
		setLocation = flag.String("set-location", "", "save a postcode or suburb as the location and exit")
		concurrency = flag.Int("concurrency", 4, "most requests to Bunnings in flight at once")
	)
	flag.Parse()

	if *showConfig {
		fmt.Println(*configPath)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// More than a handful in flight is not ours to take from someone else's
	// website.
	if *concurrency < 1 || *concurrency > 8 {
		fatal("-concurrency must be between 1 and 8")
	}
	deps := &tools.Deps{API: bunnings.New(*concurrency), ConfigPath: *configPath}

	if *setLocation != "" {
		p, err := geo.Resolve(*setLocation)
		if err != nil {
			fatal("%v", err)
		}
		if err := deps.SaveLocation(p); err != nil {
			fatal("%v", err)
		}
		// stderr: stdout is reserved for the protocol, even in a one-shot.
		fmt.Fprintf(os.Stderr, "location saved: %s, in %s\n", p.Label(), *configPath)
		return
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "snag", Version: version}, nil)
	tools.Register(server, deps)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fatal("server: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "snag-mcp: "+format+"\n", args...)
	os.Exit(1)
}
