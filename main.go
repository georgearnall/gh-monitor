package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/georgearnall/gha-monitor/internal/discovery"
)

func main() {
	fs := flag.NewFlagSet("gha-monitor", flag.ExitOnError)
	maxRepos := fs.Int("max-repos", 20, "maximum number of repos to monitor")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gha-monitor [flags] [list-repos]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !startsWithDash(args[0]) {
		cmd = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	client, err := api.DefaultRESTClient()
	if err != nil {
		fail("auth: %v", err)
	}

	switch cmd {
	case "", "watch":
		runWatch(client, *maxRepos)
	case "list-repos":
		runListRepos(client, *maxRepos)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n", cmd)
		os.Exit(2)
	}
}

func runListRepos(client *api.RESTClient, maxRepos int) {
	repos, err := discovery.Discover(client, maxRepos)
	if err != nil {
		fail("discover: %v", err)
	}
	for _, r := range repos {
		fmt.Printf("%-50s %s\n", r.FullName, r.Activity.Format("2006-01-02 15:04"))
	}
}

func runWatch(client *api.RESTClient, maxRepos int) {
	var user struct {
		Login string `json:"login"`
	}
	if err := client.Get("user", &user); err != nil {
		fail("GET /user: %v", err)
	}
	fmt.Printf("logged in as %s (max %d repos)\n", user.Login, maxRepos)
	fmt.Println("watch loop not implemented yet — run `gha-monitor list-repos` to see discovered set")
}

func startsWithDash(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
