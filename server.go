package main

// The coordinator: watches input/, turns each dropped file into a run of a
// pipeline, hands one step at a time to the workers that ask for work, and
// files the results under output/ or errors/.
//
// Layout of <dir>:
//
//	token, programs.json, pipelines/*.json
//	input/<pipeline>/<file>              drop files here
//	running/<pipeline>/<name>/           state.json + original/<file>
//	output/<pipeline>/<name>/stepN-<adapter>/   results, written as steps finish
//	errors/<pipeline>/<name>/            REASON.txt + the failing step's files

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Job is one step of one run, as sent to a worker.
type Job struct {
	ID          string            `json:"id"` // lease id, new for every hand-out
	AdapterName string            `json:"adapter_name"`
	Adapter     Adapter           `json:"adapter"`
	Input       string            `json:"input"` // main input file name
	Files       map[string][]byte `json:"files"` // input + shipped files
}

// Result is what a worker sends back.
type Result struct {
	ID       string            `json:"id"`
	ExitCode int               `json:"exit_code"`
	Error    string            `json:"error,omitempty"` // could not run, timeout, ...
	Files    map[string][]byte `json:"files"`
}

// runState is saved as running/<pipeline>/<name>/state.json; it is the
// checkpoint that lets a restarted coordinator continue a chain.
type runState struct {
	Pipeline Pipeline `json:"pipeline"` // copy taken when the run started
	Name     string   `json:"name"`
	Original string   `json:"original"` // dropped file name
	Step     int      `json:"step"`     // current step, 1-based
	Worker   string   `json:"worker,omitempty"`
	Started  string   `json:"started"`
}

type pending struct {
	run      *runState
	job      Job
	worker   string
	lastBeat time.Time
}

type server struct {
	dir      string
	token    string
	adapters map[string]Adapter

	mu     sync.Mutex
	queue  []*pending
	leased map[string]*pending
}

const leaseTimeout = 2 * time.Minute // a worker that stops sending heartbeats loses its job

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8470", "address to listen on")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fatalf("usage: catenaline serve [-listen :8470] <dir>")
	}
	s, err := newServer(fs.Arg(0))
	if err != nil {
		fatalf("%v", err)
	}
	s.recover()
	go s.loop()
	log.Printf("serving %s on %s (token in %s)", s.dir, *listen, filepath.Join(s.dir, "token"))
	fatalf("%v", http.ListenAndServe(*listen, s.handler()))
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /job", s.auth(s.handleJob))
	mux.HandleFunc("POST /heartbeat", s.auth(s.handleHeartbeat))
	mux.HandleFunc("POST /result", s.auth(s.handleResult))
	return mux
}

func newServer(dir string) (*server, error) {
	for _, d := range []string{"input", "running", "output", "errors", "pipelines"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	tokPath := filepath.Join(dir, "token")
	tok, err := os.ReadFile(tokPath)
	if errors.Is(err, os.ErrNotExist) {
		tok = []byte(randomHex(16))
		err = os.WriteFile(tokPath, append(tok, '\n'), 0o600)
	}
	if err != nil {
		return nil, err
	}
	progPath := filepath.Join(dir, "programs.json")
	if _, err := os.Stat(progPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(progPath, []byte(programsFor(orcaDir())), 0o644); err != nil {
			return nil, err
		}
	}
	adapters, err := LoadAdapters(progPath)
	if err != nil {
		return nil, err
	}
	s := &server{dir: dir, token: strings.TrimSpace(string(tok)), adapters: adapters, leased: map[string]*pending{}}
	if _, err := s.pipelines(); err != nil {
		return nil, err
	}
	return s, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pipelines returns the pipelines in pipelines/ plus a one-step pipeline per
// adapter, so that input/xtb/ runs xtb alone.
func (s *server) pipelines() (map[string]Pipeline, error) {
	m, err := LoadPipelines(filepath.Join(s.dir, "pipelines"))
	if err != nil {
		return nil, err
	}
	for name := range s.adapters {
		if _, ok := m[name]; !ok {
			m[name] = Pipeline{Name: name, Steps: []Step{{Adapter: name}}}
		}
	}
	for _, p := range m {
		if err := p.Validate(s.adapters); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ---- the loop: new files and lost workers ----

func (s *server) loop() {
	for {
		s.scanInput()
		s.expireLeases()
		time.Sleep(2 * time.Second)
	}
}

func (s *server) scanInput() {
	pls, err := s.pipelines()
	if err != nil {
		log.Printf("pipelines: %v", err)
		return
	}
	// One input folder per pipeline and per adapter (a one-step pipeline).
	for name := range pls {
		os.MkdirAll(filepath.Join(s.dir, "input", name), 0o755)
	}
	dirs, _ := os.ReadDir(filepath.Join(s.dir, "input"))
	for _, d := range dirs {
		p, ok := pls[d.Name()]
		if !d.IsDir() || !ok {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(s.dir, "input", d.Name()))
		for _, f := range files {
			info, err := f.Info()
			if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(f.Name(), ".") ||
				strings.HasSuffix(f.Name(), ".part") || time.Since(info.ModTime()) < time.Second {
				continue // skip hidden, partial and still-being-written files
			}
			s.startRun(p, filepath.Join(s.dir, "input", d.Name(), f.Name()))
		}
	}
}

// startRun moves a dropped file into running/ and queues step 1.
func (s *server) startRun(p Pipeline, path string) {
	file := filepath.Base(path)
	base := strings.TrimSuffix(file, filepath.Ext(file))
	name := base
	for i := 2; s.nameUsed(p.Name, name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	runDir := filepath.Join(s.dir, "running", p.Name, name)
	if err := os.MkdirAll(filepath.Join(runDir, "original"), 0o755); err != nil {
		log.Printf("%v", err)
		return
	}
	if err := os.Rename(path, filepath.Join(runDir, "original", file)); err != nil {
		log.Printf("%v", err)
		return
	}
	st := &runState{Pipeline: p, Name: name, Original: file, Step: 1, Started: time.Now().Format(time.RFC3339)}
	s.saveState(st)
	log.Printf("%s/%s: started (%d steps)", p.Name, name, len(p.Steps))
	s.queueStep(st)
}

func (s *server) nameUsed(pipeline, name string) bool {
	for _, area := range []string{"running", "output", "errors"} {
		if _, err := os.Stat(filepath.Join(s.dir, area, pipeline, name)); err == nil {
			return true
		}
	}
	return false
}

func (s *server) runDir(st *runState) string {
	return filepath.Join(s.dir, "running", st.Pipeline.Name, st.Name)
}

func (s *server) stepDir(area string, st *runState, n int) string {
	return filepath.Join(s.dir, area, st.Pipeline.Name, st.Name, fmt.Sprintf("step%d-%s", n, st.Pipeline.Steps[n-1].Adapter))
}

func (s *server) saveState(st *runState) {
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(filepath.Join(s.runDir(st), "state.json"), data, 0o644); err != nil {
		log.Printf("save state: %v", err)
	}
}

// queueStep builds the input of the current step and puts it in the queue.
func (s *server) queueStep(st *runState) {
	n := st.Step
	step := st.Pipeline.Steps[n-1]
	src := filepath.Join(s.runDir(st), "original")
	if from := step.Source(n); from > 0 {
		src = s.stepDir("output", st, from)
	}
	files, input, err := BuildInput(st.Pipeline, n, st.Name, src, s.adapters)
	if err != nil {
		s.fail(st, fmt.Sprintf("step %d (%s): could not build the input: %v", n, step.Adapter, err), nil)
		return
	}
	job := Job{AdapterName: step.Adapter, Adapter: s.adapters[step.Adapter], Input: input, Files: files}
	s.mu.Lock()
	s.queue = append(s.queue, &pending{run: st, job: job})
	s.mu.Unlock()
}

// recover re-queues the current step of every run left in running/ by a
// previous coordinator (the checkpoint is state.json).
func (s *server) recover() {
	paths, _ := filepath.Glob(filepath.Join(s.dir, "running", "*", "*", "state.json"))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		var st runState
		if err == nil {
			err = json.Unmarshal(data, &st)
		}
		if err != nil {
			log.Printf("%s: %v", p, err)
			continue
		}
		log.Printf("%s/%s: continuing at step %d", st.Pipeline.Name, st.Name, st.Step)
		s.queueStep(&st)
	}
}

func (s *server) expireLeases() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.leased {
		if time.Since(p.lastBeat) > leaseTimeout {
			log.Printf("%s/%s step %d: worker %s silent, job queued again", p.run.Pipeline.Name, p.run.Name, p.run.Step, p.worker)
			delete(s.leased, id)
			s.queue = append([]*pending{p}, s.queue...)
		}
	}
}

// ---- HTTP handlers ----

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if len(s.queue) == 0 {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p := s.queue[0]
	s.queue = s.queue[1:]
	p.job.ID = randomHex(8)
	p.worker = r.URL.Query().Get("worker")
	p.lastBeat = time.Now()
	s.leased[p.job.ID] = p
	p.run.Worker = p.worker
	s.mu.Unlock()
	s.saveState(p.run)
	log.Printf("%s/%s step %d (%s) -> %s", p.run.Pipeline.Name, p.run.Name, p.run.Step, p.job.AdapterName, p.worker)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p.job)
}

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.leased[r.URL.Query().Get("id")]
	if !ok {
		http.Error(w, "job no longer yours", http.StatusGone)
		return
	}
	p.lastBeat = time.Now()
}

func (s *server) handleResult(w http.ResponseWriter, r *http.Request) {
	var res Result
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<30)).Decode(&res); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p, ok := s.leased[res.ID]
	delete(s.leased, res.ID)
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown or expired job", http.StatusGone)
		return
	}
	s.finishStep(p, res)
}

// finishStep checks a returned step and moves the run forward or to errors/.
func (s *server) finishStep(p *pending, res Result) {
	st := p.run
	n := st.Step
	adapter := p.job.Adapter
	reason := res.Error
	if reason == "" {
		reason = adapter.Check(p.job.Input, res.ExitCode, func(name string) ([]byte, bool) {
			d, ok := res.Files[name]
			return d, ok
		})
	}
	tag := fmt.Sprintf("%s/%s step %d (%s)", st.Pipeline.Name, st.Name, n, p.job.AdapterName)
	if reason != "" {
		log.Printf("%s FAILED on %s: %s", tag, p.worker, reason)
		s.fail(st, fmt.Sprintf("step %d (%s) on worker %s: %s", n, p.job.AdapterName, p.worker, reason), res.Files)
		return
	}
	dest := s.stepDir("output", st, n)
	if err := writeFiles(dest, res.Files); err != nil {
		s.fail(st, "saving results: "+err.Error(), nil)
		return
	}
	log.Printf("%s done on %s", tag, p.worker)
	if n == len(st.Pipeline.Steps) {
		// Chain complete: keep the original input next to the results.
		final := filepath.Join(s.dir, "output", st.Pipeline.Name, st.Name)
		os.Rename(filepath.Join(s.runDir(st), "original"), filepath.Join(final, "original"))
		os.RemoveAll(s.runDir(st))
		log.Printf("%s/%s: pipeline complete", st.Pipeline.Name, st.Name)
		return
	}
	st.Step++
	st.Worker = ""
	s.saveState(st)
	s.queueStep(st)
}

// fail moves a run to errors/ with REASON.txt and the failing step's files.
// Steps that finished earlier stay in output/.
func (s *server) fail(st *runState, reason string, files map[string][]byte) {
	errDir := filepath.Join(s.dir, "errors", st.Pipeline.Name, st.Name)
	os.MkdirAll(errDir, 0o755)
	if files != nil {
		writeFiles(s.stepDir("errors", st, st.Step), files)
	}
	os.WriteFile(filepath.Join(errDir, "REASON.txt"), []byte(reason+"\n"), 0o644)
	os.Rename(filepath.Join(s.runDir(st), "original"), filepath.Join(errDir, "original"))
	os.Rename(filepath.Join(s.runDir(st), "state.json"), filepath.Join(errDir, "state.json"))
	os.RemoveAll(s.runDir(st))
	log.Printf("%s/%s: stopped, see %s", st.Pipeline.Name, st.Name, errDir)
}

// writeFiles saves files into dir, replacing what was there. Names are
// reduced to their base so a worker cannot write outside dir.
func writeFiles(dir string, files map[string][]byte) error {
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range files {
		name = filepath.Base(name)
		if name == "." || name == ".." || name == string(filepath.Separator) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
