# Catenaline

[![DOI](https://zenodo.org/badge/DOI/10.5281/zenodo.23225204.svg)](https://doi.org/10.5281/zenodo.23225204)

**Catenaline** is a proof of concept. It
answers two questions about Matriline, which spreads ORCA jobs over
volunteer computers:

1. Can the same design run other programs that read one input file and
   write one or more output files (xtb, LAMMPS, a shell script, ...)?
2. Can programs be chained, so the output of one becomes the input of the
   next?

Both answers are yes. Catenaline does it in about 1,150 lines of Go (standard
library only). It is a sketch, not a product: **security is not taken
seriously here** (see [Limits](#limits)).

## How it works

One binary, three commands:

```
catenaline serve [-listen :8470] <dir>                 the coordinator
catenaline worker -server host:port -token T [-name N] [-workdir D]
catenaline status <dir>                                a short summary
```

The coordinator keeps a spool in `<dir>`:

```
<dir>/token                     shared token, made on first start
<dir>/programs.json             program adapters (defaults written on first start)
<dir>/pipelines/<name>.json     pipelines
<dir>/input/<pipeline>/         drop a file here to start that pipeline
<dir>/running/<pipeline>/<name>/   state.json (current step) + the original file
<dir>/output/<pipeline>/<name>/step<N>-<adapter>/   each step's returned files
<dir>/output/<pipeline>/<name>/original/            the dropped file, when all steps are done
<dir>/errors/<pipeline>/<name>/    REASON.txt, the failing step's files, the original
```

Every adapter also works as a one-step pipeline: a file dropped in
`input/xtb/` runs xtb alone.

Workers pull: they ask `GET /job` every few seconds, run the job in a fresh
folder, send a heartbeat every 30 s, and post all the collected files with
the exit code to `POST /result`. The coordinator, not the worker, decides
whether the step succeeded (it applies the adapter's check to the returned
files). Then it builds the next step's input from those files and queues it.
Several workers may run on the same computer. The worker gets the whole
adapter with each job, so workers need no configuration.

## Compared with Matriline

Ideas that carry over unchanged:

- **Spool folders** `input/ → running/ → output/` (+ `errors/`): the user
  works with files and folders only.
- **Pull-based workers**: workers connect out to the coordinator, so they can
  sit behind NAT and need no open port.
- **Return all files**, not only the main output (here the `.gbw`, `.hess`,
  `xtbopt.xyz`...), so a later step or a person can use them.
- **Checkpoints**: `running/.../state.json` records the current step; a
  restarted coordinator continues every chain at that step. A worker that
  goes silent for 2 minutes loses its job, which is queued again.
- **The success check belongs to the program**: Matriline looks for
  `ORCA TERMINATED NORMALLY`; Catenaline makes that a setting per program.

What Catenaline does not have (Matriline does):

- authentication beyond one shared token; no per-user or per-computer keys,
  no way to revoke one helper;
- TLS: everything, token included, travels in clear HTTP;
- a sandbox: **the worker runs whatever command the coordinator sends**. A
  Matriline client only ever runs ORCA. Anyone with the token who controls
  the coordinator controls the workers;
- result verification (re-running jobs on another computer, comparing
  results) and fingerprints of the inputs and outputs;
- a relay, a web page, services, translations, limits per helper, ORCA
  restart from orbitals after a timeout.

Adding other programs to Matriline would mean replacing its ORCA-specific
parts (input parsing, `TERMINATED NORMALLY`, the orbitals files, the
fingerprints of an ORCA run) with an adapter like the one below, and
deciding how far a client should trust an adapter it did not write. The
easy safe choice is a list of allowed adapters on the client.

## Adding a program

Add an entry to `<dir>/programs.json` (it is read when the coordinator
starts). Text fields may use `{input}` (the input file name, e.g.
`water.xyz`) and `{stem}` (the same without the extension, `water`).

```json
"xtb": {
  "command": "/opt/orca-6.1.1/otool_xtb",
  "args": ["{input}", "--opt"],
  "input_ext": ".xyz",
  "stdout": "{stem}.out",
  "stderr": "{stem}.err",
  "env": {"OMP_NUM_THREADS": "1"},
  "success": {"exit_code": 0, "file": "{stem}.err", "regex": "normal termination of xtb"},
  "collect": ["xtbopt.xyz", "xtbopt.log", "charges", "wbo", "{stem}.out", "{stem}.err"],
  "timeout": "30m"
}
```

| field | meaning |
|---|---|
| `command` | the executable; a full path is safest (ORCA requires it) |
| `args` | arguments, with `{input}` and `{stem}` |
| `input_ext` | extension given to the input file (`.inp`, `.xyz`, `.in`) |
| `stdout`, `stderr` | files that receive the program's output streams (optional) |
| `env` | extra environment variables (optional) |
| `success.exit_code` | required exit code (optional) |
| `success.file` + `success.regex` | a regular expression that must appear in that file (optional; at least one check is required) |
| `collect` | globs of files to send back; empty = everything. The stdout, stderr and check files always come back |
| `timeout` | Go duration (`90s`, `30m`, `2h`); empty = no limit |
| `note` | free text |

The default `programs.json` has four adapters. ORCA's folder in it is the one in
`ORCA_DIR` when the coordinator first starts, else the folder of the `orca` found
on `PATH`, else `/opt/orca-6.1.1`:

- **orca**: `/opt/orca-6.1.1/orca {input}`, stdout to `{stem}.out`, success =
  exit code 0 and `ORCA TERMINATED NORMALLY` in `{stem}.out`.
- **xtb**: ORCA's bundled `otool_xtb {input} --opt` (ORCA for macOS has none: there an
  `xtb` found on `PATH` is used, e.g. from `conda install -c conda-forge xtb`). Run once on this
  machine: it writes `xtbopt.xyz` (the optimized geometry, with the energy on
  the comment line), `xtbopt.log`, `charges`, `wbo`, `xtbrestart` and
  `xtbtopo.mol`, prints the results on stdout and **`normal termination of
  xtb` on stderr**, so that is where the check looks.
- **shell**: `/bin/sh {input}` with success = exit code 0. An example for
  programs that are not installed: a script can call anything.
- **lammps**: `lmp -in {input} -log {stem}.log`, success = `Total wall time`
  in the log. **Untested**: LAMMPS is not installed on the development
  machine.

## Writing a pipeline

A pipeline is `<dir>/pipelines/<name>.json`, a list of steps. The file
dropped into `input/<name>/` is "step 0". Each step names an adapter and
builds its input from the files of a source step:

| field | meaning |
|---|---|
| `adapter` | which program to run |
| `from` | source step number; absent = the previous step, `0` = the dropped file |
| `input_file` | pass this file (name or glob, exactly one match) from the source step as the input |
| `template` | or write these lines as the input; `{stem}` is the job name, `{file:NAME}` the content of NAME from the source step, `{xyz:NAME}` the coordinate lines of an `.xyz` file (without its first two lines) |
| `ship` | more files from the source step to send along, for inputs that refer to them |

A step with neither `input_file` nor `template` takes the dropped file
(only for step 1, or with `"from": 0`). The input is named
`<name><input_ext>`, so `water.xyz` becomes `water.inp` for ORCA.

The demo pipeline, `examples/pipelines/opt-sp-freq.json`:

```json
{
  "steps": [
    {"adapter": "xtb"},
    {"adapter": "orca",
     "template": ["! r2SCAN-3c", "%maxcore 500", "%pal nprocs 1 end",
                  "* xyzfile 0 1 xtbopt.xyz"],
     "ship": ["xtbopt.xyz"]},
    {"adapter": "orca", "from": 1,
     "template": ["! r2SCAN-3c Freq", "%maxcore 500", "%pal nprocs 1 end",
                  "* xyz 0 1", "{xyz:xtbopt.xyz}", "*"]}
  ]
}
```

Step 2 refers to the xtb geometry and ships the file; step 3 takes the same
file from step 1 (not from step 2) and embeds the coordinates in the input.
Both ways give ORCA the same geometry, and indeed the single-point energy
printed by step 3 equals step 2's.

When a step fails (bad exit code, check not found, timeout, a file the next
step needs is missing), the run moves to `errors/<pipeline>/<name>/` with
`REASON.txt` and that step's files, and the chain stops. Earlier steps stay
in `output/`.

## Demo

`./demo.sh` builds Catenaline, starts a coordinator on `127.0.0.1:8470` and two
workers on this computer, drops `water.xyz`, `methanol.xyz` and
`ethanol.xyz` (in `examples/molecules/`) into `input/opt-sp-freq/`, waits,
prints the status and the energies, and stops the three processes by their
PIDs. Everything goes into `demo-run/`. ORCA is looked for as above; when it is
elsewhere, give its folder: `ORCA_DIR=$HOME/orca ./demo.sh`. The demo stops with
exit code 1 when ORCA is missing, the coordinator stops, a step fails (each
reason is printed) or nothing is done after 30 minutes.

Run on 2026-10-07 (ORCA 6.1.1, xtb 6.7.1, one core per job, `%maxcore 500`):
all nine steps done in 20 s; the two workers shared them (5 and 4 jobs). A second run gave the same energies.

| molecule | xtb GFN2 (Eh) | r2SCAN-3c single point (Eh) | ZPE (Eh) | r2SCAN-3c G, 298 K (Eh) | imaginary freq. |
|---|---|---|---|---|---|
| water | -5.070544374391 | -76.418688384198 | 0.02145945 | -76.41485411 | 0 |
| methanol | -8.226118334032 | -115.696237005915 | 0.05148231 | -115.66746457 | 0 |
| ethanol | -11.391867019715 | -155.001867803094 | 0.07972024 | -154.94791966 | 0 |

No imaginary frequencies: the xtb geometries are minima at the r2SCAN-3c
level too (close enough for this demo; they were not re-optimized).

Also tried by hand on this machine:

- an ORCA input with an unknown keyword dropped in `input/orca/`: it went to
  `errors/orca/bad/` with "step 1 (orca) on worker w: exit code 4, expected 0";
- a wrong token: HTTP 401;
- stopping the coordinator during ethanol's frequency step and starting it
  again: it said "continuing at step 3", refused the old result (the job it
  belonged to no longer existed), handed step 3 out again and finished the
  chain. The work of that step was done twice.

## Tests

`go test ./...` (no ORCA needed):

- adapter templating (`{input}`, `{stem}`), collect globs and success checks;
- that the default `programs.json` loads;
- pipeline input building: `input_file`, `template` with `{file:}` and
  `{xyz:}`, `ship`, `from`, and validation errors;
- a full chain over HTTP with a real coordinator and worker and a shell
  script as the program, plus a chain whose second step fails and stops.

## Limits

This is a proof of concept:

- **Not secure.** One shared token over plain HTTP; workers run any command
  the coordinator sends; returned files are trusted (only their names are
  cleaned). Use it only on a network and with people you trust.
- Files travel as base64 inside JSON and are held in memory: fine for
  megabytes, not for gigabytes.
- One input file per run and a linear chain: no fan-out (one step feeding
  several parallel jobs), no joins, no conditions.
- The queue is in memory; only the chain position is saved. A job that was
  running when the coordinator stopped is run again from the start.
- No limits per worker, no priorities, no retries (a failed step needs a new
  drop), no web page.
- Workers trust the adapter they receive; different computers with programs
  in different places would need a per-worker path setting.
- Linux tested only (the process-group kill is Unix-only).

## The family

Catenaline is the third of three related projects by the same author, all under the same
license:

- [Matriline](https://github.com/agaloya/matriline): ORCA jobs on many computers, with
  security and verification of results from machines you do not control.
- [Nacomline](https://github.com/agaloya/nacomline): ORCA jobs on one computer.
- Catenaline (this one): a proof of concept for any program, and for chains of programs.

## License

AGPL-3.0 with an author-attribution additional term (see [docs/NOTICE](docs/NOTICE)).
If you use it in scientific work, please cite it as described in [CITATION.cff](CITATION.cff).

## Why "Catenaline"

From Latin *catena*, a chain: Catenaline links programs so that the output of one becomes
the input of the next. The ending *-line* follows its siblings Matriline and Nacomline.

