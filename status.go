package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdStatus prints a short summary by reading the spool folders.
func cmdStatus(args []string) {
	if len(args) != 1 {
		fatalf("usage: catenaline status <dir>")
	}
	dir := args[0]
	if _, err := os.Stat(filepath.Join(dir, "input")); err != nil {
		fatalf("%s is not a catenaline folder", dir)
	}

	fmt.Println("waiting:")
	dirs, _ := os.ReadDir(filepath.Join(dir, "input"))
	for _, d := range dirs {
		files, _ := os.ReadDir(filepath.Join(dir, "input", d.Name()))
		if len(files) > 0 {
			fmt.Printf("  %-30s %d file(s)\n", d.Name(), len(files))
		}
	}

	fmt.Println("running:")
	states, _ := filepath.Glob(filepath.Join(dir, "running", "*", "*", "state.json"))
	for _, p := range states {
		var st runState
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &st) == nil {
			who := "queued"
			if st.Worker != "" {
				who = "on " + st.Worker
			}
			fmt.Printf("  %-30s step %d/%d (%s) %s\n", st.Pipeline.Name+"/"+st.Name, st.Step,
				len(st.Pipeline.Steps), st.Pipeline.Steps[st.Step-1].Adapter, who)
		}
	}

	fmt.Println("done:")
	runs, _ := filepath.Glob(filepath.Join(dir, "output", "*", "*"))
	for _, r := range runs {
		steps, _ := filepath.Glob(filepath.Join(r, "step*"))
		_, err := os.Stat(filepath.Join(r, "original"))
		state := "complete"
		if err != nil {
			state = "not finished (see running or errors)"
		}
		fmt.Printf("  %-30s %d step(s), %s\n", rel(dir, "output", r), len(steps), state)
	}

	fmt.Println("errors:")
	fails, _ := filepath.Glob(filepath.Join(dir, "errors", "*", "*"))
	for _, f := range fails {
		reason, _ := os.ReadFile(filepath.Join(f, "REASON.txt"))
		fmt.Printf("  %-30s %s\n", rel(dir, "errors", f), strings.TrimSpace(string(reason)))
	}
}

func rel(dir, area, path string) string {
	r, _ := filepath.Rel(filepath.Join(dir, area), path)
	return r
}
