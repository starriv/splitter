package router

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/wire"
)

// fwd is a Forwarder that identifies itself by name and returns no response.
type fwd string

func (f fwd) Name() string { return string(f) }
func (fwd) Exchange(context.Context, []byte, dnsmessage.Question, wire.Proto) ([]byte, error) {
	return nil, nil
}

// set builds a named Set from domain rule strings.
func set(t *testing.T, name string, rules ...string) *Set {
	b := domain.NewBuilder()
	for _, r := range rules {
		if err := b.AddString(r); err != nil {
			t.Fatal(err)
		}
	}
	return &Set{Name: name, Matcher: b.Build()}
}

// TestFirstMatchWins verifies that the first rule whose set and query type
// conditions hold decides the query, including query-type-only rules and the
// catch-all rule.
func TestFirstMatchWins(t *testing.T) {
	block := set(t, "block", "domain:ads.baidu.com")
	cn := set(t, "cn", "domain:baidu.com", "domain:qq.com")
	extra := set(t, "extra", "domain:example.org")
	r, err := New([]*Rule{
		{Name: "block", Sets: []*Set{block}, Action: Reject, RCode: dnsmessage.RCodeNameError},
		{Name: "no-https-cn", Sets: []*Set{cn}, QTypes: []dnsmessage.Type{65}, Action: Reject},
		{Name: "aaaa-off", QTypes: []dnsmessage.Type{dnsmessage.TypeAAAA}, Action: Reject},
		{Name: "cn", Sets: []*Set{extra, cn}, Action: Forward, Forwarder: fwd("aliyun")},
		{Name: "default", Action: Forward, Forwarder: fwd("cloudflare")},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		qt      dnsmessage.Type
		rule    string
		set     string
		target  string
		matched domain.Kind
	}{
		{"x.ads.baidu.com", dnsmessage.TypeA, "block", "block", "NXDOMAIN", domain.KindDomain},
		{"www.baidu.com", 65, "no-https-cn", "cn", "NODATA", domain.KindDomain},
		{"www.baidu.com", dnsmessage.TypeAAAA, "aaaa-off", "", "NODATA", 0},
		{"www.baidu.com", dnsmessage.TypeA, "cn", "cn", "aliyun", domain.KindDomain},
		{"a.example.org", dnsmessage.TypeA, "cn", "extra", "aliyun", domain.KindDomain},
		{"www.google.com", 65, "default", "", "cloudflare", 0},
	}
	for _, c := range cases {
		d := r.Route(c.name, c.qt)
		if d.Rule.Name != c.rule || d.Set != c.set || d.Rule.Target() != c.target || d.Kind != c.matched {
			t.Errorf("%s/%d -> rule %s set %q target %s kind %s", c.name, c.qt, d.Rule.Name, d.Set, d.Rule.Target(), d.Kind)
		}
	}
}

// TestNewRejectsBadRuleLists verifies that New rejects rule lists that leave
// queries undecided, contain unreachable rules, or lack an action.
func TestNewRejectsBadRuleLists(t *testing.T) {
	s := set(t, "s", "a.com")
	cases := []struct {
		rules []*Rule
		want  string
	}{
		{nil, "no rules"},
		{[]*Rule{{Name: "x", Sets: []*Set{s}, Action: Forward, Forwarder: fwd("u")}}, "must match everything"},
		{[]*Rule{{Name: "all", Action: Reject}, {Name: "after", Action: Reject}}, "unreachable"},
		{[]*Rule{{Name: "f", Action: Forward}}, "without upstream"},
		{[]*Rule{{Name: "n"}}, "no action"},
	}
	for _, c := range cases {
		if _, err := New(c.rules); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
}
