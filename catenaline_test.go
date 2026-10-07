package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func intp(i int) *int { return &i }

func TestAdapterTemplating(t *testing.T) {
	a := Adapter{
		Command: "/opt/x/{stem}-runner",
		Args:    []string{"-in", "{input}", "-log", "{stem}.log"},
		Collect: []string{"{stem}.gbw", "*.xyz"},
		Stdout:  "{stem}.out",
	}
	exe, args := a.CommandLine("water.inp")
	if exe != "/opt/x/water-runner" || strings.Join(args, " ") != "-in water.inp -log water.log" {
		t.Fatalf("got %s %v", exe, args)
	}
	for name, want := range map[string]bool{
		"water.gbw": true, "xtbopt.xyz": true, "water.out": true, // stdout always comes back
		"water.tmp": false, "other.gbw": false,
	} {
		if got := a.Collected("water.inp", name); got != want {
			t.Errorf("Collected(%s) = %v, want %v", name, got, want)
		}
	}
	if !(Adapter{}).Collected("water.inp", "anything") {
		t.Error("an empty collect list should bring back everything")
	}
}

func TestAdapterCheck(t *testing.T) {
	a := Adapter{Success: Success{ExitCode: intp(0), File: "{stem}.out", Regex: "TERMINATED NORMALLY"}}
	files := map[string][]byte{"w.out": []byte("...\n****ORCA TERMINATED NORMALLY****\n")}
	read := func(n string) ([]byte, bool) { d, ok := files[n]; return d, ok }
	if r := a.Check("w.inp", 0, read); r != "" {
		t.Errorf("good run rejected: %s", r)
	}
	if r := a.Check("w.inp", 1, read); !strings.Contains(r, "exit code 1") {
		t.Errorf("bad exit code accepted: %q", r)
	}
	files["w.out"] = []byte("error")
	if r := a.Check("w.inp", 0, read); !strings.Contains(r, "not found") {
		t.Errorf("missing regex accepted: %q", r)
	}
	delete(files, "w.out")
	if r := a.Check("w.inp", 0, read); !strings.Contains(r, "not returned") {
		t.Errorf("missing file accepted: %q", r)
	}
}

func TestDefaultProgramsLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "programs.json")
	os.WriteFile(p, []byte(defaultPrograms), 0o644)
	m, err := LoadAdapters(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"orca", "xtb", "shell", "lammps"} {
		if _, ok := m[name]; !ok {
			t.Errorf("default adapter %s missing", name)
		}
	}
}

func TestBuildInput(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "xtbopt.xyz"), []byte("2\nenergy -1.0\nH 0 0 0\nH 0 0 0.74\n"), 0o644)
	os.WriteFile(filepath.Join(src, "w.out"), []byte("log"), 0o644)
	adapters := map[string]Adapter{"orca": {InputExt: ".inp"}, "xtb": {InputExt: ".xyz"}}
	p := Pipeline{Name: "p", Steps: []Step{
		{Adapter: "xtb"},
		{Adapter: "orca", Template: []string{"! r2SCAN-3c", "* xyzfile 0 1 xtbopt.xyz"}, Ship: []string{"xtbopt.xyz"}},
		{Adapter: "orca", From: intp(1), Template: []string{"# {stem}", "* xyz 0 1", "{xyz:xtbopt.xyz}", "*"}},
		{Adapter: "xtb", From: intp(1), InputFile: "xtbopt.xyz"},
	}}
	if err := p.Validate(adapters); err != nil {
		t.Fatal(err)
	}

	files, input, err := BuildInput(p, 2, "w", src, adapters)
	if err != nil {
		t.Fatal(err)
	}
	if input != "w.inp" || string(files["w.inp"]) != "! r2SCAN-3c\n* xyzfile 0 1 xtbopt.xyz\n" || files["xtbopt.xyz"] == nil || len(files) != 2 {
		t.Errorf("template+ship: input %s, files %q", input, files)
	}

	files, _, err = BuildInput(p, 3, "w", src, adapters)
	if want := "# w\n* xyz 0 1\nH 0 0 0\nH 0 0 0.74\n*\n"; err != nil || string(files["w.inp"]) != want {
		t.Errorf("{xyz:} template: %v %q", err, files["w.inp"])
	}

	files, input, err = BuildInput(p, 4, "w", src, adapters)
	if err != nil || input != "w.xyz" || !strings.HasPrefix(string(files["w.xyz"]), "2\nenergy") {
		t.Errorf("input_file: %v %s %q", err, input, files)
	}

	bad := Pipeline{Name: "b", Steps: []Step{{Adapter: "orca", Template: []string{"{file:missing.txt}"}}}}
	if _, _, err := BuildInput(bad, 1, "w", src, adapters); err == nil {
		t.Error("missing referenced file not reported")
	}
	if err := (Pipeline{Steps: []Step{{Adapter: "nope"}}}).Validate(adapters); err == nil {
		t.Error("unknown adapter accepted")
	}
	if err := (Pipeline{Steps: []Step{{Adapter: "xtb"}, {Adapter: "xtb"}}}).Validate(adapters); err == nil {
		t.Error("step 2 without an input accepted")
	}
}

// TestChain runs a three-step pipeline with a fake program (a shell script)
// through a real coordinator and worker over HTTP, plus a pipeline whose
// second step fails.
func TestChain(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake.sh")
	// Reads a number from its input, writes twice the number to result.txt.
	// An input containing "fail" makes it exit 3.
	os.WriteFile(fake, []byte(`#!/bin/sh
grep -q fail "$1" && { echo broken; exit 3; }
n=$(grep -o '[0-9][0-9]*' "$1" | head -1)
echo $((n * 2)) > result.txt
echo "input was: $(cat "$1")"
echo "FAKE DONE"
`), 0o755)
	os.WriteFile(filepath.Join(dir, "programs.json"), []byte(`{
  "fake": {"command": "`+fake+`", "args": ["{input}"], "input_ext": ".txt",
           "stdout": "{stem}.log", "success": {"exit_code": 0, "file": "{stem}.log", "regex": "FAKE DONE"},
           "collect": ["result.txt"]}
}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "pipelines"), 0o755)
	os.WriteFile(filepath.Join(dir, "pipelines", "triple.json"), []byte(`{"steps": [
  {"adapter": "fake"},
  {"adapter": "fake", "template": ["value {file:result.txt}"]},
  {"adapter": "fake", "from": 1, "input_file": "result.txt"}
]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "pipelines", "breaks.json"), []byte(`{"steps": [
  {"adapter": "fake"},
  {"adapter": "fake", "template": ["fail {file:result.txt}"]},
  {"adapter": "fake", "input_file": "result.txt"}
]}`), 0o644)

	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	// A wrong token is refused.
	bad := &client{base: ts.URL, token: "wrong", name: "x", http: http.DefaultClient}
	if _, err := bad.getJob(); err == nil {
		t.Error("wrong token accepted")
	}

	s.scanInput() // creates input/triple and input/breaks
	old := time.Now().Add(-time.Minute)
	for _, f := range []string{"triple/n.txt", "breaks/m.txt"} {
		p := filepath.Join(dir, "input", f)
		os.WriteFile(p, []byte("21\n"), 0o644)
		os.Chtimes(p, old, old)
	}
	s.scanInput()

	c := &client{base: ts.URL, token: s.token, name: "w1", http: http.DefaultClient}
	for i := 0; i < 10; i++ {
		job, err := c.getJob()
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		res := runJob(job, filepath.Join(dir, "work", job.ID), func(string) bool { return true })
		if err := c.sendResult(res); err != nil {
			t.Fatal(err)
		}
	}

	read := func(p string) string {
		d, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Errorf("%v", err)
		}
		return strings.TrimSpace(string(d))
	}
	// 21 -> 42; "value 42" -> 84; step 3 reads step 1's result.txt (42) -> 84.
	if got := read("output/triple/n/step1-fake/result.txt"); got != "42" {
		t.Errorf("step 1: %s", got)
	}
	if got := read("output/triple/n/step2-fake/n.log"); !strings.Contains(got, "input was: value 42") {
		t.Errorf("step 2 log: %s", got)
	}
	if got := read("output/triple/n/step3-fake/result.txt"); got != "84" {
		t.Errorf("step 3: %s", got)
	}
	if got := read("output/triple/n/original/n.txt"); got != "21" {
		t.Errorf("original: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "running", "triple", "n")); err == nil {
		t.Error("finished run left in running/")
	}

	// The broken pipeline stops at step 2 with a reason; step 3 never runs.
	if got := read("errors/breaks/m/REASON.txt"); !strings.Contains(got, "step 2") || !strings.Contains(got, "exit code 3") {
		t.Errorf("reason: %s", got)
	}
	if got := read("errors/breaks/m/step2-fake/m.log"); got != "broken" {
		t.Errorf("failing step's log: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "output", "breaks", "m", "step3-fake")); err == nil {
		t.Error("step 3 ran after a failure")
	}
	if got := read("output/breaks/m/step1-fake/result.txt"); got != "42" {
		t.Errorf("step 1 of the broken chain should be kept: %s", got)
	}
}

// The default programs.json points at the ORCA folder it is given, escaped for JSON.
func TestProgramsFor(t *testing.T) {
	for _, dir := range []string{"/home/claudia/orca 6", `C:\ORCA\orca_6_1_1`} {
		p := filepath.Join(t.TempDir(), "programs.json")
		os.WriteFile(p, []byte(programsFor(dir)), 0o644)
		a, err := LoadAdapters(p)
		if err != nil {
			t.Fatal(err)
		}
		if a["orca"].Command != dir+"/orca" || a["xtb"].Command != dir+"/otool_xtb" {
			t.Errorf("%s: orca %q, xtb %q", dir, a["orca"].Command, a["xtb"].Command)
		}
	}
	t.Setenv("ORCA_DIR", "/srv/orca/")
	if d := orcaDir(); d != "/srv/orca" {
		t.Errorf("ORCA_DIR: got %q", d)
	}
}

// ORCA for macOS has no otool_xtb: an xtb on PATH is used instead.
func TestProgramsForNoOtoolXtb(t *testing.T) {
	orcaDir, bin := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(bin, "xtb"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	p := filepath.Join(t.TempDir(), "programs.json")
	os.WriteFile(p, []byte(programsFor(orcaDir)), 0o644)
	a, err := LoadAdapters(p)
	if err != nil {
		t.Fatal(err)
	}
	if a["xtb"].Command != filepath.Join(bin, "xtb") || a["orca"].Command != orcaDir+"/orca" {
		t.Errorf("xtb %q, orca %q", a["xtb"].Command, a["orca"].Command)
	}
}
