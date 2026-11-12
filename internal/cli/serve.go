package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/serve"
)

func serveCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "serve takes no arguments\n\n%s", usage)
		return 2
	}

	addr := opts.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		fmt.Fprintf(stderr, "--addr must be host:port, got %q\n\n%s", addr, usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = serve.Run(ctx, root, addr, version, func(bound string) {
		fmt.Fprintf(stdout, "repath serve listening on http://%s (read-only)\n", bound)
	}, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "serve: %v\n", err)
		if errors.Is(err, serve.ErrNoIndex) {
			return 3
		}
		return 1
	}
	return 0
}
