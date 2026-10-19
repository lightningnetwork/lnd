package dnsclient

import "golang.org/x/net/dns/dnsmessage"

// rcodeText maps a DNS RCode to the same human-readable string lnd
// has historically used for SRV-bootstrap error messages. The
// strings come from the IANA DNS RCODES registry
// (https://www.iana.org/assignments/dns-parameters/dns-parameters.xhtml).
//
// We keep this here rather than using dnsmessage.RCode.String()
// directly because the upstream renders compact mnemonics like
// "NXDOMAIN" / "ServFail", and lnd's existing operator-facing log
// output uses the spelled-out form ("non-existent domain", "server
// failure", etc.). Preserving the wording keeps the upgrade
// invisible to anyone grepping logs for these strings.
var rcodeText = map[dnsmessage.RCode]string{
	dnsmessage.RCodeSuccess:        "no error",
	dnsmessage.RCodeFormatError:    "format error",
	dnsmessage.RCodeServerFailure:  "server failure",
	dnsmessage.RCodeNameError:      "non-existent domain",
	dnsmessage.RCodeNotImplemented: "not implemented",
	dnsmessage.RCodeRefused:        "query refused",

	// The remaining codes are not exported as named constants by
	// dnsmessage but appear in the IANA registry. The values below
	// match RFC 6895 §2.3.
	6:  "name exists when it should not",
	7:  "RR set exists when it should not",
	8:  "RR set that should exist does not",
	9:  "server not authoritative for zone",
	10: "name not contained in zone",
	16: "TSIG signature failure",
	17: "key not recognized",
	18: "signature out of time window",
	19: "bad TKEY mode",
	20: "duplicate key name",
	21: "algorithm not supported",
	22: "bad truncation",
	23: "bad/missing server cookie",
}

// RCodeText returns the human-readable description for a DNS RCode,
// falling back to the dnsmessage default ("Unknown RCode(N)") for
// values not present in the registry table.
func RCodeText(c dnsmessage.RCode) string {
	if s, ok := rcodeText[c]; ok {
		return s
	}
	return c.String()
}
