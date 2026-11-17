package config

import (
	"errors"
	"strings"
	"testing"
)

const head = `format_version = 1
[medium]
id = "6f1c2e9a-8b4d-4f3a-9c51-2d7e0b8a4f11"
name = "Stick"
`

func parseValidate(t *testing.T, s string) (*Config, *Result, error) {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		return nil, nil, err
	}
	return c, c.Validate("/home/u"), nil
}

func TestDefaults(t *testing.T) {
	c, r, err := parseValidate(t, head+`
[[mapping]]
name = "a"
source = "~/x"
target = "x"
`)
	if err != nil || !r.OK() {
		t.Fatalf("err=%v res=%v", err, r)
	}
	if c.Safety.MaxDeletePercent != 20 || c.Safety.AllowEmptySource {
		t.Errorf("defaults wrong: %+v", c.Safety)
	}
	m := c.Mappings[0]
	if m.Filter != FilterRsync || !m.UsesDefaultExcludes() || !m.GitDirs() {
		t.Errorf("mapping defaults wrong: %+v", m)
	}
}

func TestTemplateIsValid(t *testing.T) {
	id, err := NewUUID()
	if err != nil || !ValidUUID(id) {
		t.Fatalf("uuid %q %v", id, err)
	}
	_, r, err := parseValidate(t, Template(id, `Name "mit" Zeichen`))
	if err != nil || !r.OK() {
		t.Fatalf("template invalid: %v %v", err, r)
	}
	if _, err := Parse([]byte(head + "[retention]\nkeep_last = 5\n")); err == nil {
		t.Error("removed [retention] must be reported as unknown")
	}
}

func TestUnknownField(t *testing.T) {
	_, err := Parse([]byte(head + "[[mapping]]\nname=\"a\"\nsource=\"/a\"\ntarget=\"a\"\nexlcude=[\"x\"]\n"))
	if err == nil || !strings.Contains(err.Error(), "exlcude") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
	_, err = Parse([]byte(head + "[safty]\nx=1\n"))
	if err == nil {
		t.Fatal("expected error for unknown table")
	}
}

func TestFormatVersion(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(head, "format_version = 1", "format_version = 2", 1)))
	var tn *TooNewError
	if !errors.As(err, &tn) {
		t.Fatalf("expected TooNewError, got %v", err)
	}
	_, err = Parse([]byte(`[medium]` + "\nid=\"x\"\nname=\"y\"\n"))
	if err == nil {
		t.Fatal("missing format_version must fail")
	}
}

func TestTargets(t *testing.T) {
	ok := []string{"a", "a/b/", "./a", "a/./b", "Dokumente/Privat"}
	bad := []string{"", ".", "./", "/abs", "..", "../a", "a/../../b", "a/../b/.."}
	for _, s := range ok {
		if err := ValidateTarget(s); err != nil {
			t.Errorf("%q should be valid: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateTarget(s); err == nil {
			t.Errorf("%q should be invalid", s)
		}
	}
	// "a/../b" cleans to "b" and is fine.
	if err := ValidateTarget("a/../b"); err != nil {
		t.Errorf("a/../b: %v", err)
	}
}

func mapping(name, src, tgt string) string {
	return "[[mapping]]\nname=\"" + name + "\"\nsource=\"" + src + "\"\ntarget=\"" + tgt + "\"\n"
}

func TestOverlaps(t *testing.T) {
	cases := []struct {
		body       string
		errs, warn int
	}{
		{mapping("a", "/a", "x") + mapping("b", "/b", "y"), 0, 0},
		{mapping("a", "/a", "x") + mapping("b", "/b", "x/"), 1, 0},
		{mapping("a", "/a", "x") + mapping("b", "/b", "x/y"), 1, 0},
		{mapping("a", "/a", "xy") + mapping("b", "/b", "x"), 0, 0},
		{mapping("a", "/a", "x") + mapping("A", "/b", "y"), 1, 0},
		{mapping("a", "~/a", "x") + mapping("b", "/home/u/a/sub", "y"), 0, 1},
		{mapping("a", "/a", "x") + mapping("b", "/ab", "y"), 0, 0},
	}
	for i, c := range cases {
		_, r, err := parseValidate(t, head+c.body)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Errors) != c.errs || len(r.Warnings) != c.warn {
			t.Errorf("case %d: errors=%v warnings=%v", i, r.Errors, r.Warnings)
		}
	}
}

func TestExpandSource(t *testing.T) {
	cases := map[string]string{
		"~":       "/home/u",
		"~/":      "/home/u",
		"~/a/b/":  "/home/u/a/b",
		"/abs/p/": "/abs/p",
		"/a/../b": "/b",
	}
	for in, want := range cases {
		got, err := ExpandSource(in, "/home/u")
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"rel/p", "~other/x", ""} {
		if _, err := ExpandSource(in, "/home/u"); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestFilterRules(t *testing.T) {
	cases := []struct {
		body string
		errs int
	}{
		{"[defaults]\nexclude=[\"!x\"]\n" + mapping("a", "/a", "a"), 1},
		{"[defaults]\nexclude=[\"!x\"]\n" + mapping("a", "/a", "a") + "use_default_excludes=false\n", 0},
		{"[defaults]\nexclude=[\"!x\"]\n" + mapping("a", "/a", "a") + "filter=\"git\"\n", 0},
		{mapping("a", "/a", "a") + "exclude=[\"!x\"]\n", 1},
		{mapping("a", "/a", "a") + "filter=\"git\"\nexclude=[\"!x\"]\ninclude=[\"**/.env\"]\nbackup_git_dirs=false\n", 0},
		{mapping("a", "/a", "a") + "include=[\"instance.cfg\", \"/mc/saves/Chill/\"]\n", 0},
		{mapping("a", "/a", "a") + "include=[\"!x\"]\n", 1},
		{mapping("a", "/a", "a") + "include=[\"a/../../etc\"]\n", 1},
		{mapping("a", "/a", "a") + "include=[\"\"]\n", 1},
		{mapping("a", "/a", "a") + "backup_git_dirs=true\n", 1},
		{mapping("a", "/a", "a") + "filter=\"svn\"\n", 1},
		{mapping("a", "/a", "a") + "exclude=[\"\"]\n", 1},
	}
	for i, c := range cases {
		_, r, err := parseValidate(t, head+c.body)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Errors) != c.errs {
			t.Errorf("case %d: errors=%v", i, r.Errors)
		}
	}
}

func TestExcludes(t *testing.T) {
	c, _, err := parseValidate(t, head+"[defaults]\nexclude=[\"d\"]\n"+mapping("a", "/a", "a")+"exclude=[\"m\"]\n"+mapping("b", "/b", "b")+"use_default_excludes=false\nexclude=[\"m\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Excludes(&c.Mappings[0]), ","); got != "d,m" {
		t.Errorf("got %q", got)
	}
	if got := strings.Join(c.Excludes(&c.Mappings[1]), ","); got != "m" {
		t.Errorf("got %q", got)
	}
}

func TestInvalidMedium(t *testing.T) {
	_, r, _ := parseValidate(t, "format_version=1\n[medium]\nid=\"nope\"\nname=\"\"\n[safety]\nmax_delete_percent=101\n")
	if len(r.Errors) != 3 {
		t.Errorf("errors: %v", r.Errors)
	}
}
