package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// base は検査を通る設定を返す。各テストはこれを1か所だけ壊す。
func base(t *testing.T) (dir string, yaml string) {
	t.Helper()
	dir = t.TempDir()
	for _, p := range []string{"source/music", "source/bob", "library"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	yaml = fmt.Sprintf(`
listen: ":8080"
sources:
  - name: music
    path: %[1]s/source/music
    defaultLibraries: [shared]
    allowedLibraries: [shared, alice]
  - name: bob-music
    path: %[1]s/source/bob
    defaultLibraries: [bob]
    allowedLibraries: [bob]
libraryRoot: %[1]s/library
libraries:
  - name: shared
    path: %[1]s/library/shared
  - name: alice
    path: %[1]s/library/alice
  - name: bob
    path: %[1]s/library/bob
workDir: %[1]s/library/.curator
auth:
  mode: none
`, dir)
	return dir, yaml
}

func load(t *testing.T, y string) (*Config, error) {
	t.Helper()
	c, err := Parse([]byte(y))
	if err != nil {
		return nil, err
	}
	return c, c.Prepare()
}

func TestValidConfig(t *testing.T) {
	dir, y := base(t)
	c, err := load(t, y)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// 既定値
	if c.Index.RefreshInterval.D() != 6*time.Hour || c.Copy.Concurrency != 1 || c.Copy.FreeSpaceMargin != 5<<30 ||
		c.Copy.FileMode != 0o644 || c.Copy.DirMode != 0o755 || c.Trash.Retention.D() != 168*time.Hour ||
		c.Jobs.HistoryLimit != 500 || c.Notify.Webhook.Debounce.D() != 30*time.Second || !*c.ExcludeDotDirectories {
		t.Errorf("defaults not applied: %+v", c)
	}
	if len(c.AudioExtensions) != 14 || len(c.Exclude) != 8 {
		t.Errorf("default lists: %v %v", c.AudioExtensions, c.Exclude)
	}
	// 存在しない library と作業領域は作る。
	for _, p := range []string{"library/shared", "library/alice", "library/bob", "library/.curator"} {
		if fi, err := os.Stat(filepath.Join(dir, p)); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created: %v", p, err)
		}
	}
}

func TestInvalidConfigs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dir, y string) string
		want   string
	}{
		{
			name:   "library outside libraryRoot",
			mutate: func(dir, y string) string { return strings.Replace(y, dir+"/library/alice", dir+"/elsewhere/alice", 1) },
			want:   "must be inside libraryRoot",
		},
		{
			name: "workDir outside libraryRoot",
			mutate: func(dir, y string) string {
				return strings.Replace(y, "workDir: "+dir+"/library/.curator", "workDir: "+dir+"/work", 1)
			},
			want: "workDir",
		},
		{
			name: "workDir inside a library",
			mutate: func(dir, y string) string {
				return strings.Replace(y, "workDir: "+dir+"/library/.curator", "workDir: "+dir+"/library/shared/.curator", 1)
			},
			want: "must not be inside library",
		},
		{
			name: "libraries overlap",
			mutate: func(dir, y string) string {
				return strings.Replace(y, dir+"/library/alice", dir+"/library/shared/alice", 1)
			},
			want: "overlap",
		},
		{
			name:   "library equals another",
			mutate: func(dir, y string) string { return strings.Replace(y, dir+"/library/alice", dir+"/library/shared", 1) },
			want:   "overlap",
		},
		{
			name: "source overlaps a library",
			mutate: func(dir, y string) string {
				return strings.Replace(y, "path: "+dir+"/source/bob", "path: "+dir+"/library/bob/src", 1)
			},
			want: "overlaps library",
		},
		{
			name:   "library inside source",
			mutate: func(dir, y string) string { return strings.Replace(y, "path: "+dir+"/source/music", "path: "+dir, 1) },
			want:   "overlaps library",
		},
		{
			name: "unknown default library",
			mutate: func(_, y string) string {
				return strings.Replace(y, "defaultLibraries: [bob]", "defaultLibraries: [carol]", 1)
			},
			want: `unknown library "carol"`,
		},
		{
			name: "unknown allowed library",
			mutate: func(_, y string) string {
				return strings.Replace(y, "allowedLibraries: [bob]", "allowedLibraries: [bob, dave]", 1)
			},
			want: `unknown library "dave"`,
		},
		{
			name: "default not in allowed",
			mutate: func(_, y string) string {
				return strings.Replace(y, "defaultLibraries: [shared]", "defaultLibraries: [bob]", 1)
			},
			want: "not in allowedLibraries",
		},
		{
			name:   "source does not exist",
			mutate: func(dir, y string) string { return strings.Replace(y, dir+"/source/bob", dir+"/source/missing", 1) },
			want:   "sources.bob-music",
		},
		{
			name:   "duplicated library name",
			mutate: func(_, y string) string { return strings.Replace(y, "name: alice", "name: shared", 1) },
			want:   "duplicated name",
		},
		{
			name:   "invalid auth mode",
			mutate: func(_, y string) string { return strings.Replace(y, "mode: none", "mode: magic", 1) },
			want:   "auth.mode",
		},
		{
			name:   "header mode without trusted proxies",
			mutate: func(_, y string) string { return strings.Replace(y, "mode: none", "mode: header", 1) },
			want:   "trustedProxies",
		},
		{
			name:   "unknown key",
			mutate: func(_, y string) string { return y + "unknownKey: 1\n" },
			want:   "unknownKey",
		},
		{
			name: "relative path",
			mutate: func(dir, y string) string {
				return strings.Replace(y, "libraryRoot: "+dir+"/library", "libraryRoot: library", 1)
			},
			want: "absolute",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, y := base(t)
			_, err := load(t, c.mutate(dir, y))
			if err == nil {
				t.Fatal("config was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestLibrarySymlinkRejected(t *testing.T) {
	dir, y := base(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "library", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := load(t, y); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("want symlink error, got %v", err)
	}
}

func TestDifferentFilesystem(t *testing.T) {
	dir, y := base(t)
	// library の1つだけが別のファイルシステムにある状態を、st_dev を返す関数の差し替えで作る。
	orig := statDevice
	t.Cleanup(func() { statDevice = orig })
	statDevice = func(p string) (uint64, error) {
		d, err := orig(p)
		if p == dir+"/library/bob" {
			d++
		}
		return d, err
	}
	if _, err := load(t, y); err == nil || !strings.Contains(err.Error(), "not on the same filesystem") {
		t.Fatalf("want filesystem error, got %v", err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"5GiB": 5 << 30, "500MB": 500e6, "1024": 1024, "1.5KiB": 1536, "0": 0, "2 tib": 2 << 40,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "five", "5XB", "-1GiB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"6h": 6 * time.Hour, "7d": 168 * time.Hour, "30s": 30 * time.Second} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
}

func TestLoadSecrets(t *testing.T) {
	dir, y := base(t)
	pw := filepath.Join(dir, "password")
	if err := os.WriteFile(pw, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	y = strings.Replace(y, "auth:\n  mode: none", "auth:\n  mode: basic\n  basic:\n    username: admin\n    passwordFile: "+pw, 1)
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAuthPassword, "")
	if err := c.LoadSecrets(); err != nil || c.Auth.Basic.Password != "s3cret" {
		t.Fatalf("password file: %q %v", c.Auth.Basic.Password, err)
	}
	t.Setenv(EnvAuthPassword, "fromenv")
	if err := c.LoadSecrets(); err != nil || c.Auth.Basic.Password != "fromenv" {
		t.Fatalf("env: %q %v", c.Auth.Basic.Password, err)
	}
}
