package main

// Program adapters: how to run one program on one input file, how to tell
// whether it worked, and which files to bring back.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Adapter describes one program. Text fields may use {input} (the input
// file name, e.g. water.xyz) and {stem} (the same without extension, water).
type Adapter struct {
	Command  string            `json:"command"`           // executable; a full path is safest
	Args     []string          `json:"args"`              // e.g. ["{input}", "--opt"]
	InputExt string            `json:"input_ext"`         // e.g. ".inp"
	Stdout   string            `json:"stdout,omitempty"`  // file that receives stdout, e.g. "{stem}.out"
	Stderr   string            `json:"stderr,omitempty"`  // file that receives stderr
	Env      map[string]string `json:"env,omitempty"`     // extra environment variables
	Success  Success           `json:"success"`           // how to tell that the run worked
	Collect  []string          `json:"collect"`           // globs of files to bring back; empty = all
	Timeout  string            `json:"timeout,omitempty"` // e.g. "30m"; empty = no limit
	Note     string            `json:"note,omitempty"`    // free text for humans
}

// Success: every condition that is set must hold.
type Success struct {
	ExitCode *int   `json:"exit_code,omitempty"` // required exit code (usually 0)
	File     string `json:"file,omitempty"`      // file to search, e.g. "{stem}.out"
	Regex    string `json:"regex,omitempty"`     // must match somewhere in File
}

// expand replaces {input} and {stem} in s.
func expand(s, input string) string {
	stem := strings.TrimSuffix(input, filepath.Ext(input))
	return strings.NewReplacer("{input}", input, "{stem}", stem).Replace(s)
}

// CommandLine returns the executable and arguments for the given input file.
func (a Adapter) CommandLine(input string) (string, []string) {
	args := make([]string, len(a.Args))
	for i, s := range a.Args {
		args[i] = expand(s, input)
	}
	return expand(a.Command, input), args
}

// TimeoutDuration parses Timeout (0 = none).
func (a Adapter) TimeoutDuration() (time.Duration, error) {
	if a.Timeout == "" {
		return 0, nil
	}
	return time.ParseDuration(a.Timeout)
}

// Check decides whether a finished run succeeded, given its exit code and a
// function that reads one of the returned files. It returns "" on success,
// otherwise the reason.
func (a Adapter) Check(input string, exitCode int, readFile func(name string) ([]byte, bool)) string {
	s := a.Success
	if s.ExitCode != nil && exitCode != *s.ExitCode {
		return fmt.Sprintf("exit code %d, expected %d", exitCode, *s.ExitCode)
	}
	if s.Regex != "" {
		name := expand(s.File, input)
		data, ok := readFile(name)
		if !ok {
			return fmt.Sprintf("success file %s was not returned", name)
		}
		re, err := regexp.Compile(s.Regex)
		if err != nil {
			return "bad success regex: " + err.Error()
		}
		if !re.Match(data) {
			return fmt.Sprintf("%q not found in %s", s.Regex, name)
		}
	}
	return ""
}

// Collected reports whether a file name produced by the run should be
// returned, according to the Collect globs.
func (a Adapter) Collected(input, name string) bool {
	if len(a.Collect) == 0 {
		return true
	}
	for _, g := range a.Collect {
		if ok, _ := filepath.Match(expand(g, input), name); ok {
			return true
		}
	}
	// The files named for stdout, stderr and the success check always come back.
	for _, f := range []string{a.Stdout, a.Stderr, a.Success.File} {
		if f != "" && expand(f, input) == name {
			return true
		}
	}
	return false
}

func (a Adapter) validate(name string) error {
	if a.Command == "" {
		return fmt.Errorf("adapter %s: no command", name)
	}
	if a.Success.Regex != "" {
		if a.Success.File == "" {
			return fmt.Errorf("adapter %s: success.regex needs success.file", name)
		}
		if _, err := regexp.Compile(a.Success.Regex); err != nil {
			return fmt.Errorf("adapter %s: %v", name, err)
		}
	}
	if a.Success.ExitCode == nil && a.Success.Regex == "" {
		return fmt.Errorf("adapter %s: success needs exit_code and/or regex", name)
	}
	if _, err := a.TimeoutDuration(); err != nil {
		return fmt.Errorf("adapter %s: timeout: %v", name, err)
	}
	return nil
}

// LoadAdapters reads programs.json.
func LoadAdapters(path string) (map[string]Adapter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]Adapter{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	for name, a := range m {
		if err := a.validate(name); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// orcaDir is the ORCA folder the default programs.json uses: $ORCA_DIR, else the
// folder of the orca found on PATH (links followed), else /opt/orca-6.1.1.
func orcaDir() string {
	if d := os.Getenv("ORCA_DIR"); d != "" {
		return filepath.Clean(d)
	}
	if p, err := exec.LookPath("orca"); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if abs, err := filepath.Abs(p); err == nil {
			return filepath.Dir(abs)
		}
	}
	return "/opt/orca-6.1.1"
}

// programsFor is defaultPrograms with ORCA in dir. ORCA for macOS has no otool_xtb: then
// an xtb found on PATH is used instead (it takes the same arguments).
func programsFor(dir string) string {
	text := defaultPrograms
	if _, err := os.Stat(filepath.Join(dir, "otool_xtb")); err != nil {
		if p, err := exec.LookPath("xtb"); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				q, _ := json.Marshal(abs)
				text = strings.Replace(text, `"/opt/orca-6.1.1/otool_xtb"`, string(q), 1)
			}
		}
	}
	q, _ := json.Marshal(dir)
	return strings.ReplaceAll(text, "/opt/orca-6.1.1", string(q[1:len(q)-1]))
}

// defaultPrograms is written to <dir>/programs.json on first start, with ORCA's
// folder from orcaDir.
const defaultPrograms = `{
  "orca": {
    "note": "ORCA 6. Called by its full path, as ORCA requires.",
    "command": "/opt/orca-6.1.1/orca",
    "args": ["{input}"],
    "input_ext": ".inp",
    "stdout": "{stem}.out",
    "stderr": "{stem}.err",
    "env": {"LD_LIBRARY_PATH": "/opt/orca-6.1.1/lib", "OMP_NUM_THREADS": "1"},
    "success": {"exit_code": 0, "file": "{stem}.out", "regex": "ORCA TERMINATED NORMALLY"},
    "collect": ["*"],
    "timeout": "2h"
  },
  "xtb": {
    "note": "xtb as bundled with ORCA (otool_xtb). Writes xtbopt.xyz; prints 'normal termination of xtb' on stderr.",
    "command": "/opt/orca-6.1.1/otool_xtb",
    "args": ["{input}", "--opt"],
    "input_ext": ".xyz",
    "stdout": "{stem}.out",
    "stderr": "{stem}.err",
    "env": {"OMP_NUM_THREADS": "1"},
    "success": {"exit_code": 0, "file": "{stem}.err", "regex": "normal termination of xtb"},
    "collect": ["xtbopt.xyz", "xtbopt.log", "charges", "wbo", "{stem}.out", "{stem}.err"],
    "timeout": "30m"
  },
  "shell": {
    "note": "Runs a shell script. Example for any program that is not installed as an adapter.",
    "command": "/bin/sh",
    "args": ["{input}"],
    "input_ext": ".sh",
    "stdout": "{stem}.out",
    "stderr": "{stem}.err",
    "success": {"exit_code": 0},
    "collect": ["*"],
    "timeout": "10m"
  },
  "lammps": {
    "note": "UNTESTED: LAMMPS is not installed on the development machine.",
    "command": "lmp",
    "args": ["-in", "{input}", "-log", "{stem}.log"],
    "input_ext": ".in",
    "stdout": "{stem}.out",
    "stderr": "{stem}.err",
    "success": {"exit_code": 0, "file": "{stem}.log", "regex": "Total wall time"},
    "collect": ["*"],
    "timeout": "2h"
  }
}
`
