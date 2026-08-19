// Command genba is the command line client.
//
// It talks to a running server over the same HTTP API a browser uses, which
// keeps it honest: anything this client can do is something an integration can
// do, and anything it cannot see is something the API does not expose.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tamnd/genba"
)

func main() {
	os.Exit(cli())
}

// cli is main with a return value, so that the signal handler is unregistered on
// every path out. os.Exit does not run deferred calls, which is why the work is
// not done in main itself.
func cli() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "genba:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "search":
		return search(ctx, rest, getenv, stdout, stderr)
	case "health":
		return health(ctx, rest, getenv, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "genba %s (%s, built %s)\n", genba.Version, genba.Commit, genba.Date)
		return nil
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `genba is the command line client for a genba server.

Usage:
  genba <command> [flags]

Commands:
  search    run a query and print the results
  health    check that a server is up
  version   print the version

Run genba <command> -h for the flags of a command.

The server address comes from GENBA_SERVER and defaults to http://127.0.0.1:8080.
Credentials come from GENBA_SUBJECT, GENBA_TENANT and GENBA_GROUPS.
`)
}

// client is the small amount of HTTP this command needs.
type client struct {
	base    string
	subject string
	tenant  string
	groups  string
	http    *http.Client
}

func newClient(getenv func(string) string) *client {
	base := getenv("GENBA_SERVER")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return &client{
		base:    strings.TrimRight(base, "/"),
		subject: getenv("GENBA_SUBJECT"),
		tenant:  getenv("GENBA_TENANT"),
		groups:  getenv("GENBA_GROUPS"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.subject != "" {
		req.Header.Set("X-Genba-Subject", c.subject)
	}
	if c.tenant != "" {
		req.Header.Set("X-Genba-Tenant", c.tenant)
	}
	if c.groups != "" {
		req.Header.Set("X-Genba-Groups", c.groups)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("the server rejected the credentials, set GENBA_SUBJECT")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("the server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

type searchResponse struct {
	Total int `json:"total"`
	Hits  []struct {
		ID      string  `json:"id"`
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Source  string  `json:"source"`
		Kind    string  `json:"kind"`
		Snippet string  `json:"snippet"`
		Score   float64 `json:"score"`
	} `json:"hits"`
}

func search(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("genba search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 10, "how many results to print")
	source := fs.String("source", "", "restrict to these sources, comma separated")
	asJSON := fs.Bool("json", false, "print the raw response")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(query) == "" {
		return errors.New("no query given")
	}

	values := url.Values{"q": {query}, "limit": {strconv.Itoa(*limit)}}
	if *source != "" {
		values.Set("source", *source)
	}

	var res searchResponse
	if err := newClient(getenv).get(ctx, "/api/v1/search", values, &res); err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if res.Total == 0 {
		fmt.Fprintln(stdout, "no results")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	for _, h := range res.Hits {
		title := h.Title
		if title == "" {
			title = h.ID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", title, h.Source, h.URL)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n%d of %d results\n", len(res.Hits), res.Total)
	return nil
}

func health(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("genba health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var res struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Uptime  string `json:"uptime"`
	}
	if err := newClient(getenv).get(ctx, "/healthz", nil, &res); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s, version %s, up %s\n", res.Status, res.Version, res.Uptime)
	return nil
}
