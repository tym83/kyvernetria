/*
Copyright 2026 The Kyvernetria Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kyvctl

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// explanations turn common kubectl errors into sentences. The original
// message is always printed as well.
var explanations = []struct {
	re   *regexp.Regexp
	say  func(m []string) string
}{
	{regexp.MustCompile(`connection refused|dial tcp .*: connect: `), func([]string) string {
		return "The cluster isn't answering yet. If it's still starting up, give it a moment; otherwise check that the kubeconfig points at the right place."
	}},
	{regexp.MustCompile(`\(Forbidden\): .* User "([^"]+)" cannot (\w+) resource "([^"]+)"`), func(m []string) string {
		return fmt.Sprintf("You're signed in as %s, and that identity isn't allowed to %s %s here. Someone with more access can grant it; `kyvctl auth can-i --list` shows what you can do.", m[1], m[2], m[3])
	}},
	{regexp.MustCompile(`\(NotFound\): (\S+?) "([^"]+)" not found`), func(m []string) string {
		return fmt.Sprintf("I couldn't find %s %q in this namespace. It may live in another one: try adding -A, or check the name.", m[1], m[2])
	}},
	{regexp.MustCompile(`the server doesn't have a resource type "([^"]+)"`), func(m []string) string {
		return fmt.Sprintf("This cluster doesn't know the kind %q. `kyvctl api-resources` lists the kinds it knows.", m[1])
	}},
}

// Explain returns a sentence for a known error message, or "".
func Explain(msg string) string {
	for _, e := range explanations {
		if m := e.re.FindStringSubmatch(msg); m != nil {
			return e.say(m)
		}
	}
	return ""
}

// Failures remembers recent failed invocations to notice frustration: the
// same command failing again and again within a few minutes.
//
// Models: a small-to-moderate female advantage in recognizing emotions
// (Thompson & Voyer 2014). A CLI can't see a face; repetition is the one
// signal it has.
type Failures struct {
	Path   string
	Window time.Duration
	Now    func() time.Time
}

// DefaultFailures stores history under the user cache directory.
func DefaultFailures() *Failures {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return &Failures{Path: filepath.Join(dir, "kyvernetria", "failures"), Window: 3 * time.Minute, Now: time.Now}
}

// Record stores a failure of args and returns how many times the same
// command failed within the window, this one included.
func (f *Failures) Record(args []string) int {
	now := f.Now()
	cmd := strings.Join(args, " ")
	var kept []string
	count := 1
	if file, err := os.Open(f.Path); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			ts, prev, ok := strings.Cut(scanner.Text(), "\t")
			if !ok {
				continue
			}
			sec, err := strconv.ParseInt(ts, 10, 64)
			if err != nil || now.Sub(time.Unix(sec, 0)) > f.Window {
				continue
			}
			kept = append(kept, scanner.Text())
			if prev == cmd {
				count++
			}
		}
		_ = file.Close()
	}
	kept = append(kept, fmt.Sprintf("%d\t%s", now.Unix(), cmd))
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err == nil {
		_ = os.WriteFile(f.Path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
	}
	return count
}

// Comfort is what kyvctl says when it notices the same failure repeating.
func Comfort(count int, args []string) string {
	if count < 3 {
		return ""
	}
	hint := "`kyvctl calm` summarizes what's going wrong around you"
	for _, a := range args {
		if strings.Contains(a, "/") && !strings.HasPrefix(a, "-") {
			hint = fmt.Sprintf("`kyvctl remember %s` shows what has been happening to it", a)
			break
		}
	}
	return fmt.Sprintf("That's %d times in a row. Let's step back instead of retrying: %s.", count, hint)
}

// Fatal is installed as kubectl's fatal-error behavior.
func Fatal(args []string, failures *Failures) func(string, int) {
	return func(msg string, code int) {
		if len(msg) > 0 {
			if !strings.HasSuffix(msg, "\n") {
				msg += "\n"
			}
			fmt.Fprint(os.Stderr, msg)
			if say := Explain(msg); say != "" {
				fmt.Fprintf(os.Stderr, "\n%s\n", say)
			}
		}
		if comfort := Comfort(failures.Record(args), args); comfort != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", comfort)
		}
		os.Exit(code)
	}
}
