// Command viiwork-release signs and checks viiwork releases on the publisher's
// machine. It is not shipped: nodes verify with internal/release directly.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/janit/viiwork/v2/internal/release"
)

const usage = `usage: viiwork-release <command> [flags]

  keygen  [--key PATH] [--pub PATH]            make the release key pair; refuses to overwrite
  sign    --version V [--key PATH] [--out PATH] SHA256SUMS
                                               write SHA256SUMS.sig for release V
  verify  --version V [--pub PATH] SHA256SUMS SIG
                                               check a signature (compiled-in keys unless --pub)
  compare ARCHIVE DIR                           the archive holds exactly DIR's files, byte for byte

The private key defaults to ~/.config/viiwork/release.key and never leaves this
machine. See docs/releases.md.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:], stdout, stderr)
	case "sign":
		return sign(args[1:], stdout, stderr)
	case "verify":
		return verify(args[1:], stdout, stderr)
	case "compare":
		return compare(args[1:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func defaultKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "release.key"
	}
	return filepath.Join(home, ".config", "viiwork", "release.key")
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

func fail(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "viiwork-release: "+format+"\n", a...)
	return 1
}

func keygen(args []string, stdout, stderr io.Writer) int {
	fs := flags("keygen", stderr)
	key := fs.String("key", defaultKey(), "")
	pub := fs.String("pub", filepath.Join("internal", "release", "keys", "release.pub"), "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if _, err := os.Stat(*key); err == nil {
		return fail(stderr, "refusing to overwrite %s: every node trusts the key it holds", *key)
	}
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*key), 0o700); err != nil {
		return fail(stderr, "%v", err)
	}
	f, err := os.OpenFile(*key, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if _, err := f.Write(release.MarshalPrivateKey(sk)); err != nil {
		f.Close()
		return fail(stderr, "%v", err)
	}
	if err := f.Close(); err != nil {
		return fail(stderr, "%v", err)
	}
	if err := os.WriteFile(*pub, release.MarshalPublicKey(pk), 0o644); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "private key: %s (back it up; it must never leave this machine)\npublic key:  %s (commit it)\n", *key, *pub)
	return 0
}

func readKey(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others: chmod 600 it", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return release.ParsePrivateKey(b)
}

func sign(args []string, stdout, stderr io.Writer) int {
	fs := flags("sign", stderr)
	key := fs.String("key", defaultKey(), "")
	out := fs.String("out", "", "")
	version := fs.String("version", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 1 || *version == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sumsPath := fs.Arg(0)
	if *out == "" {
		*out = sumsPath + ".sig"
	}
	sk, err := readKey(*key)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	parsed, err := release.ParseSums(sums)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if err := release.CheckSumsFor(*version, parsed); err != nil {
		return fail(stderr, "%v", err)
	}
	sig := release.Sign(sk, *version, sums)
	if err := release.Verify([]ed25519.PublicKey{sk.Public().(ed25519.PublicKey)}, *version, sums, sig); err != nil {
		return fail(stderr, "self-check: %v", err)
	}
	if err := os.WriteFile(*out, sig, 0o644); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "signed %s -> %s\n", sumsPath, *out)
	return 0
}

func verify(args []string, stdout, stderr io.Writer) int {
	fs := flags("verify", stderr)
	pub := fs.String("pub", "", "")
	version := fs.String("version", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 2 || *version == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var keys []ed25519.PublicKey
	if *pub != "" {
		b, err := os.ReadFile(*pub)
		if err != nil {
			return fail(stderr, "%v", err)
		}
		k, err := release.ParsePublicKey(b)
		if err != nil {
			return fail(stderr, "%s: %v", *pub, err)
		}
		keys = []ed25519.PublicKey{k}
	} else {
		var err error
		if keys, err = release.Keys(); err != nil {
			return fail(stderr, "%v", err)
		}
	}
	sums, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return fail(stderr, "%v", err)
	}
	sig, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return fail(stderr, "%v", err)
	}
	parsed, err := release.ParseSums(sums)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if err := release.CheckSumsFor(*version, parsed); err != nil {
		return fail(stderr, "%v", err)
	}
	if err := release.Verify(keys, *version, sums, sig); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintln(stdout, "signature ok")
	return 0
}

// compare checks that ARCHIVE holds exactly DIR's regular files under one top
// directory, byte for byte. Contents are compared, not archives, so tar and
// gzip metadata cannot cause a false mismatch; anything but a regular file or
// the top directory in the archive is a failure, since nothing else belongs in
// a release.
func compare(args []string, stdout, stderr io.Writer) int {
	fs := flags("compare", stderr)
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return fail(stderr, "%v", err)
	}
	defer f.Close()
	archived, err := release.ReadArchive(f, filepath.Base(filepath.Clean(fs.Arg(1))), 1<<30)
	if err != nil {
		return fail(stderr, "%s: %v", fs.Arg(0), err)
	}
	local, err := readDir(fs.Arg(1))
	if err != nil {
		return fail(stderr, "%s: %v", fs.Arg(1), err)
	}
	var problems []string
	for name, b := range archived {
		l, ok := local[name]
		switch {
		case !ok:
			problems = append(problems, "extra in archive: "+name)
		case !bytes.Equal(b, l):
			problems = append(problems, name+": differs")
		}
	}
	for name := range local {
		if _, ok := archived[name]; !ok {
			problems = append(problems, "missing from archive: "+name)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return fail(stderr, "%s does not match %s", fs.Arg(0), fs.Arg(1))
	}
	fmt.Fprintf(stdout, "%s matches %s (%d files)\n", filepath.Base(fs.Arg(0)), fs.Arg(1), len(local))
	return 0
}

func readDir(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	return files, err
}
