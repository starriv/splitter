package app

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/querylog"
)

// AnswerRecord is one answer record rendered for display. It is the same type
// the query log stores, so the resolve endpoint and the recent-queries table
// describe an answer identically.
type AnswerRecord = querylog.Answer

const (
	// maxRecentAnswers bounds the answer records kept per query. A query log
	// entry must stay small, and the control page never shows more than a few
	// values; an upstream answering with hundreds of records (or a large TXT
	// set) would otherwise inflate every stored line.
	maxRecentAnswers = 8
	// maxAnswerDataBytes bounds one rendered answer. A TXT record can carry
	// multiple 255-byte strings; longer data is shortened and flagged.
	maxAnswerDataBytes = 256
)

// decodeAnswers renders the answer section for display. Record types that are
// not decoded explicitly are listed with their size and are never dropped.
func decodeAnswers(resp []byte) []AnswerRecord {
	out := []AnswerRecord{}
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		return out
	}
	if err := p.SkipAllQuestions(); err != nil {
		return out
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			return out
		}
		rec, ok := decodeAnswer(&p, h)
		if !ok {
			return out
		}
		out = append(out, rec)
	}
}

// recentAnswers decodes the answer section for the recent ring and the query
// log. It stops at maxRecentAnswers records and shortens long data, reporting
// whether anything was left out. A malformed or cut-short section keeps the
// records decoded so far and is flagged; the RCODE is recorded by the caller
// either way.
func recentAnswers(resp []byte) (answers []querylog.Answer, truncated bool) {
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		return nil, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, false
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			// Anything but the normal end of the section means the reply was
			// malformed or cut short; keep the records decoded so far.
			return answers, truncated || !errors.Is(err, dnsmessage.ErrSectionDone)
		}
		if answers == nil {
			// Allocate only once there is something to keep: most refusals and
			// NODATA replies never reach this branch.
			answers = make([]querylog.Answer, 0, 4)
		}
		if len(answers) == maxRecentAnswers {
			return answers, true
		}
		rec, ok := decodeAnswer(&p, h)
		if !ok {
			return answers, true
		}
		if d := rec.Data; len(d) > maxAnswerDataBytes {
			rec.Data, truncated = shorten(d), true
		}
		answers = append(answers, rec)
	}
}

// decodeAnswer reads one answer resource. ok is false when the record cannot
// be decoded; the caller keeps the answers decoded so far.
func decodeAnswer(p *dnsmessage.Parser, h dnsmessage.ResourceHeader) (rec AnswerRecord, ok bool) {
	rec = AnswerRecord{Name: domain.Normalize(h.Name.String()), Type: config.QTypeName(h.Type), TTL: h.TTL}
	switch h.Type {
	case dnsmessage.TypeA:
		r, err := p.AResource()
		if err != nil {
			return rec, false
		}
		rec.Data = netip.AddrFrom4(r.A).String()
	case dnsmessage.TypeAAAA:
		r, err := p.AAAAResource()
		if err != nil {
			return rec, false
		}
		rec.Data = netip.AddrFrom16(r.AAAA).String()
	case dnsmessage.TypeCNAME:
		r, err := p.CNAMEResource()
		if err != nil {
			return rec, false
		}
		rec.Data = domain.Normalize(r.CNAME.String())
	case dnsmessage.TypeTXT:
		r, err := p.TXTResource()
		if err != nil {
			return rec, false
		}
		rec.Data = strings.Join(r.TXT, " ")
	default:
		if err := p.SkipAnswer(); err != nil {
			return rec, false
		}
		rec.Data = fmt.Sprintf("%d bytes", h.Length)
	}
	return rec, true
}

// shorten cuts s to maxAnswerDataBytes without splitting a UTF-8 sequence.
// TXT data is arbitrary bytes, so invalid text is kept as is; JSON encoding
// replaces invalid sequences.
func shorten(s string) string {
	cut := maxAnswerDataBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
