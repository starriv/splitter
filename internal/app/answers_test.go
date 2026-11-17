package app

import (
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// answerMessage packs one response whose answer section is answers.
func answerMessage(t *testing.T, answers ...dnsmessage.Resource) []byte {
	t.Helper()
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 1, Response: true, RecursionAvailable: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("www.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
		Answers:   answers,
	}
	raw, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func resource(name string, typ dnsmessage.Type, ttl uint32, body dnsmessage.ResourceBody) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   body,
	}
}

// TestRecentAnswers verifies the bounded decoding used by the recent ring and
// the query log: one record per answer, a cap on the record count, a cap on
// each rendered value, and no answers for a message that has none.
func TestRecentAnswers(t *testing.T) {
	t.Run("one A record", func(t *testing.T) {
		resp := answerMessage(t, resource("www.example.", dnsmessage.TypeA, 300, &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}}))
		answers, truncated := recentAnswers(resp)
		if len(answers) != 1 || truncated {
			t.Fatalf("answers = %+v, truncated = %v", answers, truncated)
		}
		a := answers[0]
		if a.Name != "www.example" || a.Type != "A" || a.TTL != 300 || a.Data != "192.0.2.1" {
			t.Errorf("answer = %+v", a)
		}
	})

	t.Run("NODATA", func(t *testing.T) {
		answers, truncated := recentAnswers(answerMessage(t))
		if len(answers) != 0 || truncated {
			t.Fatalf("answers = %+v, truncated = %v", answers, truncated)
		}
	})

	t.Run("many records", func(t *testing.T) {
		var answers []dnsmessage.Resource
		for i := range 12 {
			answers = append(answers, resource("www.example.", dnsmessage.TypeAAAA, 60,
				&dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(i)}}))
		}
		got, truncated := recentAnswers(answerMessage(t, answers...))
		if len(got) != maxRecentAnswers || !truncated {
			t.Fatalf("got %d answers, truncated = %v", len(got), truncated)
		}
		if got[0].Data != "2001:db8::" || got[len(got)-1].Data != "2001:db8::7" {
			t.Errorf("answers = %+v", got)
		}
	})

	t.Run("long TXT data", func(t *testing.T) {
		text := strings.Repeat("a", 255) + strings.Repeat("b", 255)
		resp := answerMessage(t, resource("www.example.", dnsmessage.TypeTXT, 60, &dnsmessage.TXTResource{TXT: []string{text[:255], text[255:]}}))
		answers, truncated := recentAnswers(resp)
		if len(answers) != 1 || !truncated {
			t.Fatalf("answers = %+v, truncated = %v", answers, truncated)
		}
		if want := maxAnswerDataBytes + len("…"); len(answers[0].Data) != want || !strings.HasSuffix(answers[0].Data, "…") {
			t.Errorf("data length = %d, want %d", len(answers[0].Data), want)
		}
	})

	t.Run("CNAME and unknown type", func(t *testing.T) {
		resp := answerMessage(t,
			resource("www.example.", dnsmessage.TypeCNAME, 60, &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("edge.example.")}),
			resource("www.example.", 65280, 60, &dnsmessage.UnknownResource{Type: 65280, Data: []byte{1, 2, 3, 4}}))
		answers, _ := recentAnswers(resp)
		if len(answers) != 2 || answers[0].Data != "edge.example" || answers[1].Data != "4 bytes" {
			t.Errorf("answers = %+v", answers)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if answers, _ := recentAnswers([]byte{1, 2, 3}); answers != nil {
			t.Errorf("malformed response produced %+v", answers)
		}
	})

	t.Run("cut short", func(t *testing.T) {
		resp := answerMessage(t,
			resource("www.example.", dnsmessage.TypeA, 60, &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}}),
			resource("www.example.", dnsmessage.TypeA, 60, &dnsmessage.AResource{A: [4]byte{192, 0, 2, 2}}))
		answers, truncated := recentAnswers(resp[:len(resp)-2])
		if len(answers) != 1 || answers[0].Data != "192.0.2.1" || !truncated {
			t.Errorf("answers = %+v, truncated = %v", answers, truncated)
		}
	})
}
