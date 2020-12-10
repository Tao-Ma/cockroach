// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// testfilter is a utility to manipulate JSON streams in [test2json] format.
// Standard input is read and each line starting with `{` and ending with `}`
// parsed (and expected to parse successfully). Lines not matching this pattern
// are classified as output not related to the test and, depending on the args
// passed to `testfilter`, are passed through or removed. The arguments available
// are `--mode=(strip|omit|convert)`, where:
//
// strip: omit output for non-failing tests, pass everything else through. In
//   particular, non-test output and tests that never terminate are passed through.
// omit: print only failing tests. Note that test2json does not close scopes for
//   tests that are running in parallel (in the same package) with a "foreground"
//   test that panics, so it will pass through *only* the one foreground test.
//   Note also that package scopes are omitted; test2json does not reliably close
//   them on panic/Exit anyway.
// convert:
//   no filtering is performed, but any test2json input is translated back into
//   its pure Go test framework text representation. This is useful for output
//   intended for human eyes.
//
// [test2json]: https://golang.org/cmd/test2json/
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

const modeUsage = `strip:
  omit output for non-failing tests, but print run/pass/skip events for all tests
omit:
  only emit failing tests
convert:
  don't perform any filtering, simply convert the json back to original test format'
`

type modeT byte

const (
	modeStrip modeT = iota
	modeOmit
	modeConvert
)

func (m *modeT) Set(s string) error {
	switch s {
	case "strip":
		*m = modeStrip
	case "omit":
		*m = modeOmit
	case "convert":
		*m = modeConvert
	default:
		return errors.New("unsupported mode")
	}
	return nil
}

func (m *modeT) String() string {
	switch *m {
	case modeStrip:
		return "strip"
	case modeOmit:
		return "omit"
	case modeConvert:
		return "convert"
	default:
		return "unknown"
	}
}

var modeVar = modeStrip

func init() {
	flag.Var(&modeVar, "mode", modeUsage)
}

type testEvent struct {
	Time    time.Time // encodes as an RFC3339-format string
	Action  string
	Package string
	Test    string
	Elapsed float64 // seconds
	Output  string
}

func main() {
	flag.Parse()
	if err := filter(os.Stdin, os.Stdout, modeVar); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func filter(in io.Reader, out io.Writer, mode modeT) error {
	scanner := bufio.NewScanner(in)
	type tup struct {
		pkg  string
		test string
	}
	type ent struct {
		first, last     string // RUN and (SKIP|PASS|FAIL)
		strings.Builder        // output
	}
	m := map[tup]*ent{}
	ev := &testEvent{}
	var n int               // number of JSON lines parsed
	var passFailLine string // catch common error of piping non-json test output in
	for scanner.Scan() {
		line := scanner.Text() // has no EOL marker
		if len(line) <= 2 || line[0] != '{' || line[len(line)-1] != '}' {
			// Not test2json output, pass it through except in `omit` mode.
			// It's important that we still see build errors etc when running
			// in -mode=strip.
			if passFailLine == "" && (strings.Contains(line, "PASS") || strings.Contains(line, "FAIL")) {
				passFailLine = line
			}
			if mode != modeOmit {
				fmt.Fprintln(out, line)
			}
			continue
		}
		*ev = testEvent{}
		if err := json.Unmarshal([]byte(line), ev); err != nil {
			return err
		}
		n++

		if mode == modeConvert {
			if ev.Action == "output" {
				fmt.Fprint(out, ev.Output)
			}
			continue
		}

		if ev.Test == "" {
			if mode == modeOmit && ev.Action != "fail" {
				// Skip all regular package output when omitting. There's not much
				// package output typically (only init functions), so not worth
				// getting fancy about.
				continue
			}

			// Populate a fake test name. We need this because if a
			// package fails but no test has been marked as failed before,
			// TC simply omits the package entirely in the overview.
			ev.Test = "PackageLevel"

			const helpMessage = "\nCheck full_output.txt in artifacts for stray panics or other errors that broke the test process."
			if ev.Action == "fail" {
				// Populate a helper annotation, to guide the user towards the right place
				ev.Output += helpMessage
			}

			pkey := tup{ev.Package, ev.Test}
			if m[pkey] == nil {
				// We are observing the first entry for a package. We are
				// going to turn the package entry into a pseudo-test entry
				// below. We also need to synthetize a "run" entry for the
				// pseudo-test. We need this, because TC does not recognize a
				// test unless it starts with a "run" entry.
				evCopy := *ev
				evCopy.Action = "run"
				evCopy.Output = ""
				b, err := json.Marshal(&evCopy)
				if err != nil {
					fmt.Fprintf(out, "ERROR: %v\n", err)
				} else {
					fakeLine := string(b)
					buf := &ent{first: fakeLine}
					if _, err := fmt.Fprintln(buf, fakeLine); err != nil {
						return err
					}
					m[pkey] = buf
				}
			}

			if ev.Action == "fail" {
				// A package is terminating. Dump all the test scopes so far
				// in this package, then forget about them. This ensures that
				// the test failures are reported before the package failure
				// in the final output.
				var testReport strings.Builder
				for key := range m {
					if key.pkg == ev.Package && key.test != ev.Test {
						// We only mention the test scopes without their sub-tests;
						// otherwise we could get tens of thousands of output lines
						// for a failed logic test run due to a panic.
						if !strings.Contains(key.test, "/") {
							buf := m[key]
							// Remember the test's name to report in the
							// package-level output.
							testReport.WriteString("\n" + key.test)

							// Synthetize a "skip" message.
							//
							// We use "skip" and not "fail" to ensure that no issue
							// gets filed for the open-ended tests by the github
							// auto-poster: we don't have confidence for any of them
							// that they are the particular cause of the failure.
							synthEv := testEvent{Time: ev.Time, Action: "skip", Package: ev.Package, Test: key.test, Elapsed: 0, Output: "unfinished due to package-level failure" + helpMessage}
							b, err := json.Marshal(&synthEv)
							if err != nil {
								fmt.Fprintf(out, "ERROR: %v\n", err)
							} else {
								fmt.Fprintln(buf, string(b))
							}
							fmt.Fprintln(out, buf.String())
						}
						// In any case, remove the entries for all the tests
						// "under" the package. We know they won't get any more
						// output because the package fail entry can only appear
						// after all tests have finalized.
						delete(m, key)
					}
				}

				// Report the list of open-ended tests.
				if testReport.Len() > 0 {
					ev.Output += "\nThe following tests have not completed and could be the cause of the failure:" + testReport.String()
				}
			}

			// Re-populate the line from the JSON payload.
			b, err := json.Marshal(ev)
			if err != nil {
				fmt.Fprintf(out, "ERROR: %v\n", err)
			} else {
				line = string(b)
			}
		}

		key := tup{ev.Package, ev.Test}
		buf := m[key]
		if buf == nil {
			buf = &ent{first: line}
			m[key] = buf
		}
		if _, err := fmt.Fprintln(buf, line); err != nil {
			return err
		}
		switch ev.Action {
		case "pass", "skip", "fail":
			buf.last = line
			delete(m, key)
			if ev.Action == "fail" {
				fmt.Fprint(out, buf.String())
			} else if mode == modeStrip {
				// Output only the start and end of test so that we preserve the
				// timing information. However, the output is omitted.
				fmt.Fprintln(out, buf.first)
				fmt.Fprintln(out, buf.last)
			}
		case "run", "pause", "cont", "bench", "output":
		default:
			// We must have parsed some JSON that wasn't a testData.
			return fmt.Errorf("unknown input: %s", line)
		}
	}
	// Some scopes might still be open. To the best of my knowledge, this is due
	// to a panic/premature exit of a test binary. In that case, it seems that
	// neither is the package scope closed, nor the scopes for any tests that
	// were running in parallel, so we pass that through if stripping, but not
	// when omitting.
	if mode == modeStrip {
		for key := range m {
			fmt.Fprintln(out, m[key].String())
		}
	}
	// TODO(tbg): would like to return an error here for sanity, but the
	// JSON just isn't well-formed all the time. For example, at the time
	// of writing, here's a repro:
	// make benchshort PKG=./pkg/bench BENCHES=BenchmarkIndexJoin 2>&1 | \
	// testfilter -mode=strip
	// Interestingly it works once we remove the `log.Scope(b).Close` in
	// that test. Adding TESTFLAGS=-v doesn't matter apparently.
	// if len(m) != 0 {
	// 	return fmt.Errorf("%d tests did not terminate (a package likely exited prematurely)", len(m))
	// }
	if mode != modeConvert && n == 0 && passFailLine != "" {
		// Without this, if the input to this command wasn't even JSON, we would
		// pass. That's a mistake we should avoid at all costs. Note that even
		// `go test -run - ./some/pkg` produces n>0 due to the start/pass events
		// for the package, so if we're here then 100% something weird is going
		// on.
		return fmt.Errorf("not a single test was parsed, but detected test output: %s", passFailLine)
	}
	return nil
}
