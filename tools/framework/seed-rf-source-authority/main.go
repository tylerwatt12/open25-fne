package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"open25.local/open25-fne/internal/framework"
)

func main() {
	data := flag.String("zone-data", "", "stopped Zone data directory")
	zone := flag.String("zone-id", "", "configured Zone identity")
	output := flag.String("output", "", "new private backend authority file")
	flag.Parse()
	if *data == "" || *zone == "" || *output == "" {
		flag.Usage()
		os.Exit(2)
	}
	receipt, err := framework.SeedRFSourceAuthority(context.Background(), *data, *zone, *output)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(receipt)
}
