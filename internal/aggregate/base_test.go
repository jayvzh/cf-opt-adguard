package aggregate

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Assets.Example.COM.", "assets.example.com"},
		{"  EXAMPLE.com  ", "example.com"},
		{"a..b.com", "a.b.com"},
		{"", ""},
		{".", ""},
	}
	for _, tc := range cases {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDomainFilter(t *testing.T) {
	// include 非空：命中才留
	f := NewDomainFilter([]string{"*.example.com"}, []string{"*.corp"})
	if !f.Allow("a.EXAMPLE.com") || !f.Allow("example.com") {
		t.Error("include 后缀应命中（含裸域）")
	}
	if f.Allow("other.org") {
		t.Error("include 未命中应拒绝")
	}

	// exclude：命中即丢
	f = NewDomainFilter(nil, []string{"*.corp", "secret.example.com"})
	if f.Allow("x.corp") || f.Allow("SECRET.example.com") {
		t.Error("exclude 命中应拒绝")
	}
	if !f.Allow("ok.example.com") {
		t.Error("exclude 未命中应保留")
	}

	// 双空：全放行
	f = NewDomainFilter(nil, nil)
	if !f.Allow("anything.net") {
		t.Error("空名单应全放行")
	}
}
