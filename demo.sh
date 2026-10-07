#!/bin/sh
# Runs the opt-sp-freq demo on this computer: one coordinator, two workers,
# three molecules. Everything goes into ./demo-run (deleted first).
# The processes started here are stopped at the end by their saved PIDs.
# ORCA is looked for in $ORCA_DIR, else on PATH, else in /opt/orca-6.1.1.
set -eu
cd "$(dirname "$0")"
if [ -n "${ORCA_DIR:-}" ]; then orca="$ORCA_DIR/orca"
elif command -v orca >/dev/null; then orca=$(command -v orca)
else orca=/opt/orca-6.1.1/orca; fi
if [ ! -x "$orca" ]; then
	echo "ORCA not found ($orca): put its folder in ORCA_DIR, e.g. ORCA_DIR=\$HOME/orca ./demo.sh" >&2
	exit 1
fi
# ORCA for macOS ships no xtb (otool_xtb): an xtb on PATH is used instead
if [ ! -x "$(dirname "$orca")/otool_xtb" ] && ! command -v xtb >/dev/null; then
	echo "xtb not found: ORCA for macOS has no otool_xtb; install xtb so that it is on PATH" >&2
	echo "(e.g. conda install -c conda-forge xtb, or brew install xtb), then run this again" >&2
	exit 1
fi
go build -o catenaline .
rm -rf demo-run
mkdir -p demo-run/spool/pipelines
cp examples/pipelines/opt-sp-freq.json demo-run/spool/pipelines/

./catenaline serve -listen 127.0.0.1:8470 demo-run/spool > demo-run/serve.log 2>&1 &
SERVE=$!
PIDS="$SERVE"
trap 'kill $PIDS 2>/dev/null || true' EXIT INT TERM
# alive <seconds since start> <limit>: fails when the coordinator stopped or time is up
alive() {
	if ! kill -0 "$SERVE" 2>/dev/null; then
		echo "the coordinator stopped:" >&2; tail -5 demo-run/serve.log >&2; exit 1
	fi
	if [ "$1" -gt "$2" ]; then echo "gave up after $2 s; see demo-run/*.log" >&2; exit 1; fi
}
t0=$(date +%s)
while [ ! -s demo-run/spool/token ]; do alive $(( $(date +%s) - t0 )) 30; sleep 0.2; done
TOKEN=$(cat demo-run/spool/token)
for w in a b; do
	./catenaline worker -server 127.0.0.1:8470 -token "$TOKEN" -name "worker-$w" \
		-workdir demo-run/work > "demo-run/worker-$w.log" 2>&1 &
	PIDS="$PIDS $!"
done
echo "coordinator and workers started (PIDs $PIDS)"

while [ ! -d demo-run/spool/input/opt-sp-freq ]; do alive $(( $(date +%s) - t0 )) 30; sleep 0.2; done
cp examples/molecules/*.xyz demo-run/spool/input/opt-sp-freq/
start=$(date +%s)

# Wait until nothing is waiting or running.
sleep 4
while [ -n "$(ls -A demo-run/spool/input/opt-sp-freq)" ] || [ -n "$(ls -A demo-run/spool/running/opt-sp-freq 2>/dev/null)" ]; do
	alive $(( $(date +%s) - start )) 1800
	sleep 2
done
echo "all done in $(( $(date +%s) - start )) s"
./catenaline status demo-run/spool

echo
echo "energies (Eh):"
printf "%-10s %16s %18s %12s %16s %6s\n" molecule "xtb GFN2" "r2SCAN-3c SP" ZPE "r2SCAN-3c G" imag
for d in demo-run/spool/output/opt-sp-freq/*/; do
	[ -f "$d"step3-orca/"$(basename "$d")".out ] || continue # failed: listed below
	m=$(basename "$d")
	xtb=$(awk '/TOTAL ENERGY/ {print $4}' "$d"step1-xtb/"$m".out | tail -1)
	sp=$(awk '/FINAL SINGLE POINT ENERGY/ {print $5}' "$d"step2-orca/"$m".out | tail -1)
	zpe=$(awk '/Zero point energy/ {print $5}' "$d"step3-orca/"$m".out | tail -1)
	g=$(awk '/Final Gibbs free energy/ {print $6}' "$d"step3-orca/"$m".out | tail -1)
	imag=$(grep -c '\*\*\*imaginary mode\*\*\*' "$d"step3-orca/"$m".out || true)
	printf "%-10s %16s %18s %12s %16s %6s\n" "$m" "$xtb" "$sp" "$zpe" "$g" "$imag"
done
if [ -n "$(ls -A demo-run/spool/errors 2>/dev/null)" ]; then
	echo
	echo "some steps failed (demo-run/spool/errors):"
	find demo-run/spool/errors -name REASON.txt -exec sh -c 'echo "$1:"; head -3 "$1"' _ {} \;
	exit 1
fi
