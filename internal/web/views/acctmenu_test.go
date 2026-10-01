package views

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// RECON-00: which menu item is marked as the current page.
func TestIsCurrent(t *testing.T) {
	cases := []struct {
		path, href string
		want       bool
	}{
		{"/profile", "/profile", true},
		{"/admin/users/7", "/admin/users", true},
		{"/admin/usersx", "/admin/users", false}, // a prefix is not a parent
		{"/admin", "/admin/users", false},
		{"/", "/profile", false},
		{"", "/profile", false},
	}
	for _, c := range cases {
		if got := isCurrent(c.path, c.href); got != c.want {
			t.Errorf("isCurrent(%q, %q) = %v, want %v", c.path, c.href, got, c.want)
		}
	}
}

// Every avatar size a template asks for has a size class in the stylesheet.
// Without one the initials fallback has no box and shrinks to its letters
// (the CSP refuses inline styles, so a class is the only way to size it).
func TestAvatarSizesHaveStyles(t *testing.T) {
	css, err := os.ReadFile("../static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range avatarSizes {
		rule := fmt.Sprintf(".avatar-%d { width: %dpx; height: %dpx;", n, n, n)
		if !strings.Contains(string(css), rule) {
			t.Errorf("stylesheet lacks %q", rule)
		}
	}

	files, _ := filepath.Glob("*.templ")
	call := regexp.MustCompile(`@(?:User)?Avatar\([^)]*?, (\d+)\)`)
	seen := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			seen++
			n, _ := strconv.Atoi(m[1])
			if got := avatarSize(n); got != "avatar-"+m[1] {
				t.Errorf("%s: avatar size %d has no class of its own (maps to %s)", f, n, got)
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no avatar call sites; the pattern is stale")
	}
}

func TestAvatarSizeNearest(t *testing.T) {
	for in, want := range map[int]string{0: "avatar-22", 22: "avatar-22", 27: "avatar-26", 40: "avatar-28", 60: "avatar-72", 500: "avatar-72"} {
		if got := avatarSize(in); got != want {
			t.Errorf("avatarSize(%d) = %s, want %s", in, got, want)
		}
	}
}
