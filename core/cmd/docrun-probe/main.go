package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"orbsleep/internal/deployment/docrun"
	"orbsleep/internal/dockerx"
)

// Diffs the fingerprint of the CREATED (not started) replacement container
// against the ManagedSpec fingerprint — reproduces the phase2 fault-injection-2
// recovery fingerprint mismatch.
func main() {
	replName := os.Args[1]
	ctx := context.Background()
	c, err := dockerx.New(ctx, os.Getenv("HOME")+"/.orbstack/run/docker.sock")
	if err != nil {
		panic(err)
	}
	ins, err := c.InspectContainer(ctx, replName)
	if err != nil {
		panic(err)
	}
	rt, err := docrun.SpecFromInspect(ins)
	if err != nil {
		panic(err)
	}
	got := docrun.Fingerprint(rt)
	a, _ := json.MarshalIndent(rt, "", " ")
	_ = os.WriteFile("/tmp/spec-created.json", a, 0o600)
	fmt.Println(got)
}
