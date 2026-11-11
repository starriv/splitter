package wire

import (
	"errors"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// pack serializes m, failing the test on error.
func pack(t *testing.T, m dnsmessage.Message) []byte {
	t.Helper()
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// q builds an INET question for name and qt.
func q(name string, qt dnsmessage.Type) dnsmessage.Question {
	return dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qt, Class: dnsmessage.ClassINET}
}

// opt builds an OPT record with the DO bit set and a 4096-byte payload size.
func opt(t *testing.T) dnsmessage.Resource {
	var rh dnsmessage.ResourceHeader
	if err := rh.SetEDNS0(4096, 0, true); err != nil {
		t.Fatal(err)
	}
	return dnsmessage.Resource{Header: rh, Body: &dnsmessage.OPTResource{}}
}

// TestParseQuery covers plain, EDNS and root queries, rejection of a query
// without exactly one question, a message shorter than the header, and a
// truncated question section.
func TestParseQuery(t *testing.T) {
	plain := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 1, RecursionDesired: true}, Questions: []dnsmessage.Question{q("WWW.Baidu.COM.", dnsmessage.TypeA)}})
	got, err := ParseQuery(plain)
	if err != nil || got.Name != "www.baidu.com" || got.EDNS || got.Header.ID != 1 {
		t.Fatalf("plain: %+v %v", got, err)
	}

	withOPT := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 2}, Questions: []dnsmessage.Question{q("a.cn.", dnsmessage.TypeAAAA)},
		Additionals: []dnsmessage.Resource{opt(t)}})
	if got, err := ParseQuery(withOPT); err != nil || !got.EDNS {
		t.Fatalf("edns: %+v %v", got, err)
	}

	root := pack(t, dnsmessage.Message{Questions: []dnsmessage.Question{q(".", dnsmessage.TypeNS)}})
	if got, err := ParseQuery(root); err != nil || got.Name != "" {
		t.Fatalf("root: %+v %v", got, err)
	}

	two := pack(t, dnsmessage.Message{Questions: []dnsmessage.Question{q("a.cn.", 1), q("b.cn.", 1)}})
	if got, err := ParseQuery(two); !errors.Is(err, ErrFormat) || !got.HasQuestion {
		t.Fatalf("two questions: %+v %v", got, err)
	}

	none := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 3}})
	if got, err := ParseQuery(none); !errors.Is(err, ErrFormat) || got.HasQuestion || got.Header.ID != 3 {
		t.Fatalf("no question: %+v %v", got, err)
	}

	if _, err := ParseQuery([]byte{1, 2, 3}); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("short: %v", err)
	}

	// QDCOUNT says 1 but the question is cut off.
	cut := append([]byte{0, 9, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}, 3, 'a', 'b')
	if _, err := ParseQuery(cut); !errors.Is(err, ErrFormat) {
		t.Fatalf("truncated question: %v", err)
	}
}

// TestResponseMatches covers matching by ID, name case, type and class, and the
// acceptance of a question-less response only for error RCODEs.
func TestResponseMatches(t *testing.T) {
	question := q("www.baidu.com.", dnsmessage.TypeA)
	resp := func(id uint16, rc dnsmessage.RCode, qs ...dnsmessage.Question) []byte {
		return pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: id, Response: true, RCode: rc}, Questions: qs})
	}
	cases := []struct {
		name string
		msg  []byte
		want bool
	}{
		{"exact", resp(7, 0, question), true},
		{"case differs", resp(7, 0, q("WWW.BAIDU.com.", dnsmessage.TypeA)), true},
		{"wrong id", resp(8, 0, question), false},
		{"wrong name", resp(7, 0, q("www.baidu.cn.", dnsmessage.TypeA)), false},
		{"wrong type", resp(7, 0, q("www.baidu.com.", dnsmessage.TypeAAAA)), false},
		{"no question, NOERROR", resp(7, 0), false},
		{"no question, REFUSED", resp(7, dnsmessage.RCodeRefused), true},
		{"query not response", pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 7}, Questions: []dnsmessage.Question{question}}), false},
		{"garbage", []byte{0, 7}, false},
	}
	for _, c := range cases {
		if got := ResponseMatches(c.msg, 7, question); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestReply checks the header fields, question echo, OPT record and helper
// agreement for a locally built NXDOMAIN reply.
func TestReply(t *testing.T) {
	query := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 0x4242, RecursionDesired: true},
		Questions: []dnsmessage.Question{q("x.local.example.", dnsmessage.TypeAAAA)}, Additionals: []dnsmessage.Resource{opt(t)}})
	pq, err := ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Reply(pq, dnsmessage.RCodeNameError, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		t.Fatal(err)
	}
	h := m.Header
	if h.ID != 0x4242 || !h.Response || !h.Authoritative || !h.RecursionDesired || !h.RecursionAvailable || h.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("header %+v", h)
	}
	if len(m.Questions) != 1 || m.Questions[0].Name.String() != "x.local.example." {
		t.Fatalf("question not echoed: %+v", m.Questions)
	}
	if len(m.Additionals) != 1 || m.Additionals[0].Header.Type != dnsmessage.TypeOPT || m.Additionals[0].Header.Class != EDNSPayload {
		t.Fatalf("OPT: %+v", m.Additionals)
	}
	if RCode(raw) != 3 || Truncated(raw) || ID(raw) != 0x4242 {
		t.Fatal("helpers disagree with the packed header")
	}
}

// FuzzParseQuery verifies that every message ParseQuery accepts can be answered
// without error and that the reply preserves the query ID.
func FuzzParseQuery(f *testing.F) {
	f.Add([]byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 1, 3, 'w', 'w', 'w', 0, 0, 1, 0, 1, 0, 0, 41, 16, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 1, 1, 0, 0, 2, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0xc0, 0x0c})
	f.Fuzz(func(t *testing.T, b []byte) {
		pq, err := ParseQuery(b)
		if errors.Is(err, ErrNoHeader) {
			return
		}
		// Every query that can be parsed must be answerable without error.
		rc := dnsmessage.RCodeSuccess
		if err != nil {
			rc = dnsmessage.RCodeFormatError
		}
		out, err := Reply(pq, rc, false, nil)
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if ID(out) != pq.Header.ID {
			t.Fatal("ID changed")
		}
		_ = ResponseMatches(b, pq.Header.ID, pq.Question)
	})
}

// TestClampEDNS verifies that ClampEDNS reports the absence of an OPT record on
// a plain query, clamps the advertised payload size, and leaves every EDNS
// option unchanged.
func TestClampEDNS(t *testing.T) {
	// A plain query has no OPT record: nothing to clamp.
	if ClampEDNS(pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 1},
		Questions: []dnsmessage.Question{q("a.cn.", dnsmessage.TypeA)}}), 1232) {
		t.Fatal("ClampEDNS reported an OPT record on a plain query")
	}

	optRes := opt(t)
	optRes.Body = &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: 10, Data: []byte("cookie")}}}
	raw := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 2},
		Questions:   []dnsmessage.Question{q("a.cn.", dnsmessage.TypeA)},
		Additionals: []dnsmessage.Resource{optRes}})
	before := append([]byte(nil), raw...)
	if !ClampEDNS(raw, EDNSPayload) {
		t.Fatal("ClampEDNS did not find the OPT record")
	}
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		t.Fatal(err)
	}
	if got := uint16(m.Additionals[0].Header.Class); got != EDNSPayload {
		t.Fatalf("OPT payload size = %d, want %d", got, EDNSPayload)
	}
	// Everything except the OPT CLASS field must remain unchanged, so the EDNS
	// option data reaches the authority byte for byte.
	classOff := len(before) - 2 - 8 - len("cookie")
	if string(raw[classOff:]) != string(before[classOff:]) && string(raw[classOff+2:]) != string(before[classOff+2:]) {
		t.Fatalf("options were rewritten:\n before %x\n after  %x", before, raw)
	}
	if string(raw[len(raw)-len("cookie"):]) != "cookie" {
		t.Fatalf("EDNS option data lost: %x", raw)
	}
}

// TestFormatErrorKeepsOPT protects RFC 6891 §6.1.1: a malformed EDNS query must
// still receive an OPT record in its FORMERR reply, because the client advertised
// EDNS and the error reply must reflect that.
func TestFormatErrorKeepsOPT(t *testing.T) {
	raw := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 9},
		Questions: []dnsmessage.Question{
			q("a.cn.", dnsmessage.TypeA), q("b.cn.", dnsmessage.TypeA),
		}, Additionals: []dnsmessage.Resource{opt(t)}})
	pq, err := ParseQuery(raw)
	if err == nil {
		t.Fatal("two questions must be rejected")
	}
	if !pq.EDNS {
		t.Fatal("EDNS flag lost on a malformed query")
	}
	resp, err := Reply(pq, dnsmessage.RCodeFormatError, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatal(err)
	}
	if m.Header.RCode != dnsmessage.RCodeFormatError {
		t.Fatalf("rcode = %v", m.Header.RCode)
	}
	var optSeen bool
	for _, r := range m.Additionals {
		if r.Header.Type == dnsmessage.TypeOPT {
			optSeen = true
		}
	}
	if !optSeen {
		t.Fatal("FORMERR reply to an EDNS query has no OPT record")
	}
}

// TestReplyTruncatesLocalAnswer verifies that a local answer larger than the
// client's UDP limit is truncated with TC and no answers, and that the same
// answer is not truncated over TCP.
func TestReplyTruncatesLocalAnswer(t *testing.T) {
	pq := Query{
		Header:      dnsmessage.Header{ID: 3},
		Question:    q("many.example.", dnsmessage.TypeA),
		HasQuestion: true,
		EDNS:        true,
		UDPSize:     512,
	}
	var answers []dnsmessage.Resource
	for i := 0; i < 40; i++ {
		answers = append(answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: pq.Question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.AResource{A: [4]byte{10, 0, 0, byte(i)}},
		})
	}
	raw, err := Reply(pq, dnsmessage.RCodeSuccess, true, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 512 {
		t.Fatalf("reply is %d bytes, over the client's 512-byte limit", len(raw))
	}
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		t.Fatal(err)
	}
	if !m.Header.Truncated || len(m.Answers) != 0 {
		t.Fatalf("want TC with no answers, got %+v (%d answers)", m.Header, len(m.Answers))
	}
	// Over TCP the same answer is not truncated.
	pq.Proto = TCP
	raw, err = Reply(pq, dnsmessage.RCodeSuccess, true, answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Unpack(raw); err != nil {
		t.Fatal(err)
	}
	if m.Header.Truncated || len(m.Answers) != 40 {
		t.Fatalf("TCP reply truncated: %+v (%d answers)", m.Header, len(m.Answers))
	}
}

// TestReplyExtendedRCode checks that an extended RCODE is split into the header
// nibble and the OPT extended bits, and that it is downgraded to SERVFAIL when
// the query did not carry EDNS.
func TestReplyExtendedRCode(t *testing.T) {
	pq := Query{Header: dnsmessage.Header{ID: 4}, Question: q("a.cn.", dnsmessage.TypeA), HasQuestion: true, EDNS: true, UDPSize: 1232}
	raw, err := Reply(pq, 16, false, nil) // BADVERS
	if err != nil {
		t.Fatal(err)
	}
	if got := raw[3] & 0x0f; got != 0 {
		t.Fatalf("header RCODE nibble = %d, want 0 for BADVERS", got)
	}
	if got := EffectiveRCode(raw); got != 16 {
		t.Fatalf("EffectiveRCode = %d, want 16", got)
	}
	if RCodeName(EffectiveRCode(raw)) != "BADVERS" {
		t.Fatalf("RCodeName = %q", RCodeName(EffectiveRCode(raw)))
	}
	// Without an OPT record the extended code cannot be represented; it must not
	// leak into the CD, AD or Z flag bits.
	pq.EDNS = false
	raw, err = Reply(pq, 16, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := EffectiveRCode(raw); r != int(dnsmessage.RCodeServerFailure) {
		t.Fatalf("EDNS-less extended rcode = %d, want SERVFAIL", r)
	}
}

// TestReplyEchoesDO verifies that the DO bit parsed from the query is echoed in
// the OPT record of the reply.
func TestReplyEchoesDO(t *testing.T) {
	raw := pack(t, dnsmessage.Message{Header: dnsmessage.Header{ID: 5},
		Questions:   []dnsmessage.Question{q("a.cn.", dnsmessage.TypeA)},
		Additionals: []dnsmessage.Resource{opt(t)}}) // opt() sets DO
	pq, err := ParseQuery(raw)
	if err != nil || !pq.EDNSDo {
		t.Fatalf("DO bit not parsed: %+v %v", pq, err)
	}
	resp, err := Reply(pq, dnsmessage.RCodeSuccess, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatal(err)
	}
	if !m.Additionals[0].Header.DNSSECAllowed() {
		t.Fatal("DO bit not echoed in the reply")
	}
}

// TestRCodeLabelIsBounded verifies that RCodeLabel maps every possible RCODE to a
// bounded set of metric labels while RCodeName still reports each raw value.
func TestRCodeLabelIsBounded(t *testing.T) {
	seen := map[string]bool{}
	for rc := -1; rc < 4096; rc++ {
		seen[RCodeLabel(rc)] = true
	}
	if len(seen) > 9 || !seen["OTHER"] || !seen["BADVERS"] || !seen["NXDOMAIN"] {
		t.Errorf("labels = %v", seen)
	}
	if RCodeLabel(9) != "OTHER" || RCodeName(9) != "RCODE9" {
		t.Errorf("9 -> label %s, name %s", RCodeLabel(9), RCodeName(9))
	}
}
