// OFX statement parsing (§8.6 step c): minimal tag extraction for <STMTTRN>
// blocks. OFX 1.x is SGML (no closing tags on leaf elements), so a full XML
// parser cannot handle it; the fields banks actually emit (TRNTYPE, DTPOSTED,
// TRNAMT, FITID, NAME, MEMO) are flat tags this scanner reads directly.
package imports

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Scarvis/pet-backend/internal/common"
)

// ofxTag returns the text of <TAG> up to the next '<', or "" when absent.
func ofxTag(block, tag string) string {
	re := regexp.MustCompile(`(?i)<` + tag + `>([^<]*)`)
	m := re.FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// ofxDate parses DTPOSTED (YYYYMMDD[HHMMSS[.xxx][tz]]); only the calendar part
// matters because rates and summaries are date-granular (§8.2, §8.7).
func ofxDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return time.Time{}, common.BadRequest(fmt.Sprintf("bad OFX date %q", s))
	}
	t, err := time.Parse("20060102", s[:8])
	if err != nil {
		return time.Time{}, common.Wrap(fmt.Errorf("bad OFX date %q: %w", s, err))
	}
	return t.UTC(), nil
}

// splitTransactions cuts the OFX body into <STMTTRN> blocks. RE2 has no
// lookahead, so split on the opening tag and truncate each part at the
// enclosing </BANKTRANLIST>.
func splitTransactions(text string) []string {
	parts := regexp.MustCompile(`(?i)<STMTTRN>`).Split(text, -1)
	if len(parts) < 2 {
		return nil
	}
	end := regexp.MustCompile(`(?i)</BANKTRANLIST>`)
	var out []string
	for _, p := range parts[1:] {
		if loc := end.FindStringIndex(p); loc != nil {
			p = p[:loc[0]]
		}
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseOFX extracts statement transactions from OFX content. The account-level
// <CURDEF> supplies the currency for rows (upper-cased); a missing CURDEF
// leaves Currency empty and the mapper falls back to the account currency.
// Blocks without TRNAMT or DTPOSTED are rejected — a statement line without
// an amount or a date cannot become a transaction.
func ParseOFX(content []byte) ([]ParsedRow, error) {
	text := string(content)
	currency := strings.ToUpper(ofxTag(text, "CURDEF"))

	blocks := splitTransactions(text)
	if len(blocks) == 0 {
		return nil, common.Wrap(common.BadRequest("no <STMTTRN> blocks found"))
	}
	var out []ParsedRow
	for n, block := range blocks {
		amount := ofxTag(block, "TRNAMT")
		posted := ofxTag(block, "DTPOSTED")
		if amount == "" || posted == "" {
			return nil, common.Wrap(common.BadRequest(fmt.Sprintf("ofx transaction %d: missing TRNAMT or DTPOSTED", n+1)))
		}
		occurred, err := ofxDate(posted)
		if err != nil {
			return nil, common.Wrap(fmt.Errorf("ofx transaction %d: %w", n+1, err))
		}
		desc := ofxTag(block, "MEMO")
		if name := ofxTag(block, "NAME"); name != "" {
			if desc == "" {
				desc = name
			} else {
				desc = name + " — " + desc
			}
		}
		out = append(out, ParsedRow{
			OccurredAt:    occurred,
			AmountDecimal: amount,
			Currency:      currency,
			Description:   desc,
			FITID:         ofxTag(block, "FITID"),
		})
	}
	return out, nil
}
