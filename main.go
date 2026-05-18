package main

import (
	"fmt"
	"os"

	"github.com/cli/go-gh/v2/pkg/api"
)

func main() {
	client, err := api.DefaultRESTClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth: %v\n", err)
		os.Exit(1)
	}

	var user struct {
		Login string `json:"login"`
	}
	if err := client.Get("user", &user); err != nil {
		fmt.Fprintf(os.Stderr, "GET /user: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("logged in as %s\n", user.Login)
}
