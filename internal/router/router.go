// Package router decides what happens to a query: it evaluates the rules in
// order and applies the first rule whose conditions all hold (CoreDNS-style
// first match). The final rule must be a catch-all, so every query has exactly
// one decision and that decision is explainable (see Decision).
package router

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/wire"
)

// Forwarder sends a query upstream. *upstream.Group implements it.
type Forwarder interface {
	// Name returns the group name, which is used as the decision target in
	// logs and metrics.
	Name() string
	// Exchange returns the response to req, with the caller's query ID
	// restored, or an error when no server could complete the exchange.
	Exchange(ctx context.Context, req []byte, q dnsmessage.Question, proto wire.Proto) ([]byte, error)
}

// Action is the operation a matching rule performs.
type Action uint8

const (
	// Forward sends the query to an upstream group.
	Forward Action = iota + 1
	// Answer answers locally from fixed addresses.
	Answer
	// Reject answers locally with an RCODE, or with NODATA when RCode is
	// RCodeSuccess.
	Reject
)

// String returns the lower-case name of the action, or "none" for an unset
// action.
func (a Action) String() string {
	switch a {
	case Forward:
		return "forward"
	case Answer:
		return "answer"
	case Reject:
		return "reject"
	}
	return "none"
}

// Set is a named domain matcher used as a rule condition.
type Set struct {
	Name    string          // configuration name of the set
	Matcher *domain.Matcher // matcher applied to normalized names
}

// LocalAnswer holds fixed records returned by an answer rule. Query types
// without records get a NODATA response.
type LocalAnswer struct {
	A    []netip.Addr // IPv4 records
	AAAA []netip.Addr // IPv6 records
	TTL  uint32       // record TTL in seconds
}

// Rule is one routing rule. A rule with no Sets matches every name; a rule
// with no QTypes matches every type.
type Rule struct {
	Name      string            // rule name used in logs and in duplicate-name checks
	Sets      []*Set            // at least one must match; empty matches every name
	QTypes    []dnsmessage.Type // one of these must match; empty matches every type
	Action    Action            // operation performed when the rule matches
	Forwarder Forwarder         // required when Action is Forward
	Answer    LocalAnswer       // records returned when Action is Answer
	RCode     dnsmessage.RCode  // Reject: RCodeSuccess means NODATA
}

// CatchAll reports whether the rule matches every query.
func (r *Rule) CatchAll() bool { return len(r.Sets) == 0 && len(r.QTypes) == 0 }

// Target names where the rule sends queries, for logs and debugging.
func (r *Rule) Target() string {
	switch r.Action {
	case Forward:
		return r.Forwarder.Name()
	case Answer:
		return "local"
	case Reject:
		if r.RCode == dnsmessage.RCodeSuccess {
			return "NODATA"
		}
		return wire.RCodeName(int(r.RCode))
	}
	return ""
}

// Decision explains a routing result.
type Decision struct {
	Rule *Rule       // the rule that produced this decision
	Set  string      // the set that matched, "" for a catch-all/qtype-only rule
	Kind domain.Kind // how the name matched within that set
}

// Router is immutable and safe for concurrent use.
type Router struct{ rules []*Rule }

// New validates the rule list and returns an immutable router. Every rule must
// have exactly one action; a forward rule must name a Forwarder; no rule before
// the last may be a catch-all; and the last rule must be a catch-all.
func New(rules []*Rule) (*Router, error) {
	if len(rules) == 0 {
		return nil, errors.New("no rules")
	}
	for i, r := range rules {
		switch r.Action {
		case Forward:
			if r.Forwarder == nil {
				return nil, fmt.Errorf("rule %q: forward without upstream", r.Name)
			}
		case Answer, Reject:
		default:
			return nil, fmt.Errorf("rule %q: no action", r.Name)
		}
		if r.CatchAll() && i != len(rules)-1 {
			return nil, fmt.Errorf("rule %q matches everything, so the rules after it are unreachable", r.Name)
		}
	}
	if last := rules[len(rules)-1]; !last.CatchAll() {
		return nil, fmt.Errorf("last rule %q must match everything (no sets, no qtypes)", last.Name)
	}
	return &Router{rules: rules}, nil
}

// Rules returns the rules in evaluation order.
func (r *Router) Rules() []*Rule { return r.rules }

// Route returns the decision for a query. name must be normalized (lower case,
// without a trailing dot), and qtype is the type from the parsed question.
func (r *Router) Route(name string, qtype dnsmessage.Type) Decision {
	for _, rule := range r.rules {
		if len(rule.QTypes) > 0 && !hasType(rule.QTypes, qtype) {
			continue
		}
		if len(rule.Sets) == 0 {
			return Decision{Rule: rule}
		}
		for _, s := range rule.Sets {
			if k, ok := s.Matcher.Match(name); ok {
				return Decision{Rule: rule, Set: s.Name, Kind: k}
			}
		}
	}
	return Decision{Rule: r.rules[len(r.rules)-1]} // unreachable: New enforces a catch-all
}

func hasType(ts []dnsmessage.Type, t dnsmessage.Type) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}
