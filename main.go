// Command gh-jira-sync creates Jira issues from GitHub issues and syncs comments both ways.
package main

import (
	"os"

	"github.com/cccteam/gh-jira-sync/internal/ghjira"
)

func main() {
	os.Exit(ghjira.Main(os.Args[1:], os.Stdout))
}
