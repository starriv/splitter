// Package wire implements the DNS message handling required by the splitter:
// parsing the header, question and EDNS metadata of a query, validating that an
// upstream response belongs to a query, and constructing locally generated
// replies.
//
// Forwarded responses are returned byte for byte; only the message ID is
// restored. EDNS options, DNSSEC records and record types that it does not
// inspect therefore remain unchanged. Re-encoding upstream answers is a
// common source of EDNS defects in forwarders; returning the bytes unchanged
// avoids that class of defect.
package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/domain"
)

// Proto is the transport on which a query arrived; upstream exchanges mirror
// it. The zero value is UDP.
type Proto uint8

const (
	// UDP is the DNS-over-UDP transport.
	UDP Proto = iota
	// TCP is the DNS-over-TCP transport.
	TCP
)

// String returns the lowercase name of the transport.
func (p Proto) String() string {
	if p == TCP {
		return "tcp"
	}
	return "udp"
}

// HeaderLen is the fixed DNS header size.
const HeaderLen = 12

// EDNSPayload is the UDP payload size advertised in locally built replies
// (the DNS Flag Day 2020 recommendation).
const EDNSPayload = 1232

var (
	// ErrNoHeader reports a message that is too short to answer.
	ErrNoHeader = errors.New("message shorter than DNS header")
	// ErrFormat reports a message whose header parsed but whose query is
	// malformed; such a query is answered with FORMERR.
	ErrFormat = errors.New("malformed query")
)

// Query holds the parsed fields of an incoming query.
type Query struct {
	Header      dnsmessage.Header
	Question    dnsmessage.Question
	HasQuestion bool
	Name        string // normalized: lowercase, no trailing dot ("" for the root)
	EDNS        bool   // the query carried an OPT record
	EDNSVersion uint8  // EDNS version from the OPT TTL (0 = EDNS0)
	EDNSDo      bool   // the DO bit was set in the OPT TTL
	UDPSize     uint16 // UDP payload size the client advertised in the OPT CLASS
	Proto       Proto  // transport the query arrived on; Reply sizes local answers by it
}

// ParseQuery parses the header, the single question and the OPT record of msg.
//
// The OPT record is located by a direct walk of the wire format rather than by
// the parser, so an EDNS query with a broken question section still yields EDNS
// metadata for its FORMERR reply (RFC 6891 §6.1.1 requires an OPT record in that
// reply).
//
// ParseQuery returns ErrNoHeader when msg is too short to contain a DNS header,
// and an error wrapping ErrFormat when the question section is malformed. In
// the latter case the returned Query contains the fields parsed before the
// failure.
func ParseQuery(msg []byte) (Query, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return Query{}, ErrNoHeader
	}
	q := Query{Header: h}
	if off, ok := findOPT(msg); ok {
		q.EDNS = true
		q.UDPSize = binary.BigEndian.Uint16(msg[off+2:])
		ttl := binary.BigEndian.Uint32(msg[off+4:])
		q.EDNSVersion = uint8(ttl >> 16)
		q.EDNSDo = ttl&ednsDoBit != 0
	}
	qq, err := p.Question()
	if err != nil {
		return q, fmt.Errorf("%w: question: %v", ErrFormat, err)
	}
	q.Question, q.HasQuestion = qq, true
	q.Name = domain.Normalize(qq.Name.String())
	if _, err := p.Question(); err == nil {
		return q, fmt.Errorf("%w: more than one question", ErrFormat)
	} else if !errors.Is(err, dnsmessage.ErrSectionDone) {
		return q, fmt.Errorf("%w: question: %v", ErrFormat, err)
	}
	return q, nil
}

// ednsDoBit is the DNSSEC OK flag in the OPT TTL (RFC 6891 §6.1.4).
const ednsDoBit = 0x00008000

// findOPT returns the offset of the OPT record's TYPE field in the additional
// section, walking the message with its own header counts. The walk operates
// on bytes because it must also work on a message whose question section is
// malformed.
func findOPT(msg []byte) (int, bool) {
	if len(msg) < HeaderLen {
		return 0, false
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	an := int(binary.BigEndian.Uint16(msg[6:]))
	ns := int(binary.BigEndian.Uint16(msg[8:]))
	ar := int(binary.BigEndian.Uint16(msg[10:]))
	off := HeaderLen
	for i := 0; i < qd; i++ {
		var ok bool
		if off, ok = skipName(msg, off); !ok || off+4 > len(msg) {
			return 0, false
		}
		off += 4
	}
	for i := 0; i < an+ns; i++ {
		var ok bool
		if off, ok = skipRR(msg, off); !ok {
			return 0, false
		}
	}
	for i := 0; i < ar; i++ {
		nameEnd, ok := skipName(msg, off)
		if !ok || nameEnd+10 > len(msg) {
			return 0, false
		}
		if dnsmessage.Type(binary.BigEndian.Uint16(msg[nameEnd:])) == dnsmessage.TypeOPT {
			return nameEnd, true
		}
		if off, ok = skipRR(msg, off); !ok {
			return 0, false
		}
	}
	return 0, false
}

// skipName advances past a (possibly compressed) domain name.
func skipName(msg []byte, off int) (int, bool) {
	for off < len(msg) {
		b := msg[off]
		switch {
		case b == 0:
			return off + 1, true
		case b&0xc0 == 0xc0: // compression pointer
			if off+1 >= len(msg) {
				return 0, false
			}
			return off + 2, true
		case b&0xc0 != 0: // reserved label type
			return 0, false
		default:
			off += int(b) + 1
		}
	}
	return 0, false
}

// skipRR advances past one resource record.
func skipRR(msg []byte, off int) (int, bool) {
	off, ok := skipName(msg, off)
	if !ok || off+10 > len(msg) {
		return 0, false
	}
	off += 10 + int(binary.BigEndian.Uint16(msg[off+8:]))
	if off > len(msg) {
		return 0, false
	}
	return off, true
}

// ClampEDNS rewrites the UDP payload size advertised in the query's OPT record
// to at most max. Only the CLASS field changes; every EDNS option is preserved
// byte for byte. It reports whether the message carried an OPT record.
func ClampEDNS(msg []byte, max uint16) bool {
	off, ok := findOPT(msg)
	if !ok || off+4 > len(msg) {
		return false
	}
	if cur := binary.BigEndian.Uint16(msg[off+2:]); cur > max {
		binary.BigEndian.PutUint16(msg[off+2:], max)
	}
	return true
}

// ID returns the message ID.
func ID(msg []byte) uint16 { return binary.BigEndian.Uint16(msg) }

// SetID overwrites the message ID in place.
func SetID(msg []byte, id uint16) { binary.BigEndian.PutUint16(msg, id) }

// RCode returns the 4-bit header RCODE, or -1 for a short message.
func RCode(msg []byte) int {
	if len(msg) < HeaderLen {
		return -1
	}
	return int(msg[3] & 0x0f)
}

// Truncated reports whether the TC bit is set. It returns false for a short
// message.
func Truncated(msg []byte) bool { return len(msg) >= HeaderLen && msg[2]&0x02 != 0 }

// ResponseMatches reports whether resp answers the query with the given ID and
// question. Name comparison is case-insensitive. A response without a question
// is accepted only when it carries an error RCODE, because some servers omit
// the question from FORMERR and REFUSED replies.
func ResponseMatches(resp []byte, id uint16, q dnsmessage.Question) bool {
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil || h.ID != id || !h.Response {
		return false
	}
	rq, err := p.Question()
	if errors.Is(err, dnsmessage.ErrSectionDone) {
		return h.RCode != dnsmessage.RCodeSuccess
	}
	if err != nil {
		return false
	}
	return rq.Type == q.Type && rq.Class == q.Class &&
		bytes.EqualFold(rq.Name.Data[:rq.Name.Length], q.Name.Data[:q.Name.Length])
}

// Reply builds a response to q. answers may be nil (NODATA or an error RCODE).
//
// The 4-bit header RCODE cannot carry values above 15: those are split into the
// header nibble plus the OPT extended bits (RFC 6891 §6.1.3). A reply without an
// OPT record cannot express them at all, so it is downgraded to SERVFAIL rather
// than allowing the packer to place the high bits in the CD, AD or Z flags.
//
// A locally generated answer larger than the size the client accepts on the
// transport is replaced by the response header, question and OPT record with the
// TC bit set, so the client retries over TCP. Forwarded replies never pass
// through Reply; they are returned byte for byte.
func Reply(q Query, rcode dnsmessage.RCode, authoritative bool, answers []dnsmessage.Resource) ([]byte, error) {
	full := rcode
	if full > 15 && !q.EDNS {
		full = dnsmessage.RCodeServerFailure
	}
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 q.Header.ID,
			Response:           true,
			OpCode:             q.Header.OpCode,
			Authoritative:      authoritative,
			RecursionDesired:   q.Header.RecursionDesired,
			RecursionAvailable: true,
			RCode:              full & 0xf,
		},
		Answers: answers,
	}
	if q.HasQuestion {
		m.Questions = []dnsmessage.Question{q.Question}
	}
	if q.EDNS {
		var rh dnsmessage.ResourceHeader
		if err := rh.SetEDNS0(EDNSPayload, full, q.EDNSDo); err != nil {
			return nil, err
		}
		m.Additionals = []dnsmessage.Resource{{Header: rh, Body: &dnsmessage.OPTResource{}}}
	}
	out, err := m.AppendPack(make([]byte, 0, 512))
	if err != nil {
		return nil, err
	}
	if len(out) <= q.udpLimit() {
		return out, nil
	}
	// The answer exceeds the size the client accepts over this transport:
	// drop the answers and set TC so the client retries over TCP.
	m.Answers = nil
	m.Header.Truncated = true
	return m.AppendPack(make([]byte, 0, 512))
}

// udpLimit is the largest reply the client accepts on the transport this query
// arrived on. A UDP client that did not send EDNS is limited to 512 bytes
// (RFC 1035).
func (q Query) udpLimit() int {
	if q.Proto == TCP {
		return 65535
	}
	if !q.EDNS || q.UDPSize < 512 {
		return 512
	}
	if q.UDPSize > EDNSPayload {
		return EDNSPayload // cap the advertised size at EDNSPayload
	}
	return int(q.UDPSize)
}

// EffectiveRCode returns the full RCODE of a message: the 4-bit header RCODE
// combined with the extended bits carried in the OPT TTL, which is what an
// RFC 6891 client computes and therefore what metrics and logs should report.
func EffectiveRCode(msg []byte) int {
	if len(msg) < HeaderLen {
		return -1
	}
	rc := int(msg[3] & 0x0f)
	if off, ok := findOPT(msg); ok && off+5 <= len(msg) {
		rc |= int(msg[off+4]) << 4
	}
	return rc
}

// RCodeName returns the mnemonic used in metrics and logs.
func RCodeName(rc int) string {
	switch rc {
	case 0:
		return "NOERROR"
	case 1:
		return "FORMERR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 4:
		return "NOTIMP"
	case 5:
		return "REFUSED"
	case 16:
		return "BADVERS"
	case -1:
		return "NONE"
	}
	return fmt.Sprintf("RCODE%d", rc)
}

// RCodeLabel is RCodeName for a metric label: every RCODE without a mnemonic
// collapses into "OTHER". The value comes from upstream (12 bits with the
// extended part), so using it raw would let a peer create up to 4096 series.
func RCodeLabel(rc int) string {
	switch rc {
	case 0, 1, 2, 3, 4, 5, 16, -1:
		return RCodeName(rc)
	}
	return "OTHER"
}

// RCodeBadVers is the extended RCODE for an unsupported EDNS version
// (RFC 6891 §6.1.3). It only fits in a reply that carries an OPT record.
const RCodeBadVers dnsmessage.RCode = 16

// EDNSPayloadOf returns the UDP payload size advertised in a message's OPT
// record, or 0 when the message has none.
func EDNSPayloadOf(msg []byte) uint16 {
	off, ok := findOPT(msg)
	if !ok || off+4 > len(msg) {
		return 0
	}
	return binary.BigEndian.Uint16(msg[off+2:])
}
