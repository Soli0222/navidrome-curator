// Package config は YAML の設定を読み、既定値を補い、起動時の検査をする。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
)

// 秘密情報を渡す環境変数。設定ファイルの値より優先する。
const (
	EnvConfigPath   = "NAVIDROME_CURATOR_CONFIG"
	EnvAuthPassword = "NAVIDROME_CURATOR_AUTH_PASSWORD"
)

// DefaultAudioExtensions は音声ファイルとみなす拡張子の既定値。
var DefaultAudioExtensions = []string{"mp3", "flac", "m4a", "aac", "ogg", "opus", "wav", "aif", "aiff", "wv", "ape", "dsf", "dff", "alac"}

// DefaultExclude は除外パターンの既定値。
var DefaultExclude = []string{"._*", ".DS_Store", ".AppleDouble", "Thumbs.db", "desktop.ini", "@eaDir", "#recycle", "#snapshot"}

// Config は設定ファイル全体。
type Config struct {
	Listen                string    `yaml:"listen"`
	Sources               []Source  `yaml:"sources"`
	LibraryRoot           string    `yaml:"libraryRoot"`
	Libraries             []Library `yaml:"libraries"`
	WorkDir               string    `yaml:"workDir"`
	AudioExtensions       []string  `yaml:"audioExtensions"`
	Exclude               []string  `yaml:"exclude"`
	ExcludeDotDirectories *bool     `yaml:"excludeDotDirectories"`
	Index                 Index     `yaml:"index"`
	Copy                  Copy      `yaml:"copy"`
	Trash                 Trash     `yaml:"trash"`
	Jobs                  Jobs      `yaml:"jobs"`
	Notify                Notify    `yaml:"notify"`
	Auth                  Auth      `yaml:"auth"`
}

// Source は読み取り元のディレクトリ。
type Source struct {
	Name             string   `yaml:"name"`
	Path             string   `yaml:"path"`
	DefaultLibraries []string `yaml:"defaultLibraries"`
	AllowedLibraries []string `yaml:"allowedLibraries"`
}

// Library はコピー先のディレクトリ。
type Library struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Index は索引の設定。
type Index struct {
	RefreshInterval Duration `yaml:"refreshInterval"`
}

// Copy はコピーの設定。
type Copy struct {
	Concurrency     int      `yaml:"concurrency"`
	FreeSpaceMargin Size     `yaml:"freeSpaceMargin"`
	FileMode        FileMode `yaml:"fileMode"`
	DirMode         FileMode `yaml:"dirMode"`
}

// Trash は trash の設定。
type Trash struct {
	Retention Duration `yaml:"retention"`
}

// Jobs はジョブの履歴の設定。
type Jobs struct {
	HistoryLimit int `yaml:"historyLimit"`
}

// Notify は完了の通知の設定。
type Notify struct {
	Webhook Webhook `yaml:"webhook"`
}

// Webhook は HTTP の通知先。
type Webhook struct {
	URL      string   `yaml:"url"`
	Debounce Duration `yaml:"debounce"`
	Timeout  Duration `yaml:"timeout"`
}

// Auth は認証の設定。
type Auth struct {
	Mode   string     `yaml:"mode"`
	Basic  BasicAuth  `yaml:"basic"`
	Header HeaderAuth `yaml:"header"`
}

// BasicAuth は Basic 認証の設定。パスワードは環境変数かファイルで渡す。
type BasicAuth struct {
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"passwordFile"`
	// Password は読み込んだパスワード。設定ファイルからは読まない。
	Password string `yaml:"-"`
}

// HeaderAuth は前段の認証プロキシのヘッダーを信じる設定。
type HeaderAuth struct {
	Name           string   `yaml:"name"`
	TrustedProxies []string `yaml:"trustedProxies"`
	// Trusted は TrustedProxies を解釈したもの。
	Trusted []netip.Prefix `yaml:"-"`
}

// 認証の方式。
const (
	AuthNone   = "none"
	AuthBasic  = "basic"
	AuthHeader = "header"
)

// Load は path の設定を読み、既定値を補い、内容を検査する。ファイルシステムの検査は Validate でする。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

// Parse は YAML を読み、既定値を補い、ファイルシステムに触れない検査をする。
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AudioExtensions == nil {
		c.AudioExtensions = append([]string(nil), DefaultAudioExtensions...)
	}
	if c.Exclude == nil {
		c.Exclude = append([]string(nil), DefaultExclude...)
	}
	if c.ExcludeDotDirectories == nil {
		t := true
		c.ExcludeDotDirectories = &t
	}
	if c.WorkDir == "" && c.LibraryRoot != "" {
		c.WorkDir = filepath.Join(c.LibraryRoot, ".curator")
	}
	if c.Index.RefreshInterval == 0 {
		c.Index.RefreshInterval = Duration(6 * time.Hour)
	}
	if c.Copy.Concurrency == 0 {
		c.Copy.Concurrency = 1
	}
	if c.Copy.FreeSpaceMargin == 0 {
		c.Copy.FreeSpaceMargin = 5 << 30
	}
	if c.Copy.FileMode == 0 {
		c.Copy.FileMode = 0o644
	}
	if c.Copy.DirMode == 0 {
		c.Copy.DirMode = 0o755
	}
	if c.Trash.Retention == 0 {
		c.Trash.Retention = Duration(7 * 24 * time.Hour)
	}
	if c.Jobs.HistoryLimit == 0 {
		c.Jobs.HistoryLimit = 500
	}
	if c.Notify.Webhook.Debounce == 0 {
		c.Notify.Webhook.Debounce = Duration(30 * time.Second)
	}
	if c.Notify.Webhook.Timeout == 0 {
		c.Notify.Webhook.Timeout = Duration(10 * time.Second)
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = AuthBasic
	}
	if c.Auth.Header.Name == "" {
		c.Auth.Header.Name = "X-Forwarded-User"
	}
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// check はファイルシステムに触れずにわかる誤りを調べる。
func (c *Config) check() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Sources) == 0 {
		add("sources: at least one source is required")
	}
	if len(c.Libraries) == 0 {
		add("libraries: at least one library is required")
	}
	if !filepath.IsAbs(c.LibraryRoot) {
		add("libraryRoot: must be an absolute path: %q", c.LibraryRoot)
	}
	if !filepath.IsAbs(c.WorkDir) {
		add("workDir: must be an absolute path: %q", c.WorkDir)
	}
	c.LibraryRoot = filepath.Clean(c.LibraryRoot)
	c.WorkDir = filepath.Clean(c.WorkDir)

	libs := map[string]bool{}
	for i := range c.Libraries {
		l := &c.Libraries[i]
		if !namePattern.MatchString(l.Name) {
			add("libraries[%d].name: invalid name %q (use letters, digits, '.', '_', '-')", i, l.Name)
		}
		if libs[l.Name] {
			add("libraries[%d].name: duplicated name %q", i, l.Name)
		}
		libs[l.Name] = true
		if !filepath.IsAbs(l.Path) {
			add("libraries[%d].path: must be an absolute path: %q", i, l.Path)
			continue
		}
		l.Path = filepath.Clean(l.Path)
	}
	srcs := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		if !namePattern.MatchString(s.Name) {
			add("sources[%d].name: invalid name %q (use letters, digits, '.', '_', '-')", i, s.Name)
		}
		if srcs[s.Name] {
			add("sources[%d].name: duplicated name %q", i, s.Name)
		}
		srcs[s.Name] = true
		if !filepath.IsAbs(s.Path) {
			add("sources[%d].path: must be an absolute path: %q", i, s.Path)
		} else {
			s.Path = filepath.Clean(s.Path)
		}
		for _, n := range s.DefaultLibraries {
			if !libs[n] {
				add("sources[%d].defaultLibraries: unknown library %q", i, n)
			}
		}
		for _, n := range s.AllowedLibraries {
			if !libs[n] {
				add("sources[%d].allowedLibraries: unknown library %q", i, n)
			}
		}
		if len(s.AllowedLibraries) > 0 {
			for _, n := range s.DefaultLibraries {
				if !contains(s.AllowedLibraries, n) {
					add("sources[%d].defaultLibraries: %q is not in allowedLibraries", i, n)
				}
			}
		}
	}

	// パスの重なり。字句で比べる。
	if filepath.IsAbs(c.LibraryRoot) && filepath.IsAbs(c.WorkDir) {
		if !pathutil.StrictlyWithin(c.LibraryRoot, c.WorkDir) {
			add("workDir: %q must be inside libraryRoot %q", c.WorkDir, c.LibraryRoot)
		}
		for _, l := range c.Libraries {
			if !filepath.IsAbs(l.Path) {
				continue
			}
			if !pathutil.StrictlyWithin(c.LibraryRoot, l.Path) {
				add("libraries.%s: path %q must be inside libraryRoot %q", l.Name, l.Path, c.LibraryRoot)
			}
			if pathutil.Within(l.Path, c.WorkDir) {
				add("workDir: %q must not be inside library %s (%q)", c.WorkDir, l.Name, l.Path)
			}
			if pathutil.Within(c.WorkDir, l.Path) {
				add("libraries.%s: path %q must not be inside workDir %q", l.Name, l.Path, c.WorkDir)
			}
		}
	}
	for i, a := range c.Libraries {
		for _, b := range c.Libraries[i+1:] {
			if filepath.IsAbs(a.Path) && filepath.IsAbs(b.Path) && pathutil.Overlaps(a.Path, b.Path) {
				add("libraries: %s (%q) and %s (%q) overlap", a.Name, a.Path, b.Name, b.Path)
			}
		}
	}
	for _, s := range c.Sources {
		if !filepath.IsAbs(s.Path) {
			continue
		}
		for _, l := range c.Libraries {
			if filepath.IsAbs(l.Path) && pathutil.Overlaps(s.Path, l.Path) {
				add("sources.%s: path %q overlaps library %s (%q)", s.Name, s.Path, l.Name, l.Path)
			}
		}
		if filepath.IsAbs(c.WorkDir) && pathutil.Overlaps(s.Path, c.WorkDir) {
			add("sources.%s: path %q overlaps workDir %q", s.Name, s.Path, c.WorkDir)
		}
	}

	if c.Copy.Concurrency < 1 {
		add("copy.concurrency: must be at least 1")
	}
	if c.Copy.FreeSpaceMargin < 0 {
		add("copy.freeSpaceMargin: must not be negative")
	}
	if c.Jobs.HistoryLimit < 1 {
		add("jobs.historyLimit: must be at least 1")
	}
	if c.Index.RefreshInterval < 0 || c.Trash.Retention < 0 || c.Notify.Webhook.Debounce < 0 {
		add("durations must not be negative")
	}
	for _, p := range c.Exclude {
		if _, err := filepath.Match(strings.ToLower(p), ""); err != nil {
			add("exclude: invalid pattern %q: %v", p, err)
		}
	}

	switch c.Auth.Mode {
	case AuthNone:
	case AuthBasic:
		if c.Auth.Basic.Username == "" {
			add("auth.basic.username: required when auth.mode is basic")
		}
	case AuthHeader:
		if len(c.Auth.Header.TrustedProxies) == 0 {
			add("auth.header.trustedProxies: required when auth.mode is header")
		}
		c.Auth.Header.Trusted = nil
		for _, p := range c.Auth.Header.TrustedProxies {
			pfx, err := parsePrefix(p)
			if err != nil {
				add("auth.header.trustedProxies: %v", err)
				continue
			}
			c.Auth.Header.Trusted = append(c.Auth.Header.Trusted, pfx)
		}
	default:
		add("auth.mode: must be none, basic or header: %q", c.Auth.Mode)
	}

	return errors.Join(errs...)
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// LoadSecrets は秘密情報を環境変数かファイルから読む。
func (c *Config) LoadSecrets() error {
	if c.Auth.Mode != AuthBasic {
		return nil
	}
	if v := os.Getenv(EnvAuthPassword); v != "" {
		c.Auth.Basic.Password = v
		return nil
	}
	if c.Auth.Basic.PasswordFile == "" {
		return fmt.Errorf("auth.basic: set %s or auth.basic.passwordFile", EnvAuthPassword)
	}
	b, err := os.ReadFile(c.Auth.Basic.PasswordFile)
	if err != nil {
		return fmt.Errorf("auth.basic.passwordFile: %w", err)
	}
	c.Auth.Basic.Password = strings.TrimRight(string(b), "\r\n")
	if c.Auth.Basic.Password == "" {
		return fmt.Errorf("auth.basic.passwordFile: %s is empty", c.Auth.Basic.PasswordFile)
	}
	return nil
}

// Prepare はファイルシステムを確かめる。存在しない library と作業領域は作り、source は作らない。
func (c *Config) Prepare() error {
	var errs []error
	for _, s := range c.Sources {
		fi, err := os.Stat(s.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("sources.%s: %w", s.Name, err))
			continue
		}
		if !fi.IsDir() {
			errs = append(errs, fmt.Errorf("sources.%s: %s is not a directory", s.Name, s.Path))
		}
	}
	rootDev, err := statDevice(c.LibraryRoot)
	if err != nil {
		errs = append(errs, fmt.Errorf("libraryRoot: %w", err))
		return errors.Join(errs...)
	}
	dirMode := fs.FileMode(c.Copy.DirMode)
	check := func(label, p string) {
		if err := mkdirUnder(c.LibraryRoot, p, dirMode); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
			return
		}
		dev, err := statDevice(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
			return
		}
		if dev != rootDev {
			errs = append(errs, fmt.Errorf("%s: %s is not on the same filesystem as libraryRoot %s", label, p, c.LibraryRoot))
		}
	}
	for _, l := range c.Libraries {
		check("libraries."+l.Name, l.Path)
	}
	check("workDir", c.WorkDir)
	return errors.Join(errs...)
}

// mkdirUnder は root の下に p を作る。途中にシンボリックリンクがあれば失敗する。
func mkdirUnder(root, p string, mode fs.FileMode) error {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return err
	}
	return pathutil.MkdirAllNoFollow(root, filepath.ToSlash(rel), mode)
}

// statDevice はパスのデバイス番号（st_dev）を返す。テストで差し替える。
var statDevice = deviceOf

func deviceOf(p string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev の型は OS ごとに違う。
}

// SourceByName は名前で source を探す。
func (c *Config) SourceByName(name string) (*Source, bool) {
	for i := range c.Sources {
		if c.Sources[i].Name == name {
			return &c.Sources[i], true
		}
	}
	return nil, false
}

// LibraryByName は名前で library を探す。
func (c *Config) LibraryByName(name string) (*Library, bool) {
	for i := range c.Libraries {
		if c.Libraries[i].Name == name {
			return &c.Libraries[i], true
		}
	}
	return nil, false
}

// Allowed は、source のアルバムを library に入れてよいかを返す。
func (s *Source) Allowed(library string) bool {
	return len(s.AllowedLibraries) == 0 || contains(s.AllowedLibraries, library)
}

// AllowedList は、source が追加先にできる library の名前を返す。
func (c *Config) AllowedList(s *Source) []string {
	if len(s.AllowedLibraries) > 0 {
		return append([]string(nil), s.AllowedLibraries...)
	}
	out := make([]string, 0, len(c.Libraries))
	for _, l := range c.Libraries {
		out = append(out, l.Name)
	}
	return out
}

// StagingDir は作業領域の staging を返す。
func (c *Config) StagingDir() string { return filepath.Join(c.WorkDir, "staging") }

// TrashDir は作業領域の trash を返す。
func (c *Config) TrashDir() string { return filepath.Join(c.WorkDir, "trash") }

// StatePath はジョブの履歴を置く SQLite のパスを返す。
func (c *Config) StatePath() string { return filepath.Join(c.WorkDir, "state.db") }

// Duration は "6h" のような文字列で書く時間。
type Duration time.Duration

// UnmarshalYAML は time.ParseDuration の書式に加え、"7d" のような日数を受け付ける。
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ParseDuration は time.ParseDuration に日数（"7d"）を足したもの。
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}

// D は time.Duration を返す。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Size は "5GiB" のような文字列で書くバイト数。
type Size int64

var sizePattern = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)\s*$`)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "kib": 1 << 10,
	"m": 1e6, "mb": 1e6, "mib": 1 << 20,
	"g": 1e9, "gb": 1e9, "gib": 1 << 30,
	"t": 1e12, "tb": 1e12, "tib": 1 << 40,
}

// ParseSize は "5GiB"、"500MB"、"1024" のような文字列を読む。
func ParseSize(s string) (int64, error) {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	unit, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * unit), nil
}

// UnmarshalYAML は文字列か整数を受け付ける。
func (z *Size) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseSize(s)
	if err != nil {
		return err
	}
	*z = Size(v)
	return nil
}

// FileMode は "0644" のような8進数の文字列で書くパーミッション。
type FileMode uint32

// UnmarshalYAML は8進数の文字列を読む。
func (m *FileMode) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil || v > 0o777 {
		return fmt.Errorf("invalid file mode %q", s)
	}
	*m = FileMode(v)
	return nil
}

// Mode は fs.FileMode を返す。
func (m FileMode) Mode() fs.FileMode { return fs.FileMode(m) }
