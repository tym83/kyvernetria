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
	"crypto/sha256"
	"encoding/hex"
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
	re  *regexp.Regexp
	say func(m []string) string
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
// Models: a small average female advantage in recognizing emotions
// (d ≈ 0.19; Thompson & Voyer 2014). A CLI can't see a face; repetition
// is the one signal it has.
//
// Only a SHA-256 of a redacted command line is stored, never the command
// itself: command lines carry tokens, passwords and secret literals.
type Failures struct {
	// Path of the history file; empty disables the history.
	Path   string
	Window time.Duration
	Now    func() time.Time
}

// DefaultFailures stores history under the user cache directory. Without
// one there is no history: a shared directory such as /tmp is not safe for
// it.
func DefaultFailures() *Failures {
	f := &Failures{Window: 3 * time.Minute, Now: time.Now}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		f.Path = filepath.Join(dir, "kyvernetria", "failures")
	}
	return f
}

// keptFlagValues are flags whose values say which command it was and hold
// nothing secret. Every other flag's value is dropped before hashing.
var keptFlagValues = map[string]bool{
	"-n": true, "--namespace": true, "--context": true, "--cluster": true,
	"-o": true, "--output": true, "-l": true, "--selector": true, "-f": true, "--filename": true,
}

// Fingerprint identifies a command line without keeping it: flag values
// other than a few harmless ones and everything after "--" are dropped,
// and the rest is hashed.
func Fingerprint(args []string) string {
	var kept []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			kept = append(kept, a)
			continue
		}
		name, _, hasValue := strings.Cut(a, "=")
		keep := keptFlagValues[name]
		switch {
		case hasValue && keep:
			kept = append(kept, a)
		case hasValue:
			kept = append(kept, name)
		default:
			kept = append(kept, name)
			// The next argument may be this flag's value.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				if keep {
					kept = append(kept, args[i])
				}
			}
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(kept, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Record stores a failure of args and returns how many times the same
// command failed within the window, this one included.
func (f *Failures) Record(args []string) int {
	if f.Path == "" {
		return 1
	}
	now := f.Now()
	cmd := Fingerprint(args)
	dir := filepath.Dir(f.Path)
	if err := privateDir(dir); err != nil {
		return 1
	}
	var kept []string
	count := 1
	if file, err := openNoFollow(f.Path); err == nil {
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
	_ = writeAtomically(dir, f.Path, []byte(strings.Join(kept, "\n")+"\n"))
	return count
}

// privateDir makes sure dir exists, is a real directory (not a symlink),
// belongs to the current user and is closed to others.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%s belongs to someone else", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// writeAtomically replaces path with data through a fresh temporary file,
// so a crash or a concurrent kyvctl never leaves a half-written history and
// a symlink at path is replaced rather than followed.
func writeAtomically(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".failures-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Comfort is what kyvctl says when it notices the same failure repeating.
func Comfort(count int, args []string) string {
	if count < 3 {
		return ""
	}
	hint := "`kyvctl calm` summarizes what's going wrong around you"
	// Only positional arguments name objects; a flag value such as
	// --kubeconfig /path/to/file must never be mistaken for one.
	for _, a := range positionals(args, globalFlags()) {
		if strings.Contains(a, "/") {
			hint = fmt.Sprintf("`kyvctl remember %s` shows what has been happening to it", a)
			break
		}
	}
	return fmt.Sprintf("That's %d times in the last few minutes. Let's step back instead of retrying: %s.", count, hint)
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
