// catenaline: chains programs into pipelines and runs their steps on several computers (a proof of concept).
//
//	catenaline serve [-listen :8470] <dir>       coordinator with a spool in <dir>
//	catenaline worker -server host:port -token T  run jobs for a coordinator
//	catenaline status <dir>                      short summary of the spool
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "worker":
		cmdWorker(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  catenaline serve [-listen :8470] <dir>
  catenaline worker -server host:port -token T [-name N] [-workdir D] [-once]
  catenaline status <dir>`)
	os.Exit(2)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "catenaline: "+format+"\n", a...)
	os.Exit(1)
}
