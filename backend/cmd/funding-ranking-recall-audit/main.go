package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"selfquant/backend/internal/funding/ranking"
)

func main() {
	fullPath := flag.String("full", "", "full-universe ranking JSON file")
	candidatePath := flag.String("candidate", "", "candidate ranking JSON file")
	flag.Parse()
	if *fullPath == "" || *candidatePath == "" {
		fmt.Fprintln(os.Stderr, "-full and -candidate are required")
		os.Exit(2)
	}
	full, err := readOpportunities(*fullPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	candidate, err := readOpportunities(*candidatePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	audit := ranking.AuditRecall(full, candidate, 100)
	if err := json.NewEncoder(os.Stdout).Encode(audit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if !audit.Passed() {
		os.Exit(1)
	}
}

func readOpportunities(path string) ([]ranking.Opportunity, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	var values []ranking.Opportunity
	if err := json.NewDecoder(file).Decode(&values); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return values, nil
}
