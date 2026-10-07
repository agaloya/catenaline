package main

// The worker: asks the coordinator for a job, runs it in a fresh folder,
// and sends back the collected files with the exit code.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type client struct {
	base, token, name string
	http              *http.Client
}

func cmdWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	srv := fs.String("server", "", "coordinator host:port")
	token := fs.String("token", "", "shared token (the content of <dir>/token)")
	host, _ := os.Hostname()
	name := fs.String("name", fmt.Sprintf("%s-%d", host, os.Getpid()), "worker name shown in logs")
	workdir := fs.String("workdir", "catenaline-work", "folder for job files")
	poll := fs.Duration("poll", 3*time.Second, "wait between requests when there is no work")
	once := fs.Bool("once", false, "exit after one job")
	fs.Parse(args)
	if *srv == "" || *token == "" {
		fatalf("usage: catenaline worker -server host:port -token T [-name N] [-workdir D]")
	}
	base := *srv
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	c := &client{base: strings.TrimRight(base, "/"), token: *token, name: *name, http: &http.Client{Timeout: 10 * time.Minute}}
	log.Printf("worker %s polling %s", c.name, c.base)
	for {
		job, err := c.getJob()
		if err != nil {
			log.Printf("asking for work: %v", err)
		}
		if job == nil {
			time.Sleep(*poll)
			continue
		}
		dir := filepath.Join(*workdir, c.name, job.ID)
		res := runJob(job, dir, c.heartbeat)
		if err := c.sendResult(res); err != nil {
			log.Printf("job %s: result not delivered: %v (files kept in %s)", job.ID, err, dir)
		} else {
			os.RemoveAll(dir)
		}
		if *once {
			return
		}
	}
}

func (c *client) do(method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.http.Do(req)
}

func (c *client) getJob() (*Job, error) {
	resp, err := c.do("GET", "/job?worker="+c.name, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server said %s", resp.Status)
	}
	var job Job
	return &job, json.NewDecoder(resp.Body).Decode(&job)
}

// heartbeat tells the coordinator the job is alive; false = it was taken away.
func (c *client) heartbeat(id string) bool {
	resp, err := c.do("POST", "/heartbeat?id="+id, nil)
	if err != nil {
		return true // network hiccup: keep going
	}
	resp.Body.Close()
	return resp.StatusCode != http.StatusGone
}

func (c *client) sendResult(res Result) error {
	body, err := json.Marshal(res)
	if err != nil {
		return err
	}
	var last error
	for try := 0; try < 5; try++ {
		resp, err := c.do("POST", "/result", body)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusGone {
				return nil // Gone: the job was given to someone else; nothing to do
			}
			err = fmt.Errorf("server said %s", resp.Status)
		}
		last = err
		time.Sleep(5 * time.Second)
	}
	return last
}

// runJob runs one job in dir and returns the result to send back.
// beat is called every 30 s; when it returns false the program is stopped.
func runJob(job *Job, dir string, beat func(id string) bool) Result {
	res := Result{ID: job.ID, ExitCode: -1}
	a := job.Adapter
	if err := writeFiles(dir, job.Files); err != nil {
		res.Error = err.Error()
		return res
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if t, _ := a.TimeoutDuration(); t > 0 {
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	exe, args := a.CommandLine(job.Input)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	absDir, _ := filepath.Abs(dir)
	cmd.Env = append(os.Environ(), "TMPDIR="+absDir)
	for k, v := range a.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	killGroup(cmd) // stop the program's children too
	if a.Stdout != "" {
		f, err := os.Create(filepath.Join(dir, expand(a.Stdout, job.Input)))
		if err != nil {
			res.Error = err.Error()
			return res
		}
		defer f.Close()
		cmd.Stdout = f
	}
	if a.Stderr != "" {
		f, err := os.Create(filepath.Join(dir, expand(a.Stderr, job.Input)))
		if err != nil {
			res.Error = err.Error()
			return res
		}
		defer f.Close()
		cmd.Stderr = f
	}

	log.Printf("job %s: %s %s in %s", job.ID, exe, strings.Join(args, " "), dir)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Error = "could not start: " + err.Error()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var err error
wait:
	for {
		select {
		case err = <-done:
			break wait
		case <-tick.C:
			if !beat(job.ID) {
				log.Printf("job %s: taken away by the coordinator, stopping", job.ID)
				cancel()
			}
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Error = "timeout after " + a.Timeout
	} else if ctx.Err() == context.Canceled {
		res.Error = "cancelled"
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.Error = err.Error()
	}
	log.Printf("job %s: exit %d after %s", job.ID, res.ExitCode, time.Since(start).Round(time.Millisecond))

	res.Files = map[string][]byte{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.Type().IsRegular() || !a.Collected(job.Input, e.Name()) {
			continue
		}
		if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			res.Files[e.Name()] = data
		}
	}
	return res
}
