package main

// Pipelines: a list of steps, each one running an adapter on an input that
// is built from an earlier step's output files.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Pipeline is read from <dir>/pipelines/<name>.json.
type Pipeline struct {
	Name  string `json:"name"` // taken from the file name
	Steps []Step `json:"steps"`
}

// Step builds its input from the files of a source step and runs an adapter.
//
//   - From: which step's output files to use. Absent = the previous step;
//     0 = the file that was dropped into input/<pipeline>/.
//   - InputFile: pass this file (a name or glob matching exactly one file)
//     from the source step as the input.
//   - Template: or write these lines as the input. {stem} is the job name,
//     {file:NAME} is replaced by the content of NAME from the source step and
//     {xyz:NAME} by the coordinate lines of the .xyz file NAME (its first
//     two lines, atom count and comment, left out).
//   - Ship: more files (names or globs) from the source step to send along,
//     for inputs that refer to them (e.g. ORCA's "* xyzfile 0 1 xtbopt.xyz").
//
// With neither InputFile nor Template, the step takes the dropped file
// itself (only valid when the source is step 0).
type Step struct {
	Adapter   string   `json:"adapter"`
	From      *int     `json:"from,omitempty"`
	InputFile string   `json:"input_file,omitempty"`
	Template  []string `json:"template,omitempty"`
	Ship      []string `json:"ship,omitempty"`
}

// Source returns the number of the step whose outputs feed step n (1-based).
func (s Step) Source(n int) int {
	if s.From != nil {
		return *s.From
	}
	return n - 1
}

// LoadPipelines reads every pipelines/*.json in dir.
func LoadPipelines(dir string) (map[string]Pipeline, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	m := map[string]Pipeline{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var pl Pipeline
		if err := json.Unmarshal(data, &pl); err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		pl.Name = strings.TrimSuffix(filepath.Base(p), ".json")
		m[pl.Name] = pl
	}
	return m, nil
}

// Validate checks a pipeline against the known adapters.
func (p Pipeline) Validate(adapters map[string]Adapter) error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("pipeline %s has no steps", p.Name)
	}
	for i, s := range p.Steps {
		n := i + 1
		if _, ok := adapters[s.Adapter]; !ok {
			return fmt.Errorf("pipeline %s step %d: unknown adapter %q", p.Name, n, s.Adapter)
		}
		if src := s.Source(n); src < 0 || src >= n {
			return fmt.Errorf("pipeline %s step %d: from must be an earlier step (0..%d)", p.Name, n, n-1)
		}
		if s.InputFile != "" && s.Template != nil {
			return fmt.Errorf("pipeline %s step %d: use input_file or template, not both", p.Name, n)
		}
		if s.InputFile == "" && s.Template == nil && s.Source(n) != 0 {
			return fmt.Errorf("pipeline %s step %d: needs input_file or template", p.Name, n)
		}
	}
	return nil
}

// BuildInput makes the input files for step n (1-based) of job "stem".
// srcDir holds the source step's files (for step 0, the dropped file).
// It returns the files to send and the name of the main input file.
func BuildInput(p Pipeline, n int, stem string, srcDir string, adapters map[string]Adapter) (map[string][]byte, string, error) {
	s := p.Steps[n-1]
	a := adapters[s.Adapter]
	files := map[string][]byte{}
	var main []byte

	switch {
	case s.Template != nil:
		text, err := fillTemplate(strings.Join(s.Template, "\n")+"\n", stem, srcDir)
		if err != nil {
			return nil, "", err
		}
		main = []byte(text)
	default:
		pattern := s.InputFile
		if pattern == "" {
			pattern = "*" // step fed by the dropped file: the only file there
		}
		path, err := matchOne(srcDir, pattern)
		if err != nil {
			return nil, "", err
		}
		if main, err = os.ReadFile(path); err != nil {
			return nil, "", err
		}
	}

	for _, pattern := range s.Ship {
		paths, _ := filepath.Glob(filepath.Join(srcDir, pattern))
		if len(paths) == 0 {
			return nil, "", fmt.Errorf("file to ship %q not found in the source step", pattern)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, "", err
			}
			files[filepath.Base(path)] = data
		}
	}

	input := stem + a.InputExt
	files[input] = main // the main input wins over a shipped file of the same name
	return files, input, nil
}

var fileRef = regexp.MustCompile(`\{(file|xyz):([^}]+)\}`)

// fillTemplate replaces {stem}, {file:NAME} and {xyz:NAME} (see Step).
func fillTemplate(t, stem, srcDir string) (string, error) {
	var err error
	out := fileRef.ReplaceAllStringFunc(t, func(m string) string {
		sub := fileRef.FindStringSubmatch(m)
		path, e := matchOne(srcDir, sub[2])
		if e != nil {
			err = e
			return ""
		}
		data, e := os.ReadFile(path)
		if e != nil {
			err = e
			return ""
		}
		text := string(data)
		if sub[1] == "xyz" {
			lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
			if len(lines) < 3 {
				err = fmt.Errorf("%s is not an .xyz file", sub[2])
				return ""
			}
			text = strings.Join(lines[2:], "\n")
		}
		return strings.TrimRight(text, "\n")
	})
	return strings.ReplaceAll(out, "{stem}", stem), err
}

// matchOne finds exactly one file matching pattern in dir.
func matchOne(dir, pattern string) (string, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Strings(paths)
	switch len(paths) {
	case 0:
		return "", fmt.Errorf("no file %q in the source step", pattern)
	case 1:
		return paths[0], nil
	default:
		return "", fmt.Errorf("%q matches %d files in the source step", pattern, len(paths))
	}
}
